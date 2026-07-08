// select.go 实现 v0.4.x Skill 闭环 Phase 1+2：
//
//   - Phase 1 渐进式加载 / Top-K：避免把 100 个 Skill 全塞进 LLM context
//   - Phase 2 条件激活：SkillMeta 携带 when/not_when/preferred_mode 自动过滤
//
// 设计取舍：所有打分逻辑在 registry 之外（select.go），registry 保持极简。
// 评分基于 trigger/tag/name 关键词包含度 + 描述子串匹配，可解释、零依赖。
// 后续可换 BM25 / 向量召回，但当前规模（<10K Skill）不必要。
package skill

import (
	"sort"
	"strings"
)

// Activation 条件激活上下文。
//
// 由调用方（chat 引擎）在每次请求开始时构造，传入 SelectByContext 让 registry
// 过滤出"在当前上下文下被激活"的 Skill 子集。
type Activation struct {
	// Mode 当前 Agent 模式（"react"/"tot"/"reflection"...），匹配 frontmatter when/not_when。
	Mode string
	// Subject 当前学科（"math"/"chinese"/"english"），匹配 frontmatter when.subject。
	Subject string
	// Grade 学段（"小学"/"初中"/"高中"），匹配 frontmatter when.grade。
	Grade string
	// Custom 任意业务标签（如 "k12"/"tutor-mode"），用于灵活扩展。
	Custom map[string]string
}

// TrustLevel 表示 Skill 的可信级别。值越大越可信。
//
// v0.4.0 F10 安全底线：
//   - LLM 暴露给终端用户前可按 SelectByMinTrust 过滤掉 Untrusted Skill
//   - patch_skill / create_skill 写到 .pending 后审核者可用 Trust 决定是否批准
//
// Trust 不由 LLM 自填，而由 loader 推导：
//   - 内置代码（skill/builtin/*）→ TrustBuiltin
//   - frontmatter 含 signature 字段且校验通过 → TrustSigned
//   - 在用户主目录 / 项目目录加载且无签名 → TrustLocal
//   - 通过网络下载 / 第三方源 / 签名校验失败 → TrustUntrusted
type TrustLevel int

const (
	// TrustUntrusted 第三方未签名 / 签名失败 / 来源不明。建议默认隐藏给 LLM。
	TrustUntrusted TrustLevel = iota
	// TrustLocal 用户本地加载、未签名。可见但需用户审批写入。
	TrustLocal
	// TrustSigned 已通过 signature 字段校验。
	TrustSigned
	// TrustBuiltin 内置 Skill（编译进二进制），最高信任。
	TrustBuiltin
)

// String 返回易读的 trust 名（用于日志和调试）。
func (t TrustLevel) String() string {
	switch t {
	case TrustBuiltin:
		return "builtin"
	case TrustSigned:
		return "signed"
	case TrustLocal:
		return "local"
	default:
		return "untrusted"
	}
}

// TrustProvider 可选接口：实现该接口的 Skill 主动声明自己的信任级别。
// 不实现的 Skill 默认视作 TrustLocal（在 select.go 的过滤逻辑里）。
type TrustProvider interface {
	TrustLevel() TrustLevel
}

// SkillTrust 返回 Skill 的信任级别（不实现 TrustProvider 时默认 TrustLocal）。
func SkillTrust(s Skill) TrustLevel {
	if tp, ok := s.(TrustProvider); ok {
		return tp.TrustLevel()
	}
	return TrustLocal
}

// ContentLoader 可选接口：实现该接口的 Skill 暴露完整 Prompt / Markdown 正文内容，
// 供 Tier 3 渐进披露（例如 skill_view 工具）按需加载。
//
// 与 Skill.Execute 的区别：ContentLoader.LoadContent 必须无副作用，可被引擎在用户
// 仅"查看 Skill 详情"的场景下安全调用。MarkdownSkill 实现了该接口（返回懒加载的
// 完整 Markdown 内容）；副作用型 Skill（spawn / orchestrate / web search 等）应
// 不实现，让 Tier 3 视图退化为元信息 + Description。
type ContentLoader interface {
	LoadContent() (string, error)
}

// MetaProvider 可选接口：实现该接口的 Skill 暴露 SkillMeta，供条件激活与排序使用。
//
// 不要求所有 Skill 都实现（内置 Skill 没有 frontmatter）。未实现者按 Name+Description
// 兜底参与排序，并默认通过条件激活检查。
type MetaProvider interface {
	SkillMeta() SkillMetaInfo
}

// SkillMetaInfo 条件激活和排序需要的元信息子集（与 marketplace.SkillMeta 对齐）。
//
// 单独抽出此 struct 是因为 skill/marketplace 依赖 skill 包，反向引用会循环。
// MarkdownSkill 在 marketplace 包中实现 SkillMeta() 返回此结构即可。
type SkillMetaInfo struct {
	Name          string
	Description   string
	Triggers      []string
	Tags          []string
	When          []string // 形如 "subject=math"、"mode=tot"、"grade=小学"，AND 关系
	NotWhen       []string // AND 关系，匹配任一即排除
	PreferredMode string   // 例如 "tot"、"plan-execute"，由 chat 引擎读取后路由

	// v0.4.0 F5: 工具集依赖。Tools 是硬依赖（缺失则 Skill 跳过）；Requires 是
	// 抽象能力标签（用于上层环境画像匹配，不强制为可执行工具名）。
	Tools    []string
	Requires []string
}

// SelectByQuery 按 query 关键词召回 Top-K Skill。
//
// 评分规则（高到低）：
//  1. 名称完全匹配     +100
//  2. trigger 包含     +30 / 个
//  3. tag 包含         +10 / 个
//  4. 描述子串包含     +5  / 命中
//
// 同分按注册顺序稳定输出。k<=0 时不限制返回数。
func (r *DefaultRegistry) SelectByQuery(query string, k int) []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.selectLocked(query, nil, k)
}

// SelectByContext 在 SelectByQuery 基础上叠加条件激活过滤（when/not_when）。
func (r *DefaultRegistry) SelectByContext(query string, act Activation, k int) []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.selectLocked(query, &act, k)
}

func (r *DefaultRegistry) selectLocked(query string, act *Activation, k int) []Skill {
	q := strings.ToLower(strings.TrimSpace(query))

	type scored struct {
		idx   int // 注册顺序，用于 stable sort
		score int
		skill Skill
	}
	var hits []scored

	for i, name := range r.order {
		s := r.skills[name]
		if !r.enabled[name] {
			continue
		}
		// 条件激活过滤
		if act != nil && !MatchActivation(s, *act) {
			continue
		}
		score := scoreSkill(s, q)
		if score <= 0 && q != "" {
			continue // 有 query 时分数为 0 不返回
		}
		hits = append(hits, scored{idx: i, score: score, skill: s})
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].idx < hits[j].idx
	})

	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}

	out := make([]Skill, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.skill)
	}
	return out
}

func scoreSkill(s Skill, qLower string) int {
	if qLower == "" {
		return 1 // 无 query：所有可见 Skill 等分参与
	}
	score := 0
	name := strings.ToLower(s.Name())
	if name == qLower {
		score += 100
	} else if strings.Contains(name, qLower) {
		score += 20
	}

	_, hasMeta := s.(MetaProvider)
	if hasMeta {
		meta := s.(MetaProvider).SkillMeta()
		for _, t := range meta.Triggers {
			if strings.Contains(qLower, strings.ToLower(t)) {
				score += 30
			}
		}
		for _, tag := range meta.Tags {
			if strings.Contains(qLower, strings.ToLower(tag)) {
				score += 10
			}
		}
	}

	desc := strings.ToLower(s.Description())
	if desc != "" && strings.Contains(desc, qLower) {
		score += 5
	}

	// 内置 Skill（未实现 MetaProvider）保留最低分参与召回，避免被 query 过滤
	// —— 它们没有 frontmatter 声明 trigger / tag，但应永远可被 LLM 调用。
	if !hasMeta && score == 0 {
		score = 1
	}
	return score
}

// MatchActivation 检查 Skill 是否在给定上下文下被激活。
//
//   - 不实现 MetaProvider 的 Skill 默认通过（内置 Skill 永远可用）
//   - When 子句 AND 语义：所有 when 必须满足
//   - NotWhen 任一满足即排除
func MatchActivation(s Skill, act Activation) bool {
	mp, ok := s.(MetaProvider)
	if !ok {
		return true
	}
	meta := mp.SkillMeta()
	for _, w := range meta.When {
		if !checkClause(w, act) {
			return false
		}
	}
	for _, nw := range meta.NotWhen {
		if checkClause(nw, act) {
			return false
		}
	}
	return true
}

// checkClause 解析 "key=value" 子句并匹配 Activation。
//
// 支持的 key：subject / grade / mode / custom.<key>
// value 大小写不敏感，"*" 表示通配（任意非空值都匹配）。
func checkClause(clause string, act Activation) bool {
	parts := strings.SplitN(clause, "=", 2)
	if len(parts) != 2 {
		return false
	}
	key := strings.TrimSpace(strings.ToLower(parts[0]))
	want := strings.TrimSpace(strings.ToLower(parts[1]))

	var got string
	switch {
	case key == "subject":
		got = strings.ToLower(act.Subject)
	case key == "grade":
		got = strings.ToLower(act.Grade)
	case key == "mode":
		got = strings.ToLower(act.Mode)
	case strings.HasPrefix(key, "custom."):
		got = strings.ToLower(act.Custom[strings.TrimPrefix(key, "custom.")])
	default:
		return false
	}

	if want == "*" {
		return got != ""
	}
	return got == want
}

// PreferredMode 取得 Skill 的 frontmatter 偏好模式（用于 Agent 路由覆盖）。
// 返回 "" 表示该 Skill 无声明，不影响默认路由。
func PreferredMode(s Skill) string {
	if mp, ok := s.(MetaProvider); ok {
		return strings.TrimSpace(mp.SkillMeta().PreferredMode)
	}
	return ""
}

// MissingDependencies 返回 Skill 在当前环境下缺失的硬依赖（Tools）。
//
// 调用方传入当前可用的工具/Skill 名集合（通常是 registry.All() 的名字 + 注册到 LLM
// 的工具名），本函数返回 frontmatter 声明的 Tools 中不在该集合的元素列表。
//
// 用法：
//
//	available := skill.AvailableSet(registry.All(), extraTools...)
//	if missing := skill.MissingDependencies(s, available); len(missing) > 0 {
//	    log.Warn("skill missing tools", "skill", s.Name(), "missing", missing)
//	    continue
//	}
//
// 不实现 MetaProvider 的 Skill（内置 Skill）永远视作无依赖，返回 nil。
func MissingDependencies(s Skill, available map[string]bool) []string {
	mp, ok := s.(MetaProvider)
	if !ok {
		return nil
	}
	tools := mp.SkillMeta().Tools
	if len(tools) == 0 {
		return nil
	}
	var missing []string
	for _, t := range tools {
		name := strings.TrimSpace(t)
		if name == "" {
			continue
		}
		if !available[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// SelectByMinTrust 在 SelectByContext 基础上过滤掉信任级别低于 minTrust 的 Skill。
//
// 用法：Agent 暴露给终端用户时传 minTrust=TrustSigned，可同时收紧 Top-K
// 召回 + 信任过滤。query="" + 零 Activation 时退化为"按信任级别拉全表"。
func (r *DefaultRegistry) SelectByMinTrust(query string, act Activation, minTrust TrustLevel, k int) []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	candidates := r.selectLocked(query, &act, k*2) // 多取一些以便信任过滤后仍有 K 个
	out := make([]Skill, 0, k)
	for _, s := range candidates {
		if SkillTrust(s) >= minTrust {
			out = append(out, s)
			if k > 0 && len(out) >= k {
				break
			}
		}
	}
	return out
}

// AvailableSet 把已注册的 Skill 列表 + 额外工具名转换成 map，便于多次调用
// MissingDependencies 时复用。
func AvailableSet(skills []Skill, extraTools ...string) map[string]bool {
	set := make(map[string]bool, len(skills)+len(extraTools))
	for _, s := range skills {
		if s == nil {
			continue
		}
		set[s.Name()] = true
	}
	for _, t := range extraTools {
		t = strings.TrimSpace(t)
		if t != "" {
			set[t] = true
		}
	}
	return set
}
