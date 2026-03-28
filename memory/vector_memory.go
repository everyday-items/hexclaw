package memory

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexagon"
)

// VectorMemory 基于向量存储的语义记忆
//
// 复用 hexagon 框架的向量存储（Milvus/Weaviate/Qdrant 等），
// 提供语义相似度搜索能力，增强记忆检索的智能性。
//
// 4 层记忆架构：
//  1. SessionContext - 会话上下文（内存，随会话结束释放）
//  2. DailyDiary    - 每日日记（FileMemory 的 YYYY-MM-DD.md）
//  3. LongTerm      - 长期记忆（FileMemory 的 MEMORY.md）
//  4. SemanticVector - 向量语义记忆（本层，hexagon 向量存储）
type VectorMemory struct {
	mu       sync.RWMutex
	store    hexagon.VectorMemoryStoreInterface
	embedder hexagon.VectorEmbedder
	config   VectorMemoryConfig
}

// VectorMemoryConfig 向量记忆配置
type VectorMemoryConfig struct {
	Enabled    bool    `yaml:"enabled"`
	TopK       int     `yaml:"top_k"`        // 语义搜索返回的最大条目数，默认 5
	MinScore   float32 `yaml:"min_score"`     // 最低相似度阈值，默认 0.7
	Collection string  `yaml:"collection"`    // 向量集合名称
	AutoSave   bool    `yaml:"auto_save"`     // 是否自动将对话保存到向量库
}

// NewVectorMemory 创建向量语义记忆
//
// store: hexagon 向量存储实例（Milvus/Weaviate/Qdrant/Memory 等）
// embedder: 向量生成器（将文本转为向量）
func NewVectorMemory(store hexagon.VectorMemoryStoreInterface, embedder hexagon.VectorEmbedder, cfg VectorMemoryConfig) *VectorMemory {
	if cfg.TopK <= 0 {
		cfg.TopK = 5
	}
	if cfg.MinScore <= 0 {
		cfg.MinScore = 0.7
	}
	return &VectorMemory{
		store:    store,
		embedder: embedder,
		config:   cfg,
	}
}

// Save 保存记忆到向量存储
//
// 将文本内容转为向量并存入向量数据库。
// metadata 可携带额外信息（如来源、时间、类型等）。
func (vm *VectorMemory) Save(ctx context.Context, content string, metadata map[string]any) error {
	if content == "" {
		return nil
	}

	// 在锁外执行 HTTP 调用（embedding 可能耗时数秒）
	embedding, err := vm.embedder.EmbedOne(ctx, content)
	if err != nil {
		return fmt.Errorf("生成向量失败: %w", err)
	}

	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["saved_at"] = time.Now().Format(time.RFC3339)

	doc := hexagon.VectorDocument{
		ID:        generateMemoryID(),
		Content:   content,
		Embedding: embedding,
		Metadata:  metadata,
	}

	// 仅在写存储时加锁
	vm.mu.Lock()
	defer vm.mu.Unlock()

	if err := vm.store.Add(ctx, []hexagon.VectorDocument{doc}); err != nil {
		return fmt.Errorf("保存向量记忆失败: %w", err)
	}

	return nil
}

// Search 语义搜索记忆
//
// 将查询文本转为向量，在向量库中搜索最相似的记忆。
// 返回按相似度降序排列的结果。
func (vm *VectorMemory) Search(ctx context.Context, query string, topK int) ([]VectorSearchResult, error) {
	if query == "" {
		return nil, nil
	}

	if topK <= 0 {
		topK = vm.config.TopK
	}

	// 在锁外执行 HTTP 调用（embedding 可能耗时数秒）
	queryEmbedding, err := vm.embedder.EmbedOne(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("生成查询向量失败: %w", err)
	}

	// 仅在读存储时加锁
	vm.mu.RLock()
	defer vm.mu.RUnlock()

	// 向量搜索
	docs, err := vm.store.Search(ctx, queryEmbedding, topK, hexagon.WithVectorMinScore(vm.config.MinScore))
	if err != nil {
		return nil, fmt.Errorf("向量搜索失败: %w", err)
	}

	results := make([]VectorSearchResult, 0, len(docs))
	for _, doc := range docs {
		results = append(results, VectorSearchResult{
			ID:       doc.ID,
			Content:  doc.Content,
			Score:    doc.Score,
			Metadata: doc.Metadata,
		})
	}

	return results, nil
}

// Delete 删除记忆
func (vm *VectorMemory) Delete(ctx context.Context, ids []string) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.store.Delete(ctx, ids)
}

// Count 返回记忆总数
func (vm *VectorMemory) Count(ctx context.Context) (int, error) {
	vm.mu.RLock()
	defer vm.mu.RUnlock()
	return vm.store.Count(ctx)
}

// Close 关闭向量存储连接
func (vm *VectorMemory) Close() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.store.Close()
}

// VectorSearchResult 向量搜索结果
type VectorSearchResult struct {
	ID       string         `json:"id"`
	Content  string         `json:"content"`
	Score    float32        `json:"score"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ============== 4 层记忆管理器 ==============

// LayeredMemory 4 层记忆管理器
//
// 整合会话上下文、每日日记、长期记忆和向量语义记忆，
// 提供统一的记忆读写和搜索接口。
type LayeredMemory struct {
	session *SessionContext // 第 1 层：会话上下文
	file    *FileMemory    // 第 2/3 层：日记 + 长期记忆
	vector  *VectorMemory  // 第 4 层：语义向量记忆（可选）
}

// NewLayeredMemory 创建 4 层记忆管理器
//
// vector 可为 nil，此时仅使用前 3 层。
func NewLayeredMemory(file *FileMemory, vectorMem *VectorMemory) *LayeredMemory {
	return &LayeredMemory{
		session: NewSessionContext(),
		file:    file,
		vector:  vectorMem,
	}
}

// LoadContext 加载所有层的记忆上下文
//
// 返回拼接的记忆文本，用于注入 system prompt。
func (lm *LayeredMemory) LoadContext() string {
	var sb strings.Builder

	// 第 1 层：会话上下文
	sessionCtx := lm.session.Summary()
	if sessionCtx != "" {
		sb.WriteString("## 当前会话上下文\n\n")
		sb.WriteString(sessionCtx)
		sb.WriteString("\n\n")
	}

	// 第 2/3 层：文件记忆（日记 + 长期）
	if lm.file != nil {
		fileCtx := lm.file.LoadContext()
		if fileCtx != "" {
			sb.WriteString(fileCtx)
		}
	}

	return sb.String()
}

// SemanticSearch 语义搜索（跨所有层）
//
// 先搜索向量记忆，再搜索文件记忆，合并去重。
func (lm *LayeredMemory) SemanticSearch(ctx context.Context, query string, topK int) []VectorSearchResult {
	var results []VectorSearchResult

	// 向量语义搜索
	if lm.vector != nil {
		vResults, err := lm.vector.Search(ctx, query, topK)
		if err != nil {
			log.Printf("向量搜索失败: %v", err)
		} else {
			results = append(results, vResults...)
		}
	}

	// 文件记忆关键词搜索
	if lm.file != nil {
		fResults := lm.file.Search(query)
		for _, r := range fResults {
			results = append(results, VectorSearchResult{
				ID:      fmt.Sprintf("%s:%d", r.File, r.Line),
				Content: r.Content,
				Score:   float32(r.Score * 0.8), // 关键词匹配权重稍低
			})
		}
	}

	return results
}

// SaveToSession 保存到会话上下文
func (lm *LayeredMemory) SaveToSession(key, value string) {
	lm.session.Set(key, value)
}

// SaveToLongTerm 保存到长期记忆
func (lm *LayeredMemory) SaveToLongTerm(content string) error {
	if lm.file == nil {
		return fmt.Errorf("文件记忆未初始化")
	}
	return lm.file.SaveMemory(content)
}

// SaveToVector 保存到向量记忆
func (lm *LayeredMemory) SaveToVector(ctx context.Context, content string, metadata map[string]any) error {
	if lm.vector == nil {
		return fmt.Errorf("向量记忆未初始化")
	}
	return lm.vector.Save(ctx, content, metadata)
}

// Session 返回会话上下文
func (lm *LayeredMemory) Session() *SessionContext { return lm.session }

// File 返回文件记忆
func (lm *LayeredMemory) File() *FileMemory { return lm.file }

// Vector 返回向量记忆
func (lm *LayeredMemory) Vector() *VectorMemory { return lm.vector }

// ============== 会话上下文 ==============

// SessionContext 会话级上下文（第 1 层）
//
// 存储当前会话的临时上下文，会话结束后释放。
type SessionContext struct {
	mu    sync.RWMutex
	data  map[string]string
	turns int
}

// NewSessionContext 创建会话上下文
func NewSessionContext() *SessionContext {
	return &SessionContext{
		data: make(map[string]string),
	}
}

// Set 设置上下文
func (sc *SessionContext) Set(key, value string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.data[key] = value
}

// Get 获取上下文
func (sc *SessionContext) Get(key string) string {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.data[key]
}

// IncrTurns 增加对话轮数
func (sc *SessionContext) IncrTurns() int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.turns++
	return sc.turns
}

// Turns 返回当前对话轮数
func (sc *SessionContext) Turns() int {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.turns
}

// Summary 返回上下文摘要
func (sc *SessionContext) Summary() string {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	if len(sc.data) == 0 {
		return ""
	}
	var sb strings.Builder
	for k, v := range sc.data {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", k, v))
	}
	return sb.String()
}

// Clear 清空上下文
func (sc *SessionContext) Clear() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.data = make(map[string]string)
	sc.turns = 0
}

// generateMemoryID 生成唯一记忆 ID
func generateMemoryID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 在正常系统上不会失败，panic 是合理的
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	return fmt.Sprintf("mem_%x", b)
}
