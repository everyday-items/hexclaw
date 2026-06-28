// Package recall 实现 hexclaw 记忆系统重构方案
// (.claude/记忆系统重构-对标OpenClaw与Hermes-架构方案.md) 的命脉：**召回准确性**。
//
// 它提供统一记忆模型 Entry 与一组**确定性、零 LLM**的算法 —— 三维打分、
// hybrid 检索、去重取代、常驻挑选 —— 由 recall/eval 子包度量。本包刻意保持
// 纯净（不依赖 memory / engine / 存储），以便独立测试并作为引擎接线的内核。
//
// 设计映射（方案 → 代码）：
//
//	§4.5  统一 MemoryEntry      → recall.Entry（避开 memory.MemoryEntry 命名碰撞，迁移期并存）
//	§4.6  三维打分              → Importance / Recency / Score（确定性，零 LLM）
//	§6bis+ 行为反馈环           → Importance = typePrior + saliencePrior + 召回频次自校正
//	§7-核心 重点二 召得准        → CuratedRetriever（query 改写→hybrid→三维→rerank→minScore→top-K）
//	§7-核心 重点一 记得净        → DedupUpsert（抽取后查重叠 insert/update/discard + 时序取代）
//	§6/§6bis.D 常驻保证带        → SelectResident（T1 冻结挑选，bounded）
//	G3 时效有效窗口             → ValidFrom/ValidTo/Supersedes + IsCurrentlyValid
//	G1 eval harness            → recall/eval
package recall

import (
	"math"
	"strings"
	"time"
)

// Tier 记忆分层（OS 类比，方案 §4.1）。由 Type/Pinned 派生，不手填。
type Tier string

const (
	// TierResident T1 常驻策展（Core/RAM）：会话起冻结、保证注入。
	TierResident Tier = "resident"
	// TierRetrieved T2 按需检索事实（Archival）：query 触发、三维打分 top-K。
	TierRetrieved Tier = "retrieved"
)

// Type 记忆类型（方案 §4.5）。决定 typePrior 与 Tier 派生。
type Type string

const (
	TypeRule        Type = "rule"        // 硬规则（薄版 standing 迁入）：常驻保证带
	TypeIdentity    Type = "identity"    // 用户身份
	TypeInstruction Type = "instruction" // 长期指令
	TypePreference  Type = "preference"  // 偏好
	TypeFact        Type = "fact"        // 离散事实：检索层
	TypeContext     Type = "context"     // 项目/会话上下文事实：检索层
)

// Source 记忆来源（最小 provenance；反思严格机械化，不得改写 manual 条目，方案 §7-核心 修正2）。
type Source string

const (
	SourceManual      Source = "manual"
	SourceChatExtract Source = "chat_extract"
	SourceAIManaged   Source = "ai_managed"
	SourceReflection  Source = "reflection"
)

// Entry 是统一记忆模型（方案 §4.5）。承载三维打分与 G3 时效所需的全部字段。
//
// 与 memory.MemoryEntry（文件存储层条目）刻意分离：本类型是**召回/打分内核**的
// 一等模型，迁移期并存；待砍薄版 + 引擎接线落地后由它统一对外契约。
type Entry struct {
	ID      string
	UserID  string // R9 多租户键（修 P4 串场）；空串视为单租户默认桶
	Role    string // 角色隔离；rule/identity 落 _global
	Type    Type
	Subject string // 该事实陈述的主语/属性（如「居住地」「技术栈」），G3 时序取代的键；可空
	Pinned  bool   // 逃生口：强制常驻（薄版 standing 的唯一保留语义）
	Content string

	Embedding   []float32 // 进向量库供 T2 检索；可空（降级纯 BM25）
	RecallCount int       // 行为 importance：被召回次数，自校正（替代 LLM 静态分）
	Confidence  float32   // 抽取置信度 [0,1]；<=0 视为中性

	AccessedAt time.Time // recency 衰减锚点（被召回时刷新）
	CreatedAt  time.Time
	UpdatedAt  time.Time

	// G3 时效（Zep/Graphiti 式，方案 §7ter）：事实存「有效窗口」，矛盾=取代非覆盖。
	ValidFrom  time.Time
	ValidTo    *time.Time // nil=至今有效；被 supersede 时填旧条失效时刻
	Supersedes string     // 取代的旧条目 ID（留史；召回默认只取 currently-valid）
}

// DerivedTier 据 Type/Pinned 派生分层（方案 §4.5「系统据 Type 派生」）。
func (e Entry) DerivedTier() Tier {
	if e.Pinned {
		return TierResident
	}
	switch e.Type {
	case TypeRule, TypeIdentity, TypeInstruction, TypePreference:
		return TierResident
	default: // fact / context
		return TierRetrieved
	}
}

// IsResident 报告该条是否属于常驻层（保证每轮注入）。
func (e Entry) IsResident() bool { return e.DerivedTier() == TierResident }

// ── 三维打分（确定性，零 LLM；方案 §4.6 / §7bis） ──────────────────────────

// Weights 三维打分权重（方案 §4.6，默认 w=1，可经 eval 调）。
type Weights struct {
	Recency    float64
	Importance float64
	Relevance  float64
}

// DefaultWeights 返回方案默认权重（GenAgents：w=1）。
func DefaultWeights() Weights { return Weights{Recency: 1, Importance: 1, Relevance: 1} }

// DefaultLambdaPerDay 是 recency 指数衰减率（按天）：半衰期 30 天 → 30 天后 recency=0.5。
var DefaultLambdaPerDay = math.Ln2 / 30

const (
	recallAlpha = 0.1 // 每次被召回对 importance 的边际加成
	recallCap   = 0.5 // 召回频次加成封顶（防高频条目无限膨胀）
	saliMax     = 0.5 // saliencePrior 上限
	saliPerHint = 0.15
	saliHintMax = 0.3
)

// typePrior 类型先验（方案 §6bis+ / §7bis）：rule/identity 1.0 · instruction 0.8 · preference 0.6 · fact/context 0.4。
func typePrior(t Type) float64 {
	switch t {
	case TypeRule, TypeIdentity:
		return 1.0
	case TypeInstruction:
		return 0.8
	case TypePreference:
		return 0.6
	default: // fact / context / 未知
		return 0.4
	}
}

// salienceHints 暗示词（中/英/维），命中给 importance 冷启动加成（方案 §7-核心 修正1，
// 解 RecallCount=0 冷启动：重要的新事实第一次就够分召回，不必等被用几次）。
var salienceHints = []string{
	// 中文
	"过敏", "重要", "务必", "一定要", "总是", "永远", "别忘", "不要忘", "千万", "切记", "请记住",
	// 英文
	"allergic", "important", "always", "never", "must", "critical", "remember", "deadline",
	// 维语（音译/常用）
	"مۇھىم", "ھەمىشە",
}

// SaliencePrior 轻量显著性加成 [0, saliMax]：暗示词命中 + 抽取置信度。确定性，零 LLM。
func SaliencePrior(content string, confidence float32) float64 {
	lc := strings.ToLower(content)
	var hintBonus float64
	for _, h := range salienceHints {
		if strings.Contains(lc, strings.ToLower(h)) {
			hintBonus += saliPerHint
			if hintBonus >= saliHintMax {
				hintBonus = saliHintMax
				break
			}
		}
	}
	c := float64(confidence)
	if c <= 0 {
		c = 0.5 // 缺省中性
	}
	bonus := hintBonus + 0.2*c
	if bonus > saliMax {
		bonus = saliMax
	}
	return bonus
}

// Importance = typePrior + saliencePrior + min(cap, α·RecallCount)（方案 §7bis）。
// 确定性 + 自校正、零 LLM 依赖（对齐 OpenClaw recallCount 真实做法）。
func Importance(e Entry) float64 {
	imp := typePrior(e.Type) + SaliencePrior(e.Content, e.Confidence)
	rc := recallAlpha * float64(e.RecallCount)
	if rc > recallCap {
		rc = recallCap
	}
	return imp + rc
}

// Recency = exp(-λ·Δt_days)（方案 §4.6 指数衰减）。"重要但旧"靠 importance/relevance 不被埋。
// 锚点优先 AccessedAt，回退 UpdatedAt/CreatedAt；锚点零值或未来 → 视为最新(1.0)。
func Recency(e Entry, now time.Time, lambdaPerDay float64) float64 {
	anchor := e.AccessedAt
	if anchor.IsZero() {
		anchor = e.UpdatedAt
	}
	if anchor.IsZero() {
		anchor = e.CreatedAt
	}
	if anchor.IsZero() || !now.After(anchor) {
		return 1.0
	}
	days := now.Sub(anchor).Hours() / 24
	if lambdaPerDay <= 0 {
		lambdaPerDay = DefaultLambdaPerDay
	}
	return math.Exp(-lambdaPerDay * days)
}

// Score = w_r·recency + w_i·importance + w_v·relevance（方案 §4.6）。
// relevance 由检索层算好（hybrid 归一 [0,1]）传入，使本函数纯净可测。
func Score(e Entry, relevance float64, now time.Time, w Weights, lambdaPerDay float64) float64 {
	return w.Recency*Recency(e, now, lambdaPerDay) +
		w.Importance*Importance(e) +
		w.Relevance*relevance
}

// IsCurrentlyValid 召回默认只取「当前有效」条目（G3）：
// ValidFrom 在未来 → 尚未生效；ValidTo 已过（被 supersede）→ 失效。
func IsCurrentlyValid(e Entry, now time.Time) bool {
	if !e.ValidFrom.IsZero() && now.Before(e.ValidFrom) {
		return false
	}
	if e.ValidTo != nil && !e.ValidTo.IsZero() && !now.Before(*e.ValidTo) {
		return false
	}
	return true
}

// sameUser 判断两条记忆是否同一租户桶（空 UserID 归默认桶，方案 §4.9）。
func sameUser(a, b string) bool { return a == b }
