package engine

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/memory/recall"
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
	var recalledFactIDs []string // 缺陷F：被真正注入的检索层事实 = 一次召回
	write := func(entries []recall.Entry, trackRecall bool) {
		for _, en := range entries {
			line := "- " + strings.TrimSpace(en.Content)
			cost := len([]rune(line)) + 1
			if used+cost > longTermMemoryBudgetRunes && used > 0 {
				return // 已选高优先在前；超预算停止，最不相关者被丢
			}
			b.WriteString(line + "\n")
			used += cost
			if trackRecall && en.ID != "" {
				recalledFactIDs = append(recalledFactIDs, en.ID)
			}
		}
	}
	write(resident, false) // 常驻恒注入，不计入「按相关性被召回」的频次信号
	write(ranked, true)

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
	r := &recall.CuratedRetriever{
		Source:   memEntrySource{entries: facts, embedder: e.memEmbedder},
		Expander: recall.SynonymExpander{Synonyms: recall.DefaultSynonyms()}, // 重点二 query 改写：救漏召
		Reranker: recall.LexicalReranker{},                                   // 重点二 rerank：短语/覆盖精排
		MinScore: e.recallMinScore(),                                         // 相关性地板：仅 embedding 在时设地板砍噪音，纯 BM25=0 防漏召
		TopK:     len(facts),
		Now:      func() time.Time { return now },
	}
	results, err := r.Retrieve(ctx, "", "", query)
	// 相关性地板绝不砍到空（修 S2 真机回归：「花生酱」query 因地板把唯一相关「花生过敏」也砍掉致空召回）。
	// best practice：地板只用来在「有更相关备选时」滤噪音；若砍到空 → 退回不设地板，宁注入次相关也不漏召到空。
	if r.MinScore > 0 && (err != nil || len(results) == 0) {
		r.MinScore = 0
		results, err = r.Retrieve(ctx, "", "", query)
	}
	if err != nil || len(results) == 0 {
		return facts // 失败兜底：原样返回（注入是增强，绝不阻断）
	}
	out := make([]recall.Entry, len(results))
	for i, res := range results {
		out[i] = res.Entry
	}
	return out
}

// recallMinScore 返回召回相关性地板（修复 minScore=0 噪音）。
// **仅当配了 embedder 时设地板**（hybrid relevance 才有语义意义）；纯 BM25 稀疏 → 返 0 防漏召。
func (e *ReActEngine) recallMinScore() float64 {
	if e.memEmbedder == nil {
		return 0
	}
	if e.cfg == nil {
		return 0.3 // 测试/无 cfg：用默认地板
	}
	return e.cfg.FileMemory.RecallMinScore // DefaultConfig 设 0.3；用户可调/置 0 关
}

// memEntrySource 是基于内存条目切片的 recall.CandidateSource。
// 配了 embedder 时附带向量分（激活 hybrid），否则降级纯 BM25（复用 LexicalSim）。
type memEntrySource struct {
	entries  []recall.Entry
	embedder MemoryEmbedder // 可为 nil → 纯 BM25 降级
}

func (s memEntrySource) Candidates(ctx context.Context, _, _, query string, _ int) ([]recall.Candidate, error) {
	// 向量路径（可选）：一次性 batch embed [query, ...contents]，逐条 cosine 填 VectorScore。
	// CachedEmbedder 按文本缓存：条目正文稳定→命中缓存，每轮仅新 query 真打一次 API。
	// 任一步失败 → 不填向量分，relevance() 自动软降级纯 BM25（注入是增强，绝不阻断）。
	var qVec []float32
	var contentVecs [][]float32
	if s.embedder != nil && strings.TrimSpace(query) != "" && len(s.entries) > 0 {
		texts := make([]string, 0, len(s.entries)+1)
		texts = append(texts, query)
		for _, e := range s.entries {
			texts = append(texts, e.Content)
		}
		if vecs, err := s.embedder.Embed(ctx, texts); err == nil && len(vecs) == len(texts) {
			qVec, contentVecs = vecs[0], vecs[1:]
		}
	}

	out := make([]recall.Candidate, 0, len(s.entries))
	for i, e := range s.entries {
		c := recall.Candidate{
			Entry:     e,
			BM25Score: recall.LexicalSim(query, e.Content),
		}
		if len(qVec) > 0 && contentVecs != nil {
			c.VectorScore = recall.Cosine(qVec, contentVecs[i])
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
