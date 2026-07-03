package engine

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// BUG-20260703 B8（引擎注入点集成锁）：knowledge 层的 fail-closed 严格地板必须在
// 引擎自动注入点（react.go 的 e.kb.Query → kbContext → LLM 请求）真实生效——
// 弱相关噪声文档（仅通用词法重叠、语义低于地板）绝不能出现在发给模型的消息里。
//
// knowledge 单元锁证明「QueryWithFilter 返回空」；本测试证明「返回空 ⇒ 模型看不到
// 乐知文档」这后半截缝，并用强命中对照证明断言有牙（相关文档时注入确实可见）。
func TestBug20260703_B8_EngineInjectionSkipsWeakNoise(t *testing.T) {
	const noiseDoc = "lezhi company intro: delivery teams in huzhou sanya ningde suzhou changchun hefei"
	const query = "jiuhe company address"

	capture := &capturingProviderB8{}
	eng, kb := newEngineWithKB(t, capture, map[string][]float32{
		query:    {1, 0, 0, 0},
		noiseDoc: {0, 1, 0, 0}, // 语义正交：cos=0 → 0.5 < 0.55 地板
	})
	if _, err := kb.AddDocument(context.Background(), "乐知新创公司介绍", noiseDoc, "test"); err != nil {
		t.Fatal(err)
	}

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-b8-noise", Platform: adapter.PlatformAPI, UserID: "u-1", SessionID: "sess-b8-noise",
		Content: query,
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reply == nil || reply.Content == "" {
		t.Fatal("应有 LLM 回复")
	}
	if capture.sawText("huzhou sanya") {
		t.Fatalf("B8: 弱相关噪声文档【正文】进入了发给模型的消息（串知识库复发）")
	}
	if capture.sawText("乐知新创") {
		t.Logf("注：文档标题出现在能力上下文（[你的知识库] 文档清单，设计内行为，非检索注入）")
	}
	if capture.sawText("以下是从个人知识库中检索到的相关信息") {
		t.Fatalf("B8: 无强命中时不应有任何知识库注入前导")
	}
}

func TestBug20260703_B8_EngineInjectionCarriesStrongHit(t *testing.T) {
	const relevantDoc = "jiuhe company address: hangzhou west lake district cloud town"
	const query = "jiuhe company address"

	capture := &capturingProviderB8{}
	eng, kb := newEngineWithKB(t, capture, map[string][]float32{
		query:       {1, 0, 0, 0},
		relevantDoc: {1, 0, 0, 0}, // cos=1 → 过地板
	})
	if _, err := kb.AddDocument(context.Background(), "九河科技介绍", relevantDoc, "test"); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-b8-hit", Platform: adapter.PlatformAPI, UserID: "u-1", SessionID: "sess-b8-hit",
		Content: query,
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// 对照（断言有牙）：强命中时注入内容必须真的到达模型。
	if !capture.sawText("hangzhou west lake district") {
		t.Fatalf("B8 对照: 强语义命中未注入进模型消息——注入链断了")
	}
}

// ── 测试基建 ──

type capturingProviderB8 struct {
	mu   sync.Mutex
	seen []string
}

func (p *capturingProviderB8) Name() string { return "test" }

func (p *capturingProviderB8) Complete(_ context.Context, req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	p.mu.Lock()
	for _, m := range req.Messages {
		p.seen = append(p.seen, m.Content)
	}
	p.mu.Unlock()
	return &hexagon.CompletionResponse{Content: "好的，我来回答。", Usage: hexagon.Usage{TotalTokens: 6}}, nil
}

func (p *capturingProviderB8) Stream(ctx context.Context, req hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	// Process 走 Complete；Stream 不应被本测试触发，但按接口要求提供。
	resp, _ := p.Complete(ctx, req)
	return nil, contextErrB8(resp)
}

func contextErrB8(*hexagon.CompletionResponse) error { return context.Canceled }

func (p *capturingProviderB8) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock Model"}}
}

func (p *capturingProviderB8) CountTokens([]llm.Message) (int, error) { return 0, nil }

func (p *capturingProviderB8) sawText(sub string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.seen {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// b8EngineEmbedder 工程化控制语义相关度：按「包含匹配」返回预置向量，未命中返回
// 与查询基正交的向量（与 knowledge 包 scriptedEmbedder 同思路，包含匹配抗分块微扰）。
type b8EngineEmbedder struct {
	vecs map[string][]float32
}

func (e *b8EngineEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.lookup(t)
	}
	return out, nil
}

func (e *b8EngineEmbedder) EmbedOne(_ context.Context, t string) ([]float32, error) {
	return e.lookup(t), nil
}

func (e *b8EngineEmbedder) Dimension() int { return 4 }

func (e *b8EngineEmbedder) lookup(t string) []float32 {
	for key, v := range e.vecs {
		if strings.Contains(t, key) || strings.Contains(key, t) {
			return v
		}
	}
	return []float32{0, 0, 0, 1}
}

func newEngineWithKB(t *testing.T, provider hexagon.Provider, vecs map[string][]float32) (*ReActEngine, *knowledge.Manager) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	kbStore := knowledge.NewSQLiteStore(store.DB())
	if err := kbStore.Init(context.Background()); err != nil {
		t.Fatalf("初始化知识库存储失败: %v", err)
	}
	kb := knowledge.NewManager(kbStore, kbStore, &b8EngineEmbedder{vecs: vecs},
		knowledge.WithSplitter(splitter.NewRecursiveSplitter(
			splitter.WithRecursiveChunkSize(400),
			splitter.WithRecursiveChunkOverlap(80),
		)),
		knowledge.WithHybridConfig(knowledge.HybridConfig{
			VectorWeight: 0.7, TextWeight: 0.3, MMRLambda: 0.7, TimeDecayDays: 0,
			MinScore: 0.55, CandidateK: 50, RRFK: 60, UseRRF: true,
		}))

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": provider})
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	eng.SetKnowledgeBase(kb)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng, kb
}
