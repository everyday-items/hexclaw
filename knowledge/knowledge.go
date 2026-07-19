// Package knowledge 提供个人知识库管理
//
// 分层架构 (CQRS — Command Query Responsibility Segregation):
//
//	┌─────────────────────────────────────────────────────┐
//	│  Application Layer — Manager                        │
//	│  业务编排：分块→嵌入→写入→检索→评分→格式化           │
//	├──────────────────────┬──────────────────────────────┤
//	│  Command (写路径)     │  Query (读路径)              │
//	│  DocumentRepository   │  ChunkSearcher              │
//	│  文档+Chunk CRUD     │  向量搜索 / FTS5 关键词搜索   │
//	├──────────────────────┴──────────────────────────────┤
//	│  Infrastructure — SQLite (kb_documents, kb_chunks)   │
//	└─────────────────────────────────────────────────────┘
//
// 外部依赖（hexagon / ai-core）:
//   - 向量嵌入：hexagon.VectorEmbedder (ai-core 接口, hexagon embedder 实现)
//   - 文本分块：hexagon.Splitter   (hexagon 接口 + RecursiveSplitter 实现)
package knowledge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexagon"
	hrag "github.com/hexagon-codes/hexagon/rag"
	ragquery "github.com/hexagon-codes/hexagon/rag/query"
	"github.com/hexagon-codes/hexagon/rag/reranker"
	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// ─── Domain Model ───────────────────────────────────────

// Document 文档
type Document struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Content      string    `json:"content,omitempty"`
	Source       string    `json:"source"`
	ChunkCount   int       `json:"chunk_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
	Status       string    `json:"status,omitempty"`        // processing / indexed / failed
	ErrorMessage string    `json:"error_message,omitempty"` // 失败原因
	SourceType   string    `json:"source_type,omitempty"`   // manual / upload / url / file / agent
}

// Chunk 文档片段
type Chunk struct {
	ID         string    `json:"id"`
	DocID      string    `json:"doc_id"`
	DocTitle   string    `json:"doc_title"`
	Source     string    `json:"source"`
	SourceType string    `json:"source_type,omitempty"` // 继承自所属文档（manual/upload/url/file/agent），供元数据过滤与展示
	ChunkCount int       `json:"chunk_count"`
	Content    string    `json:"content"`
	Index      int       `json:"index"`
	Embedding  []float32 `json:"-"`
	Score      float64   `json:"score"`
	CreatedAt  time.Time `json:"created_at"`
}

// SearchHit 结构化知识库搜索结果（对外暴露）
type SearchHit struct {
	DocID      string         `json:"doc_id"`
	DocTitle   string         `json:"doc_title"`
	Source     string         `json:"source,omitempty"`
	ChunkID    string         `json:"chunk_id"`
	ChunkIndex int            `json:"chunk_index"`
	ChunkCount int            `json:"chunk_count"`
	Content    string         `json:"content"`
	Score      float64        `json:"score"`
	CreatedAt  time.Time      `json:"created_at,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// SearchResult 单条搜索结果（内部使用）
type SearchResult struct {
	Chunk       *Chunk
	VectorScore float64 // 向量余弦相似度 (0-1)
	TextScore   float64 // BM25 关键词匹配分数 (0-1)
}

// Filter 检索元数据过滤条件（SOTA 召回缺口补齐）。
//
// 关键约束：过滤在「打分 / topK 截断之前」下推到存储层（SQL WHERE），
// 而不是在应用层对已截断的 topK 做后置过滤——否则匹配文档可能因排在候选池
// 之外而被静默漏召回（filter-after-topK 经典 bug）。
//
// 语义：各维度之间是 AND；同一维度内的多值是 OR。任一零值字段表示该维度不过滤；
// 全部零值（IsZero）等价于全量检索，走原有快路径（不 JOIN），零性能回归。
type Filter struct {
	// Sources 按 Document.Source 精确匹配（任一命中即可）。
	Sources []string
	// SourceTypes 按 Document.SourceType 匹配：manual / upload / url / file / agent（任一命中即可）。
	SourceTypes []string
	// CreatedAfter 仅保留所属文档创建时间 >= 该时刻的 chunk（零值=不限）。
	CreatedAfter time.Time
	// CreatedBefore 仅保留所属文档创建时间 <= 该时刻的 chunk（零值=不限）。
	CreatedBefore time.Time
}

// IsZero 报告该 filter 是否无任何约束（等价于全量检索）。
func (f Filter) IsZero() bool {
	return len(nonEmptyStrings(f.Sources)) == 0 &&
		len(nonEmptyStrings(f.SourceTypes)) == 0 &&
		f.CreatedAfter.IsZero() && f.CreatedBefore.IsZero()
}

// normalize 返回去掉空白/空字符串多值后的副本，避免空串污染 SQL IN 子句
// （IN (”) 会把无 source 的文档错误匹配进来）。
func (f Filter) normalize() Filter {
	f.Sources = nonEmptyStrings(f.Sources)
	f.SourceTypes = nonEmptyStrings(f.SourceTypes)
	return f
}

// hasDateBound 报告是否设置了任一日期边界。
func (f Filter) hasDateBound() bool {
	return !f.CreatedAfter.IsZero() || !f.CreatedBefore.IsZero()
}

// matchesDate 判断给定文档创建时间是否落在 [CreatedAfter, CreatedBefore] 闭区间内（零值边界=该侧不限）。
//
// 日期比较刻意放在 Go 层、按真实 time.Time 瞬时比较：底层 modernc.org/sqlite 把
// time.Time 存成 RFC3339 文本（UTC 带 Z、其它带 ±HH:MM），SQL 里的字符串 `>=` 在
// 跨时区时会按字面比较而非真实时刻（实测 +08:00 文档会被误判进 >= 某 UTC 边界），
// 故不能下推到 SQL。源/类型用 SQL IN（纯字符串相等，无此问题），日期回到 Go 保证正确。
func (f Filter) matchesDate(createdAt time.Time) bool {
	if !f.CreatedAfter.IsZero() && createdAt.Before(f.CreatedAfter) {
		return false
	}
	if !f.CreatedBefore.IsZero() && createdAt.After(f.CreatedBefore) {
		return false
	}
	return true
}

// ParseFilterDate 解析过滤用日期串供 Filter.CreatedAfter/Before 使用：
// 优先 RFC3339（带时区），否则按 "2006-01-02"（UTC 零点）。空串返回零值时间（不限）。
// 供 HTTP API 与 agent 工具等字符串入口共用，确保日期解析口径一致。
func ParseFilterDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("无法解析日期 %q（需 RFC3339 或 YYYY-MM-DD）", s)
}

// nonEmptyStrings 去除切片中的空白项与纯空白项（trim 后为空则丢弃）。
func nonEmptyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// HybridConfig 混合检索配置
type HybridConfig struct {
	VectorWeight  float64 // 向量搜索权重，默认 0.7（仅 UseRRF=false 的加权和回退路径用）
	TextWeight    float64 // 关键词搜索权重，默认 0.3（同上）
	MMRLambda     float64 // MMR 多样性参数 (0=最多样, 1=最相关)，默认 0.7（无 LLM 重排时的兜底排序）
	TimeDecayDays int     // 时间衰减半衰期（天），默认 30，0=不衰减

	// ── best-practice 检索参数（RRF 融合 + LLM 重排 + 查询扩展 + 相关度地板）──
	// MinScore 向量相关度地板（作用于余弦归一分 (cos+1)/2 ∈ [0,1]），默认 0.85；0=关。
	// 0.85 为真机标定值（BUG-20260712-O，nomic-embed-text 中文实测）：无关对归一分
	// 0.754~0.820（旧默认 0.55=cos 0.1 形同虚设，天气 query 曾放行《Go面试题》），
	// 相关对 0.917~0.952——0.85 落在分界带内、双侧留 margin。显式检索有放宽回退不受影响。
	MinScore      float64
	CandidateK    int     // 宽召回候选池大小（rerank 前 over-retrieve），默认 50
	RRFK          float64 // RRF 融合常数 k，默认 60（业界标准，Cormack et al. SIGIR 2009）
	UseRRF        bool    // true=用 RRF 融合替代朴素加权和（量纲不可比），默认 true
	RerankEnabled bool    // true=启用 LLM 重排（需 WithLLM 注入），默认 true
	ExpandEnabled bool    // true=启用 HyDE + multi-query 查询扩展（需 WithLLM 注入），默认 true

	ContextualEnabled bool // true=入库时给 chunk 前置文档级上下文（Anthropic Contextual Retrieval），默认 true

	// query/doc 嵌入非对称（#12）：对支持任务前缀的模型（如 nomic：search_query/search_document）
	// 给查询与文档分别前置不同指令前缀以提升检索质量。仅作用于 embedding 输入，不改存储内容/BM25。空=不加。
	EmbedQueryPrefix string
	EmbedDocPrefix   string
}

// DefaultHybridConfig 返回默认混合检索配置（best-practice：全开）
func DefaultHybridConfig() HybridConfig {
	return HybridConfig{
		VectorWeight:      0.7,
		TextWeight:        0.3,
		MMRLambda:         0.7,
		TimeDecayDays:     30,
		MinScore:          0.85, // 真机标定（BUG-20260712-O），依据见字段注释
		CandidateK:        50,
		RRFK:              60,
		UseRRF:            true,
		RerankEnabled:     true,
		ExpandEnabled:     true,
		ContextualEnabled: true,
	}
}

// ─── Repository Interface (Command — 写路径) ────────────

// DocumentRepository 文档持久化接口
//
// 负责文档和 Chunk 的生命周期管理（CRUD）。
// 实现者管理底层存储事务，确保文档与 Chunk 的原子性。
type DocumentRepository interface {
	// Init 初始化存储（建表、迁移）
	Init(ctx context.Context) error

	// Add 添加文档及其全部 Chunk（原子操作）
	Add(ctx context.Context, doc *Document, chunks []*Chunk) error

	// Get 获取单个文档详情（含正文）
	Get(ctx context.Context, docID string) (*Document, error)

	// List 列出所有文档（不含正文）
	List(ctx context.Context) ([]*Document, error)

	// GetBySourceTitle 按 (source, title) 索引查询单个文档（不含正文），
	// 不存在返回 (nil, nil)。用于 upsert 命中判定，避免全表 List 扫描。
	GetBySourceTitle(ctx context.Context, source, title string) (*Document, error)

	// Replace 替换文档的 Chunk（用于重建索引，原子操作）
	Replace(ctx context.Context, doc *Document, chunks []*Chunk) error

	// Delete 删除文档及其所有关联数据（原子操作）
	Delete(ctx context.Context, docID string) error
}

// SearchableCorpus reports whether retrieval has any indexed chunks. It is an
// optional fast-path interface: repositories that implement it let Manager
// avoid query expansion and embedding calls for an empty knowledge base.
type SearchableCorpus interface {
	HasSearchableDocuments(ctx context.Context) (bool, error)
}

// ─── Query Interface (Query — 读路径) ───────────────────

// ChunkSearcher 知识检索接口
//
// 负责从已索引的 Chunk 中检索相关结果。
// 实现者可以基于向量相似度、全文搜索或两者混合。
type ChunkSearcher interface {
	// VectorSearch 向量语义搜索，返回余弦相似度最高的 Chunk。
	// filter 在打分/截断前下推到存储层（filter.IsZero() 时走全量快路径）。
	VectorSearch(ctx context.Context, queryVec []float32, topK int, filter Filter) ([]*SearchResult, error)

	// TextSearch 全文关键词搜索（FTS5 / BM25），返回匹配度最高的 Chunk。
	// filter 在 LIMIT 前下推到存储层（filter.IsZero() 时走全量快路径）。
	TextSearch(ctx context.Context, query string, topK int, filter Filter) ([]*SearchResult, error)
}

// ─── Manager (Application Layer) ────────────────────────

// Manager 知识库管理器
//
// 协调写路径（DocumentRepository）和读路径（ChunkSearcher），
// 加上 hexagon 的 Splitter / Embedder，完成完整的 RAG 管线。
type Manager struct {
	repo      DocumentRepository     // 写路径: 文档 + Chunk CRUD
	searcher  ChunkSearcher          // 读路径: 向量搜索 + 关键词搜索
	embedder  hexagon.VectorEmbedder // hexagon/ai-core 向量嵌入（可为 nil）
	splitter  hexagon.Splitter       // hexagon 文本分块器
	llm       RerankLLM              // 查询扩展 / contextual-ingest / LLM 兜底重排用的 LLM（可为 nil → 自动降级）
	reranker  reranker.Reranker      // 专用文档重排器（如 cross-encoder via /rerank）；nil 时退回 LLM 重排
	captioner Captioner              // 图像转写器（VLM caption）；nil 时 AddImageDocument 优雅报错（见 multimodal.go）

	// config 混合检索配置。atomic.Pointer 使其可在运行时被 SetHybridConfig 原子热替换
	// （检索参数面板 PUT /knowledge/config），而读路径（searchResults 等）在并发检索时
	// 无锁取一致快照——读多写少，避免给热检索路径加锁。
	config atomic.Pointer[HybridConfig]

	// snapshotRetention 每个快照系列保留的最大文档数（IngestSnapshot 用）；0=不限。
	snapshotRetention int

	// auxBreaker RAG 辅助 LLM（查询扩展 / LLM 重排）的预算熔断状态（BUG-20260704）；
	// 跨检索共享，慢 provider 连续超预算即开闸冷却，期间纯确定性检索。零值可用。
	auxBreaker auxLLMBreaker
}

// retrievalLLM 返回带预算+熔断的辅助 LLM，用于聊天关键路径上的查询扩展 / LLM 重排
// （BUG-20260704）；m.llm 为 nil 时返回 nil（调用方自动降级）。与原始 m.llm 区分：
// 后者仍供离线 contextual-ingest 用（非关键路径，可容忍慢），不加预算。
func (m *Manager) retrievalLLM() RerankLLM {
	if m.llm == nil {
		return nil
	}
	return &budgetedRerankLLM{inner: m.llm, breaker: &m.auxBreaker}
}

// RerankLLM 是重排 / 查询扩展 / contextual-ingest 所需的最小 LLM 能力面（单 prompt 补全）。
// 与 hexagon rag/reranker、rag/query 的 LLMProvider 接口同形，可直接复用同一适配器。
type RerankLLM interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// RerankLLMFunc 把普通函数适配为 RerankLLM。
type RerankLLMFunc func(ctx context.Context, prompt string) (string, error)

// Complete 实现 RerankLLM。
func (f RerankLLMFunc) Complete(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

// ManagerOption Manager 配置选项
type ManagerOption func(*Manager)

// WithHybridConfig 设置混合检索配置
func WithHybridConfig(cfg HybridConfig) ManagerOption {
	return func(m *Manager) { m.config.Store(&cfg) }
}

// cfg 取当前混合检索配置的快照（无锁原子读，并发检索安全）。
func (m *Manager) cfg() HybridConfig { return *m.config.Load() }

// GetHybridConfig 返回当前生效的混合检索配置（快照副本，供 GET /knowledge/config 读取）。
func (m *Manager) GetHybridConfig() HybridConfig { return m.cfg() }

// SetHybridConfig 在运行时原子热替换混合检索配置（PUT /knowledge/config）。
//
// 即时生效的读路径参数：rerank/query-expand/contextual 开关、min_score、candidate_k、
// 融合权重、时间衰减等。注意：专用 cross-encoder 重排器（rerank_model 对应的 reranker）
// 在 NewManager 时一次性注入，更换 rerank_model 需重建 Manager（重启 sidecar）才生效。
func (m *Manager) SetHybridConfig(c HybridConfig) { m.config.Store(&c) }

// WithSplitter 设置文本分块器（hexagon hexagon.Splitter）
func WithSplitter(s hexagon.Splitter) ManagerOption {
	return func(m *Manager) { m.splitter = s }
}

// WithLLM 注入重排 / 查询扩展 / contextual-ingest 所用的 LLM（通常复用 Agent 的 LLM router）。
// 不注入时，rerank / query-expand / contextual 自动降级关闭（省成本，安全）。
func WithLLM(llm RerankLLM) ManagerOption {
	return func(m *Manager) { m.llm = llm }
}

// WithDocReranker 注入专用文档重排器（如 cross-encoder via /rerank 接口）。
// 优先于 LLM-as-reranker——更快、更省、质量同级或更好。未注入则退回 LLM 重排。
func WithDocReranker(r reranker.Reranker) ManagerOption {
	return func(m *Manager) { m.reranker = r }
}

// WithSnapshotRetention 设置每个快照系列保留的最大文档数（IngestSnapshot 用）。
// n<=0 表示不裁剪（保留全部）。
func WithSnapshotRetention(n int) ManagerOption {
	return func(m *Manager) { m.snapshotRetention = n }
}

// NewManager 创建知识库管理器
//
// repo 和 searcher 通常由同一个 SQLiteStore 实例同时实现。
// embedder 可为 nil，此时退化为纯关键词搜索模式。
func NewManager(repo DocumentRepository, searcher ChunkSearcher, embedder hexagon.VectorEmbedder, opts ...ManagerOption) *Manager {
	// Fail fast: a nil repo/searcher is a programmer error that would otherwise
	// surface as an obscure nil-deref deep inside a request.
	if repo == nil {
		panic("knowledge.NewManager: repo must not be nil")
	}
	if searcher == nil {
		panic("knowledge.NewManager: searcher must not be nil")
	}
	m := &Manager{
		repo:     repo,
		searcher: searcher,
		embedder: embedder,
	}
	def := DefaultHybridConfig()
	m.config.Store(&def) // 默认配置；WithHybridConfig 选项会原子覆盖
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// ─── Command Methods (写路径) ───────────────────────────

// AddDocument 添加文档到知识库
//
// 流程：hexagon 分块 → 生成向量 → Repository 持久化
func (m *Manager) AddDocument(ctx context.Context, title, content, source string) (*Document, error) {
	return m.addDocumentTyped(ctx, title, content, source, sourceTypeFromSource(source))
}

// addDocumentTyped is AddDocument with an explicit source_type, so the snapshot
// path can declare "agent" (a scheduled collection is autonomous by definition,
// regardless of the human-meaningful source label the model attaches) while the
// interactive path keeps the label-derived classification.
func (m *Manager) addDocumentTyped(ctx context.Context, title, content, source, sourceType string) (*Document, error) {
	if content == "" {
		return nil, fmt.Errorf("文档内容不能为空")
	}

	now := time.Now()

	// Upsert by (source, title): a scheduled job re-ingesting the same
	// titled document updates it in place and refreshes UpdatedAt instead of
	// accumulating duplicates (BUG-20260613). An empty title can't be matched
	// reliably, so it always inserts.
	existing, err := m.findBySourceTitle(ctx, source, title)
	if err != nil {
		return nil, fmt.Errorf("查询已有文档失败: %w", err)
	}
	if existing != nil {
		doc := &Document{
			ID:         existing.ID,
			Title:      title,
			Content:    content,
			Source:     source,
			CreatedAt:  existing.CreatedAt,
			UpdatedAt:  now,
			Status:     "indexed",
			SourceType: sourceType,
		}
		chunks, err := m.buildChunks(ctx, doc, now)
		if err != nil {
			return nil, err
		}
		if err := m.repo.Replace(ctx, doc, chunks); err != nil {
			return nil, fmt.Errorf("更新文档失败: %w", err)
		}
		return doc, nil
	}

	doc := &Document{
		ID:         "doc-" + idgen.ShortID(),
		Title:      title,
		Content:    content,
		Source:     source,
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     "indexed",
		SourceType: sourceType,
	}

	chunks, err := m.buildChunks(ctx, doc, now)
	if err != nil {
		return nil, err
	}

	if err := m.repo.Add(ctx, doc, chunks); err != nil {
		// Lost the read-then-insert race against the production
		// idx_kb_documents_unique(source,title) index — a concurrent ingest of
		// the same titled doc inserted first. Retry as an in-place update so
		// the caller sees an idempotent upsert, not a raw constraint 500
		// (BUG-20260613 review C1/C2).
		if isUniqueConstraintErr(err) {
			// On the race-fallback we already hold an Add error; a lookup error
			// here cannot improve on it, so fall through to returning the original.
			if existing, ferr := m.findBySourceTitle(ctx, source, title); ferr == nil && existing != nil {
				doc.ID = existing.ID
				doc.CreatedAt = existing.CreatedAt
				rechunks, cerr := m.buildChunks(ctx, doc, now)
				if cerr != nil {
					return nil, cerr
				}
				if rerr := m.repo.Replace(ctx, doc, rechunks); rerr != nil {
					return nil, fmt.Errorf("更新文档失败: %w", rerr)
				}
				return doc, nil
			}
		}
		return nil, fmt.Errorf("保存文档失败: %w", err)
	}
	return doc, nil
}

// IngestSnapshot writes one run of a scheduled-task "snapshot series" identified
// by (source, baseTitle). It is the write path for cron/automation collectors,
// and differs from AddDocument's upsert in three deliberate ways:
//
//  1. Append, never overwrite: the stored title is baseTitle + a timestamp
//     suffix (SnapshotTitle), made unique even within the same second, so each
//     run is a distinct document — a time series, not one mutated doc.
//  2. Skip-if-unchanged: if the content is byte-for-byte (whitespace-normalized)
//     identical to the latest snapshot of this series, nothing is written or
//     re-embedded; the existing doc is returned with written=false. This is what
//     keeps a stable collector (e.g. hourly "百度热搜" that rarely changes) from
//     flooding the index with near-duplicates.
//  3. Retention: after writing, snapshots beyond snapshotRetention (newest-kept)
//     are pruned, bounding storage and keeping vector recall from being swamped.
//
// Returns the document (the new one, or the unchanged latest) and whether a new
// document was actually written.
func (m *Manager) IngestSnapshot(ctx context.Context, baseTitle, content, source string) (*Document, bool, error) {
	if strings.TrimSpace(content) == "" {
		return nil, false, fmt.Errorf("文档内容不能为空")
	}
	base := strings.TrimSpace(baseTitle)
	if base == "" {
		base = deriveSnapshotBaseTitle(content)
	}

	// (2) Skip-if-unchanged vs the latest snapshot of this series.
	hash := contentHash(content)
	if latest, err := m.latestSnapshot(ctx, source, base); err != nil {
		return nil, false, err
	} else if latest != nil {
		full, err := m.repo.Get(ctx, latest.ID)
		if err != nil {
			return nil, false, fmt.Errorf("读取上一快照失败: %w", err)
		}
		if full != nil && contentHash(full.Content) == hash {
			return full, false, nil
		}
	}

	// (1) Append: pick a title that does not already exist, so AddDocument
	// inserts a fresh document instead of upserting over an existing one. A
	// same-second collision (rare; a series is serialized by the cron overlap
	// guard) is disambiguated with a " (N)" suffix rather than overwriting.
	title := SnapshotTitle(base)
	for n := 2; ; n++ {
		exists, err := m.repo.GetBySourceTitle(ctx, source, title)
		if err != nil {
			return nil, false, fmt.Errorf("查询快照标题失败: %w", err)
		}
		if exists == nil {
			break
		}
		title = fmt.Sprintf("%s (%d)", SnapshotTitle(base), n)
	}

	// Scheduled snapshots are autonomous → source_type "agent" regardless of the
	// label, so the desktop type filter buckets them as agent-collected, not as
	// files (real-LLM E2E caught the model attaching a "用户输入"-style source).
	doc, err := m.addDocumentTyped(ctx, title, content, source, "agent")
	if err != nil {
		return nil, false, err
	}

	// (3) Retention: prune oldest snapshots beyond the cap. Best-effort — a
	// prune failure must not fail the run (the document is already stored).
	if m.snapshotRetention > 0 {
		if err := m.pruneSnapshotSeries(ctx, source, base, m.snapshotRetention); err != nil {
			logger.Warn("快照系列裁剪失败", "source", source, "base", base, "err", err.Error())
		}
	}
	return doc, true, nil
}

// latestSnapshot returns the newest document in the (source, baseTitle) snapshot
// series, or nil if the series is empty. repo.List is ordered created_at DESC,
// so the first series match is the newest.
func (m *Manager) latestSnapshot(ctx context.Context, source, baseTitle string) (*Document, error) {
	docs, err := m.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出文档失败: %w", err)
	}
	for _, d := range docs {
		if d.Source == source && isSnapshotTitleOf(d.Title, baseTitle) {
			return d, nil
		}
	}
	return nil, nil
}

// pruneSnapshotSeries deletes the oldest documents of the (source, baseTitle)
// series so at most keep remain. List is created_at DESC, so everything past
// index keep-1 is older and gets removed.
func (m *Manager) pruneSnapshotSeries(ctx context.Context, source, baseTitle string, keep int) error {
	docs, err := m.repo.List(ctx)
	if err != nil {
		return err
	}
	kept := 0
	for _, d := range docs {
		if d.Source != source || !isSnapshotTitleOf(d.Title, baseTitle) {
			continue
		}
		kept++
		if kept <= keep {
			continue
		}
		if err := m.repo.Delete(ctx, d.ID); err != nil {
			return err
		}
	}
	return nil
}

// deriveSnapshotBaseTitle builds a fallback base title from content's first line
// when the caller supplies none. Mirrors the agent skill's 24-rune clip so the
// two write paths label untitled snapshots consistently.
func deriveSnapshotBaseTitle(content string) string {
	line := strings.TrimSpace(content)
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	if r := []rune(line); len(r) > 24 {
		return string(r[:24])
	}
	if line == "" {
		return "快照"
	}
	return line
}

// isUniqueConstraintErr reports whether err is a SQLite UNIQUE-constraint
// violation (the upsert's race fallback trigger).
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || strings.Contains(s, "constraint failed: UNIQUE")
}

// findBySourceTitle returns an existing document matching both source and a
// non-empty title, or nil. Backed by the (source, title) index via
// GetBySourceTitle instead of a full-table List scan (review M3). The returned
// doc omits content, which is fine — only the ID and timestamps are reused on
// upsert.
func (m *Manager) findBySourceTitle(ctx context.Context, source, title string) (*Document, error) {
	if title == "" {
		return nil, nil
	}
	// GetBySourceTitle returns (nil, nil) on a genuine miss and (nil, err) on a
	// real DB failure — propagate the latter so a transient error is not
	// mistaken for "not found" (which would wrongly insert a duplicate).
	return m.repo.GetBySourceTitle(ctx, source, title)
}

// ReindexDocument 重新切分并重建指定文档的索引
func (m *Manager) ReindexDocument(ctx context.Context, docID string) (*Document, error) {
	doc, err := m.repo.Get(ctx, docID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("文档不存在")
	}
	if strings.TrimSpace(doc.Content) == "" {
		return nil, fmt.Errorf("文档内容为空，无法重建索引")
	}

	doc.UpdatedAt = time.Now()
	doc.Status = "indexed"
	doc.ErrorMessage = ""
	if doc.SourceType == "" {
		doc.SourceType = sourceTypeFromSource(doc.Source)
	}

	chunks, err := m.buildChunks(ctx, doc, doc.UpdatedAt)
	if err != nil {
		return nil, err
	}

	if err := m.repo.Replace(ctx, doc, chunks); err != nil {
		return nil, fmt.Errorf("重建文档索引失败: %w", err)
	}
	return doc, nil
}

// DeleteDocument 删除文档
func (m *Manager) DeleteDocument(ctx context.Context, docID string) error {
	return m.repo.Delete(ctx, docID)
}

// GetDocument 获取单个文档详情（含正文）
func (m *Manager) GetDocument(ctx context.Context, docID string) (*Document, error) {
	return m.repo.Get(ctx, docID)
}

// ListDocuments 列出所有文档
func (m *Manager) ListDocuments(ctx context.Context) ([]*Document, error) {
	return m.repo.List(ctx)
}

// SourceCount 是按 source 聚合的文档计数（供 UI 分组/过滤的轻量 facet）。
type SourceCount struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
}

// DocListQuery 文档分页/过滤查询。
type DocListQuery struct {
	Source string // 仅返回该 source 的文档（空=不过滤）
	Limit  int    // 单页最大条数（<=0 表示不分页，返回全部）
	Offset int    // 偏移（<0 视为 0）
}

// DocListResult 文档分页结果。
type DocListResult struct {
	Documents []*Document   `json:"documents"`
	Total     int           `json:"total"`   // 过滤后、分页前的总数
	Limit     int           `json:"limit"`   // 生效的 limit（回显）
	Offset    int           `json:"offset"`  // 生效的 offset（回显）
	Sources   []SourceCount `json:"sources"` // 全量（未过滤）按 source 的计数，供分组
}

// ListDocumentsPaged 列出文档，支持按 source 过滤 + 分页，并附带按 source 的计数
// facet（防止 8760 条快照把列表页拖垮，并支撑「按来源折叠分组」的前端）。
//
// facet 基于「未过滤」的全集统计，使前端无需把全部文档拉到本地即可渲染来源分组；
// documents/total 则反映「过滤后」的页。retention 让这里的全表扫描规模可控。
func (m *Manager) ListDocumentsPaged(ctx context.Context, q DocListQuery) (*DocListResult, error) {
	all, err := m.repo.List(ctx)
	if err != nil {
		return nil, err
	}

	// Source facet over the unfiltered set (newest-first List preserves a stable
	// first-seen order for the facet slice).
	counts := make(map[string]int, 8)
	order := make([]string, 0, 8)
	for _, d := range all {
		if _, seen := counts[d.Source]; !seen {
			order = append(order, d.Source)
		}
		counts[d.Source]++
	}
	sources := make([]SourceCount, 0, len(order))
	for _, src := range order {
		sources = append(sources, SourceCount{Source: src, Count: counts[src]})
	}

	// Filter.
	filtered := all
	if q.Source != "" {
		filtered = make([]*Document, 0, len(all))
		for _, d := range all {
			if d.Source == q.Source {
				filtered = append(filtered, d)
			}
		}
	}
	total := len(filtered)

	// Paginate (limit<=0 → no paging, return the whole filtered set).
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	page := filtered[offset:]
	if q.Limit > 0 && len(page) > q.Limit {
		page = page[:q.Limit]
	}
	if page == nil {
		page = []*Document{}
	}

	return &DocListResult{
		Documents: page,
		Total:     total,
		Limit:     q.Limit,
		Offset:    offset,
		Sources:   sources,
	}, nil
}

// ─── Query Methods (读路径) ─────────────────────────────

// Search 返回结构化搜索结果，供 API/UI 展示（无元数据过滤）。
func (m *Manager) Search(ctx context.Context, query string, topK int) ([]SearchHit, error) {
	return m.SearchWithFilter(ctx, query, topK, Filter{})
}

// SearchWithFilter 在元数据过滤约束下检索（按 source / source_type / 创建日期下推到存储层）。
// 过滤在打分与 topK 截断之前生效，确保不会因截断漏召回匹配文档。
func (m *Manager) SearchWithFilter(ctx context.Context, query string, topK int, filter Filter) ([]SearchHit, error) {
	selected, err := m.searchResults(ctx, query, topK, filter)
	if err != nil {
		return nil, err
	}
	return hitsFromResults(selected), nil
}

// hitsFromResults 把内部检索结果映射为对外的 SearchHit。
func hitsFromResults(selected []*SearchResult) []SearchHit {
	hits := make([]SearchHit, 0, len(selected))
	for _, r := range selected {
		hits = append(hits, SearchHit{
			DocID:      r.Chunk.DocID,
			DocTitle:   r.Chunk.DocTitle,
			Source:     r.Chunk.Source,
			ChunkID:    r.Chunk.ID,
			ChunkIndex: r.Chunk.Index,
			ChunkCount: r.Chunk.ChunkCount,
			Content:    r.Chunk.Content,
			Score:      r.Chunk.Score,
			CreatedAt:  r.Chunk.CreatedAt,
			Metadata:   chunkMetadata(r.Chunk),
		})
	}
	return hits
}

// chunkMetadata 暴露 chunk 的可过滤/可展示元数据（source_type、创建时间），
// 让上层（API/UI/agent）能按维度筛选与回显。无可用字段时返回 nil（保持 JSON 干净）。
func chunkMetadata(c *Chunk) map[string]any {
	md := make(map[string]any, 2)
	if c.SourceType != "" {
		md["source_type"] = c.SourceType
	}
	if !c.CreatedAt.IsZero() {
		md["created_at"] = c.CreatedAt.UTC().Format(time.RFC3339)
	}
	if len(md) == 0 {
		return nil
	}
	return md
}

// Query 混合检索知识库，返回格式化的 LLM 上下文。
//
// 检索全链路（查询扩展 → 宽召回 → RRF 融合 → 相关度地板 → LLM 重排）
// 统一落在 Manager.searchResults，无 feature-flag 门控、默认即最佳实践配置；
// 缺 LLM 时自动降级（跳过 rerank / query-expand），缺 embedder 时退化纯关键词。
func (m *Manager) Query(ctx context.Context, query string, topK int) (string, error) {
	return m.QueryWithFilter(ctx, query, topK, Filter{})
}

// QueryWithFilter 同 Query，但在元数据过滤约束下检索（source / source_type / 创建日期）。
//
// 注入语义（BUG-20260703 B8）：本路径 fail-closed——仅语义相关度过地板（VectorScore >=
// MinScore）的候选可进上下文，无强命中返回空串，让模型如实答"未找到"；绝不把仅
// 通用词法重叠的弱相关文档（"公司/地址"这类分词命中）端给模型编答案。
func (m *Manager) QueryWithFilter(ctx context.Context, query string, topK int, filter Filter) (string, error) {
	selected, err := m.searchResultsMode(ctx, query, topK, filter, true)
	if err != nil {
		return "", err
	}
	return formatSearchHits(hitsFromResults(selected)), nil
}

// QueryHitsWithFilter 与 QueryWithFilter 使用同一次严格检索，但把结构化命中一并返回。
// 面向 UI/领域适配器的调用方必须消费 SearchHit.Content，不能把给 LLM 的注入信封
// （“参考 N / 相关度 / 请基于…”）当成面向用户的正文。
func (m *Manager) QueryHitsWithFilter(ctx context.Context, query string, topK int, filter Filter) (string, []SearchHit, error) {
	selected, err := m.searchResultsMode(ctx, query, topK, filter, true)
	if err != nil {
		return "", nil, err
	}
	hits := hitsFromResults(selected)
	return formatSearchHits(hits), hits, nil
}

// QueryHits 同 Query（fail-closed 严格地板），但同时返回格式化上下文与结构化命中列表。
//
// U9：引擎自动 RAG 注入点需要「注入了什么」的结构化命中回传前端渲染命中标签+详情。
// 命中集与 Query 注入的上下文**同源同判据**（同一次 searchResultsMode strict 检索），
// 保证「标签显示的命中数」== 「真正端给模型的命中数」，不产生二次检索的漂移。
func (m *Manager) QueryHits(ctx context.Context, query string, topK int) (string, []SearchHit, error) {
	selected, err := m.searchResultsMode(ctx, query, topK, Filter{}, true)
	if err != nil {
		return "", nil, err
	}
	hits := hitsFromResults(selected)
	return formatSearchHits(hits), hits, nil
}

func (m *Manager) searchResults(ctx context.Context, query string, topK int, filter Filter) ([]*SearchResult, error) {
	return m.searchResultsMode(ctx, query, topK, filter, false)
}

// searchResultsMode 是检索全链路的实现。strictFloor 区分两种召回语义：
//   - false（显式检索：桌面 KB 页 / knowledge_search 工具 / API）：宽召回——词法命中
//     放行、地板清空时放宽回退，用户看得到相关度自行判断；
//   - true（聊天自动注入 Query/QueryWithFilter）：fail-closed——仅 VectorScore 过地板
//     的候选保留，清空即空，宁缺勿滥（BM25 分是结果集内 min-max 归一，最佳垃圾恒为
//     1.0，不能当跨查询可比的相关性用）。
func (m *Manager) searchResultsMode(ctx context.Context, query string, topK int, filter Filter, strictFloor bool) ([]*SearchResult, error) {
	if topK <= 0 {
		topK = 3
	}
	if corpus, ok := m.repo.(SearchableCorpus); ok {
		hasDocuments, err := corpus.HasSearchableDocuments(ctx)
		if err == nil && !hasDocuments {
			return nil, nil
		}
	}
	cfg := m.cfg()
	candidateK := cfg.CandidateK
	if candidateK <= 0 {
		candidateK = 50
	}
	if candidateK < topK*3 {
		candidateK = topK * 3 // 至少留够 rerank 收窄空间
	}

	// 1. 查询扩展（#8 HyDE + multi-query）。向量能力待机时直接走原始 query
	// 的 FTS 路径：自动注入没有语义证据本就 fail-closed，调用辅助 LLM 只会平添延迟。
	embeddingReady := m.embedder != nil && EmbeddingReady(ctx, m.embedder)
	queries := []string{query}
	if embeddingReady {
		queries = m.expandQueries(ctx, query)
	}

	// 2. 宽召回：每个 query 各取一路向量 + 一路 BM25，记录各排序列表喂给 RRF（#6 over-retrieve）
	resultMap := make(map[string]*SearchResult)
	var rankedLists []rankedList
	// vectorRouteRan：查询时向量路是否真实跑通。嵌入/向量搜索失败（如 embedding 服务
	// 不可用）时无语义证据可要求，严格地板退回宽召回语义，避免降级态下 RAG 全盲。
	vectorRouteRan := false

	for _, q := range queries {
		if embeddingReady {
			// 查询向量化预算（BUG-20260703 同构防护，对齐 engine 记忆召回）：检索是增强，
			// 不继承整请求 ctx 的漫长余量——慢 embedding 端点超预算即掐断，本轮走纯 BM25。
			ectx, ecancel := context.WithTimeout(ragEmbedContext(ctx), queryEmbedTimeout)
			qv, err := m.embedder.Embed(ectx, []string{cfg.EmbedQueryPrefix + q})
			ecancel()
			if err != nil {
				if !errors.Is(err, ErrEmbeddingUnavailable) {
					logger.Error("[knowledge] 查询向量嵌入失败", "error", err)
				}
			} else if len(qv) > 0 {
				vres, vErr := m.searcher.VectorSearch(ctx, qv[0], candidateK, filter)
				if vErr != nil {
					logger.Error("[knowledge] 向量搜索失败", "error", vErr)
				} else {
					rankedLists = append(rankedLists, mergeRanked(resultMap, vres, true))
					vectorRouteRan = true
				}
			}
		}
		tres, tErr := m.searcher.TextSearch(ctx, q, candidateK, filter)
		if tErr != nil {
			logger.Error("[knowledge] 关键词搜索失败", "error", tErr)
		} else {
			rankedLists = append(rankedLists, mergeRanked(resultMap, tres, false))
		}
	}

	if len(resultMap) == 0 {
		return nil, nil
	}

	// 3. 融合评分（#9 RRF 或加权和回退）+ 时间衰减
	candidates := m.fuse(resultMap, rankedLists, vectorRouteRan)

	// 4. 相关度地板（#3）：宽召回模式带放宽回退；注入模式 fail-closed（B8）。
	// BUG-20260712-I：降级态（embedder 未配置 / Embed 失败超时 → 向量路未跑通）不再把
	// 注入模式放宽成宽召回——没有语义证据即返回空。BM25 是结果集内 min-max 归一分，
	// 「最佳垃圾恒 1.0」不构成相关性证据（真机取证：天气 query 注入《Go面试题》，相关度 0-2%
	// 照样端给模型+前端命中卡）。「避免降级态 RAG 全盲」只属于显式检索（Search*）语义。
	if strictFloor && !vectorRouteRan {
		return nil, nil
	}
	candidates = m.applyMinScore(candidates, strictFloor)

	// 5. 宽召回 → 重排 → 收窄（#6）；无 LLM/关闭时回退 MMR 多样性选取
	return m.rerankTopK(ctx, query, candidates, topK), nil
}

// rankedList 是一路检索的有序候选（带模态：向量 / 文本），用于分数加权 RRF。
type rankedList struct {
	ids      []string
	isVector bool
}

// mergeRanked 把一路搜索结果并入 resultMap（按 chunkID 去重，向量/文本分各取较大），
// 返回该路的有序候选列表（带模态，喂给 RRF 融合）。isVector 决定合并哪类分数。
func mergeRanked(resultMap map[string]*SearchResult, results []*SearchResult, isVector bool) rankedList {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		cur, ok := resultMap[r.Chunk.ID]
		if !ok {
			resultMap[r.Chunk.ID] = r
			cur = r
		} else {
			if cur.Chunk.Content == "" && r.Chunk.Content != "" {
				cur.Chunk.Content = r.Chunk.Content
			}
			if len(cur.Chunk.Embedding) == 0 && len(r.Chunk.Embedding) > 0 {
				cur.Chunk.Embedding = r.Chunk.Embedding
			}
			if cur.Chunk.CreatedAt.IsZero() && !r.Chunk.CreatedAt.IsZero() {
				cur.Chunk.CreatedAt = r.Chunk.CreatedAt
			}
		}
		if isVector {
			if r.VectorScore > cur.VectorScore {
				cur.VectorScore = r.VectorScore
			}
		} else if r.TextScore > cur.TextScore {
			cur.TextScore = r.TextScore
		}
		ids = append(ids, r.Chunk.ID)
	}
	return rankedList{ids: ids, isVector: isVector}
}

// fuse 用「分数加权 RRF」（#9/#11，默认）或朴素加权和（回退）给候选打分，并施加时间衰减。
//
// #11 修复：纯 rank RRF 会把「孤立弱 BM25 命中」当作与强命中同等的 rank-1 满权，导致
// 跨语种/无词法重叠时一个虚假词法命中压过强向量命中。改为按各路归一化分(VectorScore/
// TextScore)加权 rank 贡献：score(d) = Σ_list w_list · normScore(d,list) / (k + rank)。
// 这样弱命中（低 normScore）的 rank 红利被同比缩小，而真正的精确命中（高 BM25 分）仍保留
// 满权 —— 既根治虚假命中带偏，又不损失精确术语匹配能力。
func (m *Manager) fuse(resultMap map[string]*SearchResult, rankedLists []rankedList, vectorRouteRan bool) []*SearchResult {
	cfg := m.cfg()
	candidates := make([]*SearchResult, 0, len(resultMap))
	if cfg.UseRRF && len(rankedLists) > 0 {
		k := cfg.RRFK
		if k <= 0 {
			k = 60
		}
		vw, tw := cfg.VectorWeight, cfg.TextWeight
		if vw <= 0 && tw <= 0 {
			vw, tw = 0.7, 0.3
		}
		if !vectorRouteRan {
			vw, tw = 0, 1 // 无向量时退化纯关键词
		}
		fused := make(map[string]float64, len(resultMap))
		for _, list := range rankedLists {
			w := tw
			if list.isVector {
				w = vw
			}
			for rank, id := range list.ids {
				r := resultMap[id]
				s := r.TextScore
				if list.isVector {
					s = r.VectorScore
				}
				fused[id] += w * s / (k + float64(rank+1))
			}
		}
		for id, r := range resultMap {
			r.Chunk.Score = m.applyTimeDecay(fused[id], r.Chunk.CreatedAt)
			candidates = append(candidates, r)
		}
	} else {
		for _, r := range resultMap {
			r.Chunk.Score = m.hybridScoreMode(r, vectorRouteRan)
			candidates = append(candidates, r)
		}
	}
	return candidates
}

// applyMinScore 施加相关度地板（#3）。
//
// 宽召回模式（strict=false，显式检索）：丢弃"纯弱向量命中"（语义低于地板且无关键词
// 支撑），有关键词命中的候选保留；过滤后为空则放宽回退，保证不清空。
//
// 严格模式（strict=true，聊天自动注入）：仅 VectorScore 过地板者保留——TextScore 是
// 结果集内 min-max 归一分（最佳垃圾恒 1.0），不构成跨查询可比的相关性证据；清空即
// 返回空，无放宽回退（BUG-20260703 B8：宁缺勿滥，无强命中让模型如实答"未找到"）。
//
// 两种模式下，MinScore=0 或无 embedder（纯关键词检索）时均不施加地板。
func (m *Manager) applyMinScore(candidates []*SearchResult, strict bool) []*SearchResult {
	minScore := m.cfg().MinScore
	if minScore <= 0 || m.embedder == nil {
		return candidates
	}
	kept := make([]*SearchResult, 0, len(candidates))
	for _, r := range candidates {
		if r.VectorScore >= minScore || (!strict && r.TextScore > 0) {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		if strict {
			return nil // fail-closed：注入路径无强相关命中即为空
		}
		return candidates // 放宽回退：避免地板把结果清空
	}
	return kept
}

// rerankTopK 宽召回 → 重排 → 收窄（#6）。
// 先按融合分降序限定 rerank 输入规模；启用且有 LLM 时走 LLM 重排，否则回退 MMR 多样性。
func (m *Manager) rerankTopK(ctx context.Context, query string, candidates []*SearchResult, topK int) []*SearchResult {
	cfg := m.cfg()
	sortByScore(candidates)
	pool := candidates
	maxRerank := cfg.CandidateK
	if maxRerank <= 0 {
		maxRerank = 50
	}
	if len(pool) > maxRerank {
		pool = pool[:maxRerank]
	}

	if cfg.RerankEnabled && len(pool) > 1 {
		if rr := m.resolveReranker(topK); rr != nil {
			if ordered, err := m.rerankWith(ctx, rr, query, pool, topK); err != nil {
				logger.Warn("[knowledge] 重排失败，回退融合分排序", "reranker", rr.Name(), "error", err)
			} else if len(ordered) > 0 {
				return ordered
			}
		}
	}
	// 回退：MMR 多样性选取（无重排器时仍保留多样性，避免近重复 chunk 占满 topK）
	return m.mmrSelect(pool, topK)
}

// resolveReranker 选重排器：专用 cross-encoder（WithDocReranker 注入）优先——更快更省更准；
// 否则 LLM-as-reranker（复用 chat 模型）；都没有则 nil（退回 MMR）。
func (m *Manager) resolveReranker(topK int) reranker.Reranker {
	if m.reranker != nil {
		return m.reranker
	}
	if m.llm != nil {
		// BUG-20260704：LLM 重排走带预算+熔断的 retrievalLLM，慢 provider 不阻塞聊天关键路径。
		return reranker.NewLLMReranker(m.retrievalLLM(), reranker.WithLLMRerankerTopK(topK))
	}
	return nil
}

// rerankWith 用给定重排器对候选精排，按返回顺序映射回 SearchResult 并截到 topK。
func (m *Manager) rerankWith(ctx context.Context, rr reranker.Reranker, query string, pool []*SearchResult, topK int) ([]*SearchResult, error) {
	docs := make([]hrag.Document, 0, len(pool))
	byID := make(map[string]*SearchResult, len(pool))
	for _, r := range pool {
		docs = append(docs, hrag.Document{ID: r.Chunk.ID, Content: r.Chunk.Content, Score: float32(r.Chunk.Score)})
		byID[r.Chunk.ID] = r
	}
	out, err := rr.Rerank(ragEnrichContext(ctx), query, docs)
	if err != nil {
		return nil, err
	}
	res := make([]*SearchResult, 0, len(out))
	for _, d := range out {
		if r, ok := byID[d.ID]; ok {
			r.Chunk.Score = float64(d.Score) // 用重排分覆盖展示分
			res = append(res, r)
		}
	}
	if topK > 0 && len(res) > topK {
		res = res[:topK]
	}
	return res, nil
}

// expandQueries 查询扩展（#8）：原始 query + multi-query 变体 + HyDE 假设文档。
// 缺 LLM 或关闭时只返回 [query]。总数限 5，控制召回成本。
func (m *Manager) expandQueries(ctx context.Context, query string) []string {
	queries := []string{query}
	if !m.cfg().ExpandEnabled || m.llm == nil {
		return queries
	}
	seen := map[string]bool{strings.TrimSpace(query): true}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			queries = append(queries, s)
		}
	}

	// BUG-20260704：查询扩展的 multi-query / HyDE 走带预算+熔断的 retrievalLLM，慢
	// provider 超预算即掐断，本函数降级返回原查询（[query]），不阻塞聊天关键路径。
	auxLLM := m.retrievalLLM()

	// 优化（BUG-20260704 续）：multi-query 与 HyDE 是相互独立的查询变换，并行跑——
	// 健康路径省一半墙钟（原串行 2×LLM），慢 provider 下两路预算超时并发发生（而非串行叠加），
	// 更快触发熔断（阈值 2）让后续 rerank 直接跳过。结果按固定顺序 add，输出确定。
	var (
		wg         sync.WaitGroup
		mqVariants []string
		mqErr      error
		hydeDoc    string
		hydeErr    error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		mqVariants, mqErr = ragquery.NewMultiQueryGenerator(
			auxLLM, ragquery.WithNumQueries(3), ragquery.WithIncludeSelf(false)).Generate(ctx, query)
	}()
	go func() {
		defer wg.Done()
		hydeDoc, hydeErr = ragquery.NewHyDEGenerator(auxLLM).Generate(ctx, query)
	}()
	wg.Wait()

	if mqErr != nil {
		logger.Warn("[knowledge] multi-query 生成失败", "error", mqErr)
	} else {
		for _, v := range mqVariants {
			add(v)
		}
	}
	if hydeErr != nil {
		logger.Warn("[knowledge] HyDE 生成失败", "error", hydeErr)
	} else {
		add(hydeDoc)
	}

	if len(queries) > 5 {
		queries = queries[:5]
	}
	return queries
}

// ─── Internal ───────────────────────────────────────────

func (m *Manager) buildChunks(ctx context.Context, doc *Document, ts time.Time) ([]*Chunk, error) {
	if m.splitter == nil {
		return nil, fmt.Errorf("未配置文本分块器 (splitter)")
	}
	if strings.TrimSpace(doc.Content) == "" {
		return nil, fmt.Errorf("文档内容为空或仅含空白字符")
	}

	ragDocs, err := m.splitter.Split(ctx, []hexagon.Document{
		{ID: doc.ID, Content: doc.Content, Source: doc.Source},
	})
	if err != nil {
		return nil, fmt.Errorf("文本分块失败: %w", err)
	}
	if len(ragDocs) == 0 {
		return nil, fmt.Errorf("文档分块后无有效片段，请检查文档内容")
	}
	doc.ChunkCount = len(ragDocs)

	// #7 Contextual Retrieval：给每个 chunk 前置「文档级上下文」（标题路径 + 可选 LLM 情境摘要），
	// 使向量 embedding 与 BM25 都索引到增强后的文本（contextual embeddings + contextual BM25）。
	m.contextualize(ctx, doc, ragDocs)

	chunkTexts := make([]string, len(ragDocs))
	for i, d := range ragDocs {
		chunkTexts[i] = d.Content
	}

	var embeddings [][]float32
	if m.embedder != nil && len(chunkTexts) > 0 {
		// #12 文档侧前缀只作用于 embedding 输入；Chunk.Content（FTS/展示）仍用原文。
		embedTexts := chunkTexts
		if docPrefix := m.cfg().EmbedDocPrefix; docPrefix != "" {
			embedTexts = make([]string, len(chunkTexts))
			for i, t := range chunkTexts {
				embedTexts[i] = docPrefix + t
			}
		}
		// BUG-20260714：嵌入模型可能正在下载/冷启动（典型为本地 nomic-embed-text），
		// 文档批量 Embed 若无独立预算会吞掉上传请求完整的 5 分钟总超时。预算按实际
		// 批次数增长：百页教材常有 200+ chunks，固定 60 秒只够第一批，后续超时会让
		// 批处理层连同已完成结果一起丢弃，最终静默变成 0 向量。超时后仍保留 FTS。
		embedCtx, cancel := context.WithTimeout(ragEmbedContext(ctx), documentEmbeddingBudget(len(embedTexts)))
		embeddings, err = m.embedder.Embed(embedCtx, embedTexts)
		cancel()
		if err != nil {
			if !errors.Is(err, ErrEmbeddingUnavailable) {
				logger.Warn("[knowledge] 生成向量嵌入失败，降级为纯文本索引", "title", doc.Title, "error", err)
			}
			embeddings = nil
		}
	}

	chunks := make([]*Chunk, len(ragDocs))
	for i, text := range chunkTexts {
		chunk := &Chunk{
			ID:         fmt.Sprintf("%s-chunk-%d", doc.ID, i),
			DocID:      doc.ID,
			DocTitle:   doc.Title,
			Source:     doc.Source,
			ChunkCount: doc.ChunkCount,
			Content:    text,
			Index:      i,
			CreatedAt:  ts,
		}
		if i < len(embeddings) {
			chunk.Embedding = embeddings[i]
		}
		chunks[i] = chunk
	}
	return chunks, nil
}

// ─── Contextual Retrieval (#7) ──────────────────────────

const (
	contextualDocCharBudget   = 6000 // 喂给 LLM 的文档正文上限（rune）
	contextualChunkCharBudget = 1200 // 喂给 LLM 的单 chunk 上限（rune）
	// maxInlineContextualLLMChunks 限制同步摄取阶段的逐 chunk LLM 增强。
	// 百页教材通常产生数百个 chunk；继续逐块补全会让一次上传发出数百次串行模型请求。
	// 超过阈值时所有 chunk 仍保留标题/章节定位，只跳过可选的 LLM 情境句。
	maxInlineContextualLLMChunks = 24
	// queryEmbedTimeout 检索路径单次查询向量化预算（BUG-20260703 同构防护）。
	// 仅约束 Search 的 query embed（单条短文本，正常远 <1s）；文档导入的批量
	// embedding 走 UpsertDocument 等独立路径，不受此预算限制。
	queryEmbedTimeout = 4 * time.Second
)

const (
	// OpenAIEmbedder 默认每 100 条发送一批；知识库预算按相同批次口径计算。
	// 本地 nomic-embed-text 实测一批百条约 50 秒，因此每批保留 60 秒预算。
	documentEmbeddingBatchSize = 100
	// 上传处理总预算为 5 分钟；嵌入最多使用 4 分钟，给解析与 SQLite 落库留余量。
	maxDocumentEmbeddingTimeout = 4 * time.Minute
)

// documentEmbeddingTimeout 是每一批文档嵌入的时间预算。var 便于回归测试压小预算，
// 不改变生产默认值；整篇文档的预算由 documentEmbeddingBudget 按批次数计算。
var documentEmbeddingTimeout = 60 * time.Second

func documentEmbeddingBudget(chunkCount int) time.Duration {
	batches := (chunkCount + documentEmbeddingBatchSize - 1) / documentEmbeddingBatchSize
	if batches < 1 {
		batches = 1
	}
	budget := time.Duration(batches) * documentEmbeddingTimeout
	if budget > maxDocumentEmbeddingTimeout {
		return maxDocumentEmbeddingTimeout
	}
	return budget
}

// contextualize 给每个 chunk 前置文档级上下文（Anthropic Contextual Retrieval）。
//
// ContextualEnabled 关闭时不做任何增强（原始 chunk）。开启时始终前置确定性定位
// 「文档标题 › 标题路径（header_path，来自 MarkdownSplitter）」；当注入了 LLM 且
// 文档多于 1 个 chunk 时，再追加一句 LLM 生成的情境摘要。增强后的文本同时用于
// 向量 embedding 与 BM25 索引，即 contextual embeddings + contextual BM25。
func (m *Manager) contextualize(ctx context.Context, doc *Document, ragDocs []hexagon.Document) {
	if !m.cfg().ContextualEnabled {
		return
	}
	useLLM := m.llm != nil && len(ragDocs) > 1 && len(ragDocs) <= maxInlineContextualLLMChunks
	var docCtx string
	if useLLM {
		docCtx = clampRunes(doc.Content, contextualDocCharBudget)
	}
	for i := range ragDocs {
		header := headerPathOf(ragDocs[i].Metadata)
		var blurb string
		if useLLM {
			if b, err := m.generateChunkContext(ctx, docCtx, ragDocs[i].Content); err != nil {
				logger.Warn("[knowledge] contextual 情境生成失败，跳过该 chunk", "error", err)
			} else {
				blurb = b
			}
		}
		if prefix := buildContextPrefix(doc.Title, header, blurb); prefix != "" {
			ragDocs[i].Content = prefix + "\n\n" + ragDocs[i].Content
		}
	}
	if m.llm != nil && len(ragDocs) > maxInlineContextualLLMChunks {
		logger.Info("[knowledge] 大文档跳过逐 chunk LLM 情境，保留确定性标题/章节定位",
			"chunks", len(ragDocs), "limit", maxInlineContextualLLMChunks, "title", doc.Title)
	}
}

// headerPathOf 从 chunk 元数据取 MarkdownSplitter 写入的 header_path（如 "# 安装 > ## 依赖"）。
func headerPathOf(md map[string]any) string {
	if md == nil {
		return ""
	}
	if v, ok := md["header_path"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// buildContextPrefix 组装「文档标题 › 标题路径」确定性定位 + 可选 LLM 情境摘要。
func buildContextPrefix(title, headerPath, blurb string) string {
	var loc []string
	if t := strings.TrimSpace(title); t != "" {
		loc = append(loc, t)
	}
	if headerPath != "" {
		loc = append(loc, headerPath)
	}
	var parts []string
	if len(loc) > 0 {
		parts = append(parts, "【定位】"+strings.Join(loc, " › "))
	}
	if blurb != "" {
		parts = append(parts, "【情境】"+blurb)
	}
	return strings.Join(parts, "  ")
}

// generateChunkContext 用 LLM 为单个 chunk 生成一句定位/主题摘要（Anthropic Contextual Retrieval）。
func (m *Manager) generateChunkContext(ctx context.Context, docContent, chunk string) (string, error) {
	prompt := fmt.Sprintf(`<document>
%s
</document>

下面是该文档中的一个片段：
<chunk>
%s
</chunk>

请用一句不超过 50 字的话，说明这个片段在整篇文档中的位置与主题，以便检索时更好地定位。只输出这一句话，不要任何解释或前后缀。`,
		docContent, clampRunes(chunk, contextualChunkCharBudget))
	// 同步入库也使用辅助 LLM 的单次预算与共享熔断。小文档仍能获得情境增强，
	// 但本地慢模型最多消耗两个预算窗口，之后快速退化为确定性定位。
	out, err := m.retrievalLLM().Complete(ragEnrichContext(ctx), prompt)
	if err != nil {
		return "", err
	}
	return clampRunes(strings.TrimSpace(out), 200), nil
}

// hybridScore preserves the package-level scoring contract used by focused
// tests and offline callers. The request path uses hybridScoreMode so a
// temporarily unavailable embedder receives true lexical-only weighting.
func (m *Manager) hybridScore(r *SearchResult) float64 {
	return m.hybridScoreMode(r, m.embedder != nil)
}

func (m *Manager) hybridScoreMode(r *SearchResult, vectorRouteRan bool) float64 {
	cfg := m.cfg()
	vectorWeight := cfg.VectorWeight
	textWeight := cfg.TextWeight
	if !vectorRouteRan {
		vectorWeight = 0
		textWeight = 1.0
	}
	score := vectorWeight*r.VectorScore + textWeight*r.TextScore
	return m.applyTimeDecay(score, r.Chunk.CreatedAt)
}

// applyTimeDecay 对分数施加指数时间衰减（半衰期 TimeDecayDays 天）。
// 仅对有有效时间戳的 chunk 衰减；CreatedAt 零值时跳过，
// 否则 time.Since(零值) 会把分数衰减到 0、导致无时间戳 chunk 永不召回。
func (m *Manager) applyTimeDecay(score float64, createdAt time.Time) float64 {
	if days := m.cfg().TimeDecayDays; days > 0 && !createdAt.IsZero() {
		age := time.Since(createdAt).Hours() / 24
		lambda := math.Ln2 / float64(days)
		score *= math.Exp(-lambda * age)
	}
	return score
}

func (m *Manager) mmrSelect(candidates []*SearchResult, topK int) []*SearchResult {
	if len(candidates) <= topK {
		sortByScore(candidates)
		return candidates
	}

	hasEmbeddings := false
	for _, c := range candidates {
		if len(c.Chunk.Embedding) > 0 {
			hasEmbeddings = true
			break
		}
	}
	if !hasEmbeddings {
		sortByScore(candidates)
		if len(candidates) > topK {
			return candidates[:topK]
		}
		return candidates
	}

	lambda := m.cfg().MMRLambda
	selected := make([]*SearchResult, 0, topK)
	remaining := make([]*SearchResult, len(candidates))
	copy(remaining, candidates)

	for len(selected) < topK && len(remaining) > 0 {
		bestIdx := -1
		bestMMR := math.Inf(-1)

		for i, cand := range remaining {
			relevance := cand.Chunk.Score
			maxSim := 0.0
			for _, sel := range selected {
				sim := cosineSimilarity(cand.Chunk.Embedding, sel.Chunk.Embedding)
				if sim > maxSim {
					maxSim = sim
				}
			}
			mmr := lambda*relevance - (1-lambda)*maxSim
			if mmr > bestMMR {
				bestMMR = mmr
				bestIdx = i
			}
		}

		if bestIdx >= 0 {
			selected = append(selected, remaining[bestIdx])
			remaining[bestIdx] = remaining[len(remaining)-1]
			remaining = remaining[:len(remaining)-1]
		}
	}
	return selected
}

func sortByScore(results []*SearchResult) {
	sort.Slice(results, func(i, j int) bool {
		return results[i].Chunk.Score > results[j].Chunk.Score
	})
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func formatSearchHits(hits []SearchHit) string {
	if len(hits) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("以下是从个人知识库中检索到的相关信息：\n\n")
	for i, hit := range hits {
		sb.WriteString(fmt.Sprintf("--- 参考 %d (相关度: %.0f%%) ---\n", i+1, hit.Score*100))
		if hit.DocTitle != "" {
			sb.WriteString(hit.DocTitle)
			if hit.Source != "" {
				sb.WriteString(" · ")
				sb.WriteString(hit.Source)
			}
			sb.WriteString("\n")
		}
		sb.WriteString(hit.Content)
		sb.WriteString("\n\n")
	}
	sb.WriteString("请基于以上参考信息回答用户的问题。如果参考信息不足以回答，请如实告知。\n")
	return sb.String()
}

func sourceTypeFromSource(source string) string {
	switch {
	case source == "":
		return "manual"
	case strings.HasPrefix(source, "upload:"):
		return "upload"
	case strings.HasPrefix(source, imageSourcePrefix):
		// 多模态图像摄取（AddImageDocument）把 source 标成 "image:..."，
		// 与 upload:/cron: 同套约定，让 source 字符串本身即编码类型。
		return "image"
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		return "url"
	case source == "agent", strings.HasPrefix(source, "cron:"):
		// Documents ingested by the agent (knowledge_ingest tool / cron jobs)
		// are not files — without this branch they fell into "file".
		return "agent"
	default:
		return "file"
	}
}
