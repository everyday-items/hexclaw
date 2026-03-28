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
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/toolkit/util/idgen"
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
	SourceType   string    `json:"source_type,omitempty"`   // manual / upload / url / file
}

// Chunk 文档片段
type Chunk struct {
	ID         string    `json:"id"`
	DocID      string    `json:"doc_id"`
	DocTitle   string    `json:"doc_title"`
	Source     string    `json:"source"`
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

// HybridConfig 混合检索配置
type HybridConfig struct {
	VectorWeight  float64 // 向量搜索权重，默认 0.7
	TextWeight    float64 // 关键词搜索权重，默认 0.3
	MMRLambda     float64 // MMR 多样性参数 (0=最多样, 1=最相关)，默认 0.7
	TimeDecayDays int     // 时间衰减半衰期（天），默认 30，0=不衰减
}

// DefaultHybridConfig 返回默认混合检索配置
func DefaultHybridConfig() HybridConfig {
	return HybridConfig{
		VectorWeight:  0.7,
		TextWeight:    0.3,
		MMRLambda:     0.7,
		TimeDecayDays: 30,
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

	// Replace 替换文档的 Chunk（用于重建索引，原子操作）
	Replace(ctx context.Context, doc *Document, chunks []*Chunk) error

	// Delete 删除文档及其所有关联数据（原子操作）
	Delete(ctx context.Context, docID string) error
}

// ─── Query Interface (Query — 读路径) ───────────────────

// ChunkSearcher 知识检索接口
//
// 负责从已索引的 Chunk 中检索相关结果。
// 实现者可以基于向量相似度、全文搜索或两者混合。
type ChunkSearcher interface {
	// VectorSearch 向量语义搜索，返回余弦相似度最高的 Chunk
	VectorSearch(ctx context.Context, queryVec []float32, topK int) ([]*SearchResult, error)

	// TextSearch 全文关键词搜索（FTS5 / BM25），返回匹配度最高的 Chunk
	TextSearch(ctx context.Context, query string, topK int) ([]*SearchResult, error)
}

// ─── Manager (Application Layer) ────────────────────────

// Manager 知识库管理器
//
// 协调写路径（DocumentRepository）和读路径（ChunkSearcher），
// 加上 hexagon 的 Splitter / Embedder，完成完整的 RAG 管线。
type Manager struct {
	repo     DocumentRepository // 写路径: 文档 + Chunk CRUD
	searcher ChunkSearcher      // 读路径: 向量搜索 + 关键词搜索
	embedder hexagon.VectorEmbedder    // hexagon/ai-core 向量嵌入（可为 nil）
	splitter hexagon.Splitter       // hexagon 文本分块器
	config   HybridConfig
}

// ManagerOption Manager 配置选项
type ManagerOption func(*Manager)

// WithHybridConfig 设置混合检索配置
func WithHybridConfig(cfg HybridConfig) ManagerOption {
	return func(m *Manager) { m.config = cfg }
}

// WithSplitter 设置文本分块器（hexagon hexagon.Splitter）
func WithSplitter(s hexagon.Splitter) ManagerOption {
	return func(m *Manager) { m.splitter = s }
}

// NewManager 创建知识库管理器
//
// repo 和 searcher 通常由同一个 SQLiteStore 实例同时实现。
// embedder 可为 nil，此时退化为纯关键词搜索模式。
func NewManager(repo DocumentRepository, searcher ChunkSearcher, embedder hexagon.VectorEmbedder, opts ...ManagerOption) *Manager {
	m := &Manager{
		repo:     repo,
		searcher: searcher,
		embedder: embedder,
		config:   DefaultHybridConfig(),
	}
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
	if content == "" {
		return nil, fmt.Errorf("文档内容不能为空")
	}

	now := time.Now()
	doc := &Document{
		ID:         "doc-" + idgen.ShortID(),
		Title:      title,
		Content:    content,
		Source:     source,
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     "indexed",
		SourceType: sourceTypeFromSource(source),
	}

	chunks, err := m.buildChunks(ctx, doc, now)
	if err != nil {
		return nil, err
	}

	if err := m.repo.Add(ctx, doc, chunks); err != nil {
		return nil, fmt.Errorf("保存文档失败: %w", err)
	}
	return doc, nil
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

// ─── Query Methods (读路径) ─────────────────────────────

// Search 返回结构化搜索结果，供 API/UI 展示
func (m *Manager) Search(ctx context.Context, query string, topK int) ([]SearchHit, error) {
	selected, err := m.searchResults(ctx, query, topK)
	if err != nil {
		return nil, err
	}

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
		})
	}
	return hits, nil
}

// Query 混合检索知识库，返回格式化的 LLM 上下文
func (m *Manager) Query(ctx context.Context, query string, topK int) (string, error) {
	hits, err := m.Search(ctx, query, topK)
	if err != nil {
		return "", err
	}
	return formatSearchHits(hits), nil
}

func (m *Manager) searchResults(ctx context.Context, query string, topK int) ([]*SearchResult, error) {
	if topK <= 0 {
		topK = 3
	}

	candidateK := topK * 3
	if candidateK < 10 {
		candidateK = 10
	}

	resultMap := make(map[string]*SearchResult)

	// 1. 向量搜索（读路径）
	if m.embedder != nil {
		queryVecs, embedErr := m.embedder.Embed(ctx, []string{query})
		if embedErr != nil {
			log.Printf("[knowledge] 查询向量嵌入失败: %v", embedErr)
		} else if len(queryVecs) > 0 {
			vectorResults, vecErr := m.searcher.VectorSearch(ctx, queryVecs[0], candidateK)
			if vecErr != nil {
				log.Printf("[knowledge] 向量搜索失败: %v", vecErr)
			} else {
				for _, r := range vectorResults {
					resultMap[r.Chunk.ID] = r
				}
			}
		}
	}

	// 2. FTS5 关键词搜索（读路径）
	textResults, textErr := m.searcher.TextSearch(ctx, query, candidateK)
	if textErr != nil {
		log.Printf("[knowledge] 关键词搜索失败: %v", textErr)
	} else {
		for _, r := range textResults {
			if existing, ok := resultMap[r.Chunk.ID]; ok {
				existing.TextScore = r.TextScore
			} else {
				resultMap[r.Chunk.ID] = r
			}
		}
	}

	if len(resultMap) == 0 {
		return nil, nil
	}

	// 3. 混合评分 + 时间衰减
	candidates := make([]*SearchResult, 0, len(resultMap))
	for _, r := range resultMap {
		r.Chunk.Score = m.hybridScore(r)
		candidates = append(candidates, r)
	}

	// 4. MMR 去重选取
	return m.mmrSelect(candidates, topK), nil
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

	chunkTexts := make([]string, len(ragDocs))
	for i, d := range ragDocs {
		chunkTexts[i] = d.Content
	}

	var embeddings [][]float32
	if m.embedder != nil && len(chunkTexts) > 0 {
		embeddings, err = m.embedder.Embed(ctx, chunkTexts)
		if err != nil {
			return nil, fmt.Errorf("生成向量嵌入失败: %w", err)
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

func (m *Manager) hybridScore(r *SearchResult) float64 {
	vectorWeight := m.config.VectorWeight
	textWeight := m.config.TextWeight
	if m.embedder == nil {
		vectorWeight = 0
		textWeight = 1.0
	}
	score := vectorWeight*r.VectorScore + textWeight*r.TextScore
	if m.config.TimeDecayDays > 0 {
		age := time.Since(r.Chunk.CreatedAt).Hours() / 24
		lambda := math.Ln2 / float64(m.config.TimeDecayDays)
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

	lambda := m.config.MMRLambda
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
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		return "url"
	default:
		return "file"
	}
}
