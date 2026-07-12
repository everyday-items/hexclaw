package engine

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/memory/recall"
)

// BUG-20260703 会话 pre-LLM 阻塞防护（对齐同包 ActiveRecall 的超时+熔断模式）：
// 记忆召回是增强，绝不值得让会话陪慢 embedding 端点等 —— 真机曾出现 26 字回复 95s：
// Embed 继承整请求 ctx（web 适配器 10min）+ 底层仅 ResponseHeaderTimeout=120s，
// 端点排队时把「收到消息 → 调 LLM」拖出上百秒且全程零日志。
const (
	// memoryEmbedTimeout 单次召回向量化预算。缓存命中零网络；未命中正常 <1s，
	// 超预算即掐断软降级 BM25（宁可本轮少语义分，不可拖会话）。
	memoryEmbedTimeout = 2500 * time.Millisecond
	// memEmbedBreakerThreshold 连续失败/超时次数阈值，达到即开闸。
	memEmbedBreakerThreshold = 3
	// memEmbedBreakerCooldown 开闸冷却期：期间跳过向量路径（纯 BM25），到期放行探测。
	memEmbedBreakerCooldown = 60 * time.Second
)

// MemoryEmbedder 是长期记忆召回所需的最小向量化能力（hexagon.VectorEmbedder 的子集：
// 只用到批量 Embed）。窄接口便于注入假实现做单测，且与重量级 vector.Embedder 解耦。
type MemoryEmbedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// 长期记忆注入预算（rune）。沿用 buildCapabilityContext 原 8000 字符安全上限，但把
// 「按文件顺序硬截断」升级为「按三维打分选择性保留」（方案 §G1 重点二 / 修 P1：存储即检索）。
const longTermMemoryBudgetRunes = 8000

// residentBudgetRunes 常驻层（rule/identity/instruction/preference/pinned）预算上限。
// 常驻保证带（方案 §6 D5 / §6bis.D），优先占预算，剩余留给检索事实。
const residentBudgetRunes = 4000

// buildLongTermMemoryBlock 用召回内核（memory/recall）组装长期记忆注入块，
// 替代 FileMemory.LoadContextForRole 的「整包 200 行 / 8000 字符硬截断」dump。
//
// 双层（方案 §4.1）：
//   - 常驻层（rule/identity/instruction/preference/pinned）→ SelectResident 保证带、bounded。
//   - 检索层（fact/context）→ 三维打分排序（query 空时退化按 importance+recency），填充剩余预算。
//
// 关键取舍：桌面默认无 embedding，检索层 minScore=0 → **只排序不硬砍**，避免漏召回归；
// 超预算时按打分丢弃「最不相关」而非任意截尾（修 bug#3b 的真正解，非把上限调大）。
// 返回**未加围栏**的内容；调用方负责 <memory-context> 围栏 + escape + memory=off 门控（行为不变）。
//
// P4 多租户：当前存储未按 user 分桶，故此处不按 userID 过滤（否则共享存储 + 假隔离=安全错觉）；
// 真·按 user 落盘隔离属砍薄版/存储迁移增量。角色隔离沿用 ParseEntriesForRole（global+role 合并）。
func (e *ReActEngine) buildLongTermMemoryBlock(ctx context.Context, role, query string) string {
	if e.fileMem == nil {
		return ""
	}
	parsed := e.fileMem.ParseEntriesForRole(role)
	if len(parsed) == 0 {
		return ""
	}
	now := time.Now()
	all := toRecallEntries(parsed)

	// 常驻层：保证带，bounded。
	resident, _ := recall.SelectResident(all, now, residentBudgetRunes)

	// 检索层：fact/context（当前有效），三维打分排序。
	var facts []recall.Entry
	for _, en := range all {
		if !en.IsResident() && recall.IsCurrentlyValid(en, now) {
			facts = append(facts, en)
		}
	}
	ranked := e.rankFacts(ctx, facts, query, now)

	// 预算分配：常驻先占（已 bounded ≤ residentBudgetRunes），事实填至总预算。
	var b strings.Builder
	used := 0
	var recalledFactIDs []string    // 缺陷F：被真正注入的检索层事实 = 一次召回
	var recalledHits []recall.Entry // U9：**按相关性召回**并注入的条目 → 结构化命中回传前端
	write := func(entries []recall.Entry, trackRecall bool) {
		for _, en := range entries {
			line := "- " + strings.TrimSpace(en.Content)
			cost := len([]rune(line)) + 1
			if used+cost > longTermMemoryBudgetRunes && used > 0 {
				return // 已选高优先在前；超预算停止，最不相关者被丢
			}
			b.WriteString(line + "\n")
			used += cost
			if trackRecall {
				recalledHits = append(recalledHits, en)
				if en.ID != "" {
					recalledFactIDs = append(recalledFactIDs, en.ID)
				}
			}
		}
	}
	write(resident, false) // 常驻恒注入，不计入「按相关性被召回」的频次信号
	write(ranked, true)

	// U9：只把**检索层真命中**记入命中 sink（BUG-20260712-L：「记忆命中」语义=按相关性召回；
	// 常驻层（pinned/rule/preference）是恒注入不是命中——pinned 垃圾对着 1+1 显示命中卡纯属误导）。
	recordMemoryHits(ctx, role, recalledHits)

	// 缺陷F：query 驱动的真召回里被注入的事实 → 自增 HitCount，复活行为 importance/晋升/做梦保护反馈环。
	// 空 query（每轮 dump）不计：那不是「因相关被召回」，避免频次被无意义灌水。best-effort、不阻断。
	if strings.TrimSpace(query) != "" && len(recalledFactIDs) > 0 {
		e.fileMem.BumpHitCount(recalledFactIDs)
	}

	body := strings.TrimRight(b.String(), "\n")
	if body == "" {
		return ""
	}
	return "## 长期记忆\n\n" + body
}

// rankFacts 按三维打分排序检索层（query 空时退化按 importance+recency；零 LLM）。
// 配了 memEmbedder 时检索走 hybrid（0.7 向量 + 0.3 BM25），否则软降级纯 BM25。
func (e *ReActEngine) rankFacts(ctx context.Context, facts []recall.Entry, query string, now time.Time) []recall.Entry {
	if len(facts) == 0 {
		return nil
	}
	// 寒暄门（BUG-20260712-O，对齐知识库侧 shouldAutoInjectKB）：<4 rune 超短输入无检索意图，
	// 检索层直接跳过（常驻层不受影响）。真机取证：「你好」曾把「花生过敏」召回成记忆命中。
	if q := strings.TrimSpace(query); q != "" && len([]rune(q)) < 4 {
		return nil
	}
	if strings.TrimSpace(query) == "" {
		sorted := append([]recall.Entry(nil), facts...)
		sort.SliceStable(sorted, func(i, j int) bool {
			ii, ij := recall.Importance(sorted[i]), recall.Importance(sorted[j])
			if ii != ij {
				return ii > ij
			}
			return recall.Recency(sorted[i], now, recall.DefaultLambdaPerDay) >
				recall.Recency(sorted[j], now, recall.DefaultLambdaPerDay)
		})
		return sorted
	}
	// 熔断开闸期间跳过向量路径（BUG-20260703③）：端点持续慢/坏时不再逐条消息付代价。
	emb := e.memEmbedderIfHealthy()
	src := &memEntrySource{entries: facts, embedder: emb}
	minScore := 0.0
	if emb != nil {
		minScore = e.recallMinScore() // 地板仅在向量真实可用时有语义意义（同 no-embedder 分支）
	}
	r := &recall.CuratedRetriever{
		Source:   src,
		Expander: recall.SynonymExpander{Synonyms: recall.DefaultSynonyms()}, // 重点二 query 改写：救漏召
		Reranker: recall.LexicalReranker{},                                   // 重点二 rerank：短语/覆盖精排
		MinScore: minScore,
		TopK:     len(facts),
		Now:      func() time.Time { return now },
	}
	results, err := r.Retrieve(ctx, "", "", query)
	// 降级重试（BUG-20260712-O②，范围收窄）：地板刻度按 hybrid(0.7·cos) 真机标定
	// （nomic 实测：无关对 ≤0.45、相关对 ≥0.58），**只对向量路真实跑通的轮次有语义**——
	//  · embed 失败/超时 → 纯 BM25 轮次刻度失义 → 不设地板重试一次（软降级不失聪，
	//    零证据剔除仍兜底）；复用 src 的 embed memo，绝不二次打端点（BUG-20260703②）；
	//  · 向量路正常 → 砍空即空。旧「结果为空即放宽」已删：它唯一作用是复活地板下噪音
	//    （真机取证：年假问题把 hybrid 0.380 的花生记忆捞回注入；真低分真命中直接过地板）。
	if r.MinScore > 0 && src.embedAttempted && src.embedErr != nil && (err != nil || len(results) == 0) {
		r.MinScore = 0
		results, err = r.Retrieve(ctx, "", "", query)
	}
	// 熔断记账（BUG-20260703③）：本轮真实尝试过向量化才计成败。
	if src.embedAttempted {
		e.recordMemEmbedOutcome(src.embedErr == nil)
	}
	// BUG-20260712-L：空结果不再兜底原样返回——配合 recall 零证据剔除后，空=真无相关，
	// 把全部 facts 原样注入等于「1+1 也召回大大阿达」（真机取证）。注入是增强，空即是空；
	// err 同理（对相关性一无所知时注入全量=噪音），不阻断聊天只是不注入检索层。
	if err != nil || len(results) == 0 {
		return nil
	}
	out := make([]recall.Entry, len(results))
	for i, res := range results {
		out[i] = res.Entry
	}
	return out
}

// memEmbedderIfHealthy 返回可用的记忆向量化器；熔断开闸期间返回 nil（纯 BM25 降级）。
func (e *ReActEngine) memEmbedderIfHealthy() MemoryEmbedder {
	if e.memEmbedder == nil {
		return nil
	}
	if until := e.memEmbedOpenUntil.Load(); until > 0 && time.Now().UnixNano() < until {
		return nil
	}
	return e.memEmbedder
}

// recordMemEmbedOutcome 熔断记账：连续失败达阈值 → 开闸冷却；任一成功 → 复位。
func (e *ReActEngine) recordMemEmbedOutcome(ok bool) {
	if ok {
		e.memEmbedFailStreak.Store(0)
		e.memEmbedOpenUntil.Store(0)
		return
	}
	if streak := e.memEmbedFailStreak.Add(1); streak >= memEmbedBreakerThreshold {
		e.memEmbedOpenUntil.Store(time.Now().Add(memEmbedBreakerCooldown).UnixNano())
		slog.Warn("[engine] 记忆向量化连续失败，熔断开闸（冷却期内纯 BM25 召回）",
			"streak", streak, "cooldown", memEmbedBreakerCooldown.String())
	}
}

// recallMinScore 返回召回相关性地板（修复 minScore=0 噪音）。
// **仅当配了 embedder 时设地板**（hybrid relevance 才有语义意义）；纯 BM25 稀疏 → 返 0 防漏召。
func (e *ReActEngine) recallMinScore() float64 {
	if e.memEmbedder == nil {
		return 0
	}
	if e.cfg == nil {
		return 0.5 // 测试/无 cfg：用默认地板（真机标定 BUG-20260712-O）
	}
	// 经 RLock 快照读（BUG-20260703 P2-2：与 ReloadFileMemoryConfig 的热更新写互斥）
	return e.ActiveFileMemoryConfig().RecallMinScore // DefaultConfig 设 0.3；用户可调/置 0 关
}

// memEntrySource 是基于内存条目切片的 recall.CandidateSource。
// 配了 embedder 时附带向量分（激活 hybrid），否则降级纯 BM25（复用 LexicalSim）。
// embed memo（BUG-20260703②）：一轮 rankFacts 内 Retrieve 可能被调两次（地板 fallback），
// 同 query 同批文本的向量化结果/结论必须复用，绝不二次打端点。
type memEntrySource struct {
	entries  []recall.Entry
	embedder MemoryEmbedder // 可为 nil → 纯 BM25 降级

	embedAttempted bool  // 本轮已真实尝试过向量化（含失败）
	embedErr       error // 尝试结果（nil=成功）；供调用方做熔断记账
	memoQVec       []float32
	memoContent    [][]float32
}

func (s *memEntrySource) Candidates(ctx context.Context, _, _, query string, _ int) ([]recall.Candidate, error) {
	// 向量路径（可选）：一次性 batch embed [query, ...contents]，逐条 cosine 填 VectorScore。
	// CachedEmbedder 按文本缓存：条目正文稳定→命中缓存，每轮仅新 query 真打一次 API。
	// 任一步失败 → 不填向量分，relevance() 自动软降级纯 BM25（注入是增强，绝不阻断）。
	if s.embedder != nil && !s.embedAttempted && strings.TrimSpace(query) != "" && len(s.entries) > 0 {
		s.embedAttempted = true
		texts := make([]string, 0, len(s.entries)+1)
		texts = append(texts, query)
		for _, e := range s.entries {
			texts = append(texts, e.Content)
		}
		// 预算闸（BUG-20260703①）：不继承整请求 ctx 的漫长余量——超预算掐断软降级，
		// 并显式留痕（此前失败被静默吞掉，真机 110s 阻塞全程零日志无从定位）。
		ectx, cancel := context.WithTimeout(ctx, memoryEmbedTimeout)
		start := time.Now()
		vecs, err := s.embedder.Embed(ectx, texts)
		cancel()
		switch {
		case err != nil:
			s.embedErr = err
			slog.Warn("[engine] 记忆召回向量化失败，软降级 BM25",
				"error", err, "elapsed", time.Since(start).String(), "texts", len(texts))
		case len(vecs) != len(texts):
			s.embedErr = context.Canceled // 形状不符视为失败（不重试）
			slog.Warn("[engine] 记忆召回向量化返回形状不符，软降级 BM25",
				"want", len(texts), "got", len(vecs))
		default:
			s.memoQVec, s.memoContent = vecs[0], vecs[1:]
		}
	}

	out := make([]recall.Candidate, 0, len(s.entries))
	for i, e := range s.entries {
		c := recall.Candidate{
			Entry:     e,
			BM25Score: recall.LexicalSim(query, e.Content),
		}
		if len(s.memoQVec) > 0 && s.memoContent != nil {
			c.VectorScore = recall.Cosine(s.memoQVec, s.memoContent[i])
			c.HasVector = true
		}
		out = append(out, c)
	}
	return out, nil
}

// toRecallEntries 把 FileMemory 解析条目映射为 recall.Entry。
// 复用 memory.ToRecallEntry（单一映射器，全字段）——修缺陷A：旧本地映射丢 Pinned/ValidTo/Subject →
// 被取代的失效旧值再注入(矛盾)、置顶 fact 失去常驻保证。
func toRecallEntries(parsed []memory.MemoryEntry) []recall.Entry {
	out := make([]recall.Entry, 0, len(parsed))
	for _, e := range parsed {
		out = append(out, memory.ToRecallEntry(e))
	}
	return out
}
