// Package knowledge 提供个人知识库管理
//
// 支持向量搜索 + FTS5 关键词搜索的混合检索模式：
//   - 向量搜索：通过 Embedding 生成向量，余弦相似度匹配语义
//   - FTS5 搜索：SQLite FTS5 全文索引，BM25 算法匹配关键词
//   - 混合评分：vectorWeight * vectorScore + textWeight * bm25Score
//   - MMR 去重：Maximal Marginal Relevance，平衡相关性与多样性
//   - 时间衰减：指数衰减，近期文档权重更高
//
// 文档上传后自动分块（带重叠）、生成向量、建立 FTS5 索引。
// 查询时同时走向量和关键词两条路径，合并后通过 MMR 选取最终结果。
package knowledge

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// Embedder 向量嵌入接口
//
// 将文本转换为向量表示，用于语义相似度搜索。
// 实现者可以使用 OpenAI、DeepSeek 等 API 生成 embedding。
type Embedder interface {
	// Embed 将一组文本转换为向量
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimension 返回向量维度
	Dimension() int
}

// Document 文档
type Document struct {
	ID         string    `json:"id"`          // 文档 ID
	Title      string    `json:"title"`       // 文档标题
	Content    string    `json:"content"`     // 原始内容
	Source     string    `json:"source"`      // 来源（文件路径/URL/手动输入）
	ChunkCount int      `json:"chunk_count"` // 分割后的 chunk 数
	CreatedAt  time.Time `json:"created_at"`  // 创建时间
}

// Chunk 文档片段
type Chunk struct {
	ID        string    `json:"id"`        // Chunk ID
	DocID     string    `json:"doc_id"`    // 所属文档 ID
	Content   string    `json:"content"`   // 片段内容
	Index     int       `json:"index"`     // 在文档中的序号
	Embedding []float32 `json:"-"`         // 向量嵌入（不序列化）
	Score     float64   `json:"score"`     // 最终综合分数
	CreatedAt time.Time `json:"created_at"`
}

// SearchResult 单条搜索结果（内部使用）
type SearchResult struct {
	Chunk       *Chunk
	VectorScore float64 // 向量余弦相似度 (0-1)
	TextScore   float64 // BM25 关键词匹配分数 (已归一化到 0-1)
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

// Store 知识库存储接口
type Store interface {
	// Init 初始化存储（创建表和 FTS5 索引）
	Init(ctx context.Context) error

	// AddDocument 添加文档及其 chunk（含向量）
	AddDocument(ctx context.Context, doc *Document, chunks []*Chunk) error

	// DeleteDocument 删除文档及其所有 chunk
	DeleteDocument(ctx context.Context, docID string) error

	// ListDocuments 列出所有文档
	ListDocuments(ctx context.Context) ([]*Document, error)

	// VectorSearch 向量搜索：返回余弦相似度最高的 chunk
	VectorSearch(ctx context.Context, queryVec []float32, topK int) ([]*SearchResult, error)

	// TextSearch FTS5 关键词搜索：返回 BM25 分数最高的 chunk
	TextSearch(ctx context.Context, query string, topK int) ([]*SearchResult, error)
}

// Manager 知识库管理器
//
// 负责文档管理和混合检索，为 Agent 引擎提供 RAG 上下文增强。
// 内部协调 Embedder 和 Store 完成向量+关键词的混合检索。
type Manager struct {
	store    Store
	embedder Embedder     // 向量嵌入生成器
	config   HybridConfig // 混合检索配置
	chunkSize   int       // 每个 chunk 的最大字符数
	chunkOverlap int      // chunk 间的重叠字符数
}

// ManagerOption Manager 配置选项
type ManagerOption func(*Manager)

// WithChunkSize 设置 chunk 大小
func WithChunkSize(size int) ManagerOption {
	return func(m *Manager) { m.chunkSize = size }
}

// WithChunkOverlap 设置 chunk 重叠大小
func WithChunkOverlap(overlap int) ManagerOption {
	return func(m *Manager) { m.chunkOverlap = overlap }
}

// WithHybridConfig 设置混合检索配置
func WithHybridConfig(cfg HybridConfig) ManagerOption {
	return func(m *Manager) { m.config = cfg }
}

// NewManager 创建知识库管理器
//
// embedder 可为 nil，此时退化为纯关键词搜索模式。
func NewManager(store Store, embedder Embedder, opts ...ManagerOption) *Manager {
	m := &Manager{
		store:        store,
		embedder:     embedder,
		config:       DefaultHybridConfig(),
		chunkSize:    400,
		chunkOverlap: 80,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// AddDocument 添加文档到知识库
//
// 流程：分块（带重叠）→ 生成向量 → 存储到 SQLite + FTS5 索引
func (m *Manager) AddDocument(ctx context.Context, title, content, source string) (*Document, error) {
	if content == "" {
		return nil, fmt.Errorf("文档内容不能为空")
	}

	doc := &Document{
		ID:        fmt.Sprintf("doc-%d", time.Now().UnixNano()),
		Title:     title,
		Content:   content,
		Source:    source,
		CreatedAt: time.Now(),
	}

	// 分块（带重叠）
	chunkTexts := splitIntoChunks(content, m.chunkSize, m.chunkOverlap)
	doc.ChunkCount = len(chunkTexts)

	// 生成向量嵌入
	var embeddings [][]float32
	if m.embedder != nil && len(chunkTexts) > 0 {
		var err error
		embeddings, err = m.embedder.Embed(ctx, chunkTexts)
		if err != nil {
			return nil, fmt.Errorf("生成向量嵌入失败: %w", err)
		}
	}

	// 构建 Chunk 列表
	chunks := make([]*Chunk, len(chunkTexts))
	now := time.Now()
	for i, text := range chunkTexts {
		chunk := &Chunk{
			ID:        fmt.Sprintf("%s-chunk-%d", doc.ID, i),
			DocID:     doc.ID,
			Content:   text,
			Index:     i,
			CreatedAt: now,
		}
		if i < len(embeddings) {
			chunk.Embedding = embeddings[i]
		}
		chunks[i] = chunk
	}

	if err := m.store.AddDocument(ctx, doc, chunks); err != nil {
		return nil, fmt.Errorf("保存文档失败: %w", err)
	}

	return doc, nil
}

// Query 混合检索知识库，返回格式化的 LLM 上下文
//
// 执行流程：
//  1. 生成查询向量
//  2. 并行执行向量搜索和关键词搜索
//  3. 合并结果，计算混合分数
//  4. 应用时间衰减
//  5. MMR 去重选取最终结果
//  6. 格式化为 LLM 可用的上下文字符串
func (m *Manager) Query(ctx context.Context, query string, topK int) (string, error) {
	if topK <= 0 {
		topK = 3
	}

	// 候选数量：取更多候选以便 MMR 筛选
	candidateK := topK * 3
	if candidateK < 10 {
		candidateK = 10
	}

	// 收集搜索结果
	resultMap := make(map[string]*SearchResult) // chunkID -> result

	// 1. 向量搜索
	if m.embedder != nil {
		queryVecs, err := m.embedder.Embed(ctx, []string{query})
		if err == nil && len(queryVecs) > 0 {
			vectorResults, err := m.store.VectorSearch(ctx, queryVecs[0], candidateK)
			if err == nil {
				for _, r := range vectorResults {
					resultMap[r.Chunk.ID] = r
				}
			}
		}
	}

	// 2. FTS5 关键词搜索
	textResults, err := m.store.TextSearch(ctx, query, candidateK)
	if err == nil {
		for _, r := range textResults {
			if existing, ok := resultMap[r.Chunk.ID]; ok {
				// 合并分数
				existing.TextScore = r.TextScore
			} else {
				resultMap[r.Chunk.ID] = r
			}
		}
	}

	if len(resultMap) == 0 {
		return "", nil
	}

	// 3. 计算混合分数 + 时间衰减
	candidates := make([]*SearchResult, 0, len(resultMap))
	for _, r := range resultMap {
		r.Chunk.Score = m.hybridScore(r)
		candidates = append(candidates, r)
	}

	// 4. MMR 去重选取
	selected := m.mmrSelect(candidates, topK)

	if len(selected) == 0 {
		return "", nil
	}

	// 5. 格式化输出
	var sb strings.Builder
	sb.WriteString("以下是从个人知识库中检索到的相关信息：\n\n")
	for i, r := range selected {
		sb.WriteString(fmt.Sprintf("--- 参考 %d (相关度: %.0f%%) ---\n%s\n\n",
			i+1, r.Chunk.Score*100, r.Chunk.Content))
	}
	sb.WriteString("请基于以上参考信息回答用户的问题。如果参考信息不足以回答，请如实告知。\n")

	return sb.String(), nil
}

// DeleteDocument 删除文档
func (m *Manager) DeleteDocument(ctx context.Context, docID string) error {
	return m.store.DeleteDocument(ctx, docID)
}

// ListDocuments 列出所有文档
func (m *Manager) ListDocuments(ctx context.Context) ([]*Document, error) {
	return m.store.ListDocuments(ctx)
}

// hybridScore 计算混合分数（向量 + 关键词 + 时间衰减）
func (m *Manager) hybridScore(r *SearchResult) float64 {
	// 如果没有 Embedder，将全部权重给关键词搜索
	vectorWeight := m.config.VectorWeight
	textWeight := m.config.TextWeight
	if m.embedder == nil {
		vectorWeight = 0
		textWeight = 1.0
	}

	// 混合分数
	score := vectorWeight*r.VectorScore + textWeight*r.TextScore

	// 时间衰减：score * e^(-lambda * ageInDays)
	if m.config.TimeDecayDays > 0 {
		age := time.Since(r.Chunk.CreatedAt).Hours() / 24
		lambda := math.Ln2 / float64(m.config.TimeDecayDays) // 半衰期
		score *= math.Exp(-lambda * age)
	}

	return score
}

// mmrSelect 使用 Maximal Marginal Relevance 选取多样化结果
//
// MMR = lambda * sim(d, q) - (1-lambda) * max(sim(d, d'))
// 在相关性和多样性之间取平衡。
func (m *Manager) mmrSelect(candidates []*SearchResult, topK int) []*SearchResult {
	if len(candidates) <= topK {
		// 不需要 MMR，直接按分数排序
		sortByScore(candidates)
		return candidates
	}

	// 如果没有向量，退化为按分数排序
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
			// 相关性部分
			relevance := cand.Chunk.Score

			// 多样性部分：与已选中结果的最大相似度
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
			// 从 remaining 中移除
			remaining[bestIdx] = remaining[len(remaining)-1]
			remaining = remaining[:len(remaining)-1]
		}
	}

	return selected
}

// sortByScore 按分数降序排序
func sortByScore(results []*SearchResult) {
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Chunk.Score > results[j-1].Chunk.Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
}

// cosineSimilarity 计算两个向量的余弦相似度
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}

	var dotProduct, normA, normB float64
	for i := range a {
		dotProduct += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

// --- 文本分块 ---

// splitIntoChunks 将文本分割为带重叠的 chunk
//
// 优先按段落分割，如果段落过长则按句子分割。
// overlap 参数控制相邻 chunk 之间的重叠字符数，
// 用于保持上下文连贯性。
func splitIntoChunks(text string, maxSize, overlap int) []string {
	if overlap < 0 {
		overlap = 0
	}

	// 按段落分割
	paragraphs := strings.Split(text, "\n\n")

	var chunks []string
	var current strings.Builder

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		// 当前 chunk 加上新段落会超过限制
		if current.Len()+len(para)+2 > maxSize && current.Len() > 0 {
			chunks = append(chunks, current.String())
			// 重叠处理：保留尾部 overlap 字符作为下一个 chunk 的开头
			tail := overlapTail(current.String(), overlap)
			current.Reset()
			if tail != "" {
				current.WriteString(tail)
			}
		}

		// 单个段落就超过限制，按句子分割
		if len(para) > maxSize {
			if current.Len() > 0 {
				chunks = append(chunks, current.String())
				tail := overlapTail(current.String(), overlap)
				current.Reset()
				if tail != "" {
					current.WriteString(tail)
				}
			}
			sentences := splitSentences(para)
			for _, sent := range sentences {
				if current.Len()+len(sent)+1 > maxSize && current.Len() > 0 {
					chunks = append(chunks, current.String())
					tail := overlapTail(current.String(), overlap)
					current.Reset()
					if tail != "" {
						current.WriteString(tail)
					}
				}
				if current.Len() > 0 {
					current.WriteString(" ")
				}
				current.WriteString(sent)
			}
		} else {
			if current.Len() > 0 {
				current.WriteString("\n\n")
			}
			current.WriteString(para)
		}
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

// overlapTail 获取字符串末尾 n 个字符作为重叠部分
func overlapTail(s string, n int) string {
	runes := []rune(s)
	if n <= 0 || len(runes) == 0 {
		return ""
	}
	if n > len(runes) {
		return s
	}
	return string(runes[len(runes)-n:])
}

// splitSentences 按句子分割
func splitSentences(text string) []string {
	var sentences []string
	var current strings.Builder

	for _, r := range text {
		current.WriteRune(r)
		if r == '。' || r == '？' || r == '！' || r == '.' || r == '?' || r == '!' {
			sentences = append(sentences, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		sentences = append(sentences, current.String())
	}
	return sentences
}
