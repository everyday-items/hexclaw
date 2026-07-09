package engine

// BUG-20260710：qwen3.5 走应用慢（冷 364s）vs 直连快（5s）——根因 = 巨型 prompt（~7.9k token）
// × 纯 CPU prefill 23 tok/s，且 Ollama 默认 5 分钟卸载模型丢 KV 缓存。
// 本文件锁「启动预热」契约：默认路由为本地模型时，WarmupLocalDefaultModel 发送一次
// 与真实聊天同构（system prompt 非空 + num_predict=1 + keep_alive 下发）的请求；
// 默认路由非本地时跳过（云端无预热需求，不烧钱）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type warmupCapture struct {
	calls      atomic.Int32
	numPredict atomic.Int64
	keepAlive  atomic.Value // string
	sysChars   atomic.Int64
}

func newFakeOllamaServer(cap *warmupCapture) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		cap.calls.Add(1)
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Options   map[string]any `json:"options"`
			KeepAlive string         `json:"keep_alive"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Options != nil {
			if np, ok := body.Options["num_predict"].(float64); ok {
				cap.numPredict.Store(int64(np))
			}
		}
		cap.keepAlive.Store(body.KeepAlive)
		for _, m := range body.Messages {
			if m.Role == "system" {
				cap.sysChars.Store(int64(len(m.Content)))
				break
			}
		}
		// 预热走流式（BUG-20260710：非流式 120s 头超时 < CPU prefill 344s）→ NDJSON 两行
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"model":"qwen3.5:9b","message":{"role":"assistant","content":"o"},"done":false}` + "\n"))
		_, _ = w.Write([]byte(`{"model":"qwen3.5:9b","message":{"role":"assistant","content":""},"done":true}` + "\n"))
	}))
}

func newWarmupTestEngine(t *testing.T, providers map[string]config.LLMProviderConfig, def string) *ReActEngine {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = providers
	cfg.LLM.Default = def
	cfg.LLM.Routing.Enabled = false
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("llmrouter: %v", err)
	}
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "warmup.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_ = store.Init(context.Background())
	return NewReActEngine(cfg, router, store, skill.NewRegistry())
}

func TestBUG20260710_Warmup_LocalDefault_PrefillsRealPrefix(t *testing.T) {
	var cap warmupCapture
	srv := newFakeOllamaServer(&cap)
	defer srv.Close()

	eng := newWarmupTestEngine(t, map[string]config.LLMProviderConfig{
		"Ollama (本地)": {BaseURL: srv.URL + "/v1", Model: "qwen3.5:9b"},
	}, "Ollama (本地)")

	warmed, err := eng.WarmupLocalDefaultModel(context.Background())
	if err != nil {
		t.Fatalf("预热不应报错: %v", err)
	}
	if !warmed {
		t.Fatal("默认路由是本地 ollama，应执行预热")
	}
	if got := cap.calls.Load(); got != 1 {
		t.Fatalf("应恰好发 1 次预热请求，实际 %d", got)
	}
	if got := cap.numPredict.Load(); got != 1 {
		t.Fatalf("预热应 num_predict=1（只 prefill 不产出），实际 %d", got)
	}
	if ka, _ := cap.keepAlive.Load().(string); ka == "" {
		t.Fatal("预热请求应携带 keep_alive（否则 5 分钟后模型卸载、缓存白热）")
	}
	// system prompt 是被预热前缀的大头——必须与真实聊天同构（非空且有体量）
	if got := cap.sysChars.Load(); got < 200 {
		t.Fatalf("预热请求 system prompt 过小（%d 字符），未与真实聊天同构", got)
	}
}

func TestBUG20260710_Warmup_NonLocalDefault_Skips(t *testing.T) {
	var cap warmupCapture
	srv := newFakeOllamaServer(&cap)
	defer srv.Close()

	eng := newWarmupTestEngine(t, map[string]config.LLMProviderConfig{
		"cloud": {APIKey: "sk-test", BaseURL: srv.URL, Model: "m1"},
	}, "cloud")

	warmed, err := eng.WarmupLocalDefaultModel(context.Background())
	if err != nil {
		t.Fatalf("非本地默认路由不应报错: %v", err)
	}
	if warmed {
		t.Fatal("默认路由非本地模型应跳过预热（云端预热=白烧钱）")
	}
	if got := cap.calls.Load(); got != 0 {
		t.Fatalf("跳过预热不应有任何 LLM 调用，实际 %d", got)
	}
}
