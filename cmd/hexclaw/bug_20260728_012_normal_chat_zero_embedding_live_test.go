package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/localinfer"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

const (
	bug20260728012NormalChatGate      = "HEX_SEMANTIC_LIVE_NORMAL_CHAT_ZERO_EMBEDDING"
	bug20260728012NormalChatModel     = "qwen3.5:9b"
	bug20260728012NormalChatTimeout   = 6 * time.Minute
	bug20260728012NormalChatMaxTokens = "64"
)

// TestBUG20260728012NormalChatZeroEmbeddingWithLocalOllama 验证普通聊天生产入口不会
// 把非知识问题送入语义检索。测试先发布一个真实 qwen3-embedding:8b revision，避免
// 因知识库未接线或索引不可用而得到假绿；随后只执行一次真实 qwen3.5:9b 普通聊天。
// 默认跳过，不启动、拉取或删除 Ollama 模型。
func TestBUG20260728012NormalChatZeroEmbeddingWithLocalOllama(t *testing.T) {
	if strings.TrimSpace(os.Getenv(bug20260728012NormalChatGate)) != "1" {
		t.Skip("set HEX_SEMANTIC_LIVE_NORMAL_CHAT_ZERO_EMBEDDING=1 to run the isolated real local normal-chat gate")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	upstream := bug20260728012LocalOllamaURL(t)
	observer := newBUG20260728012OllamaHTTPObserver(t, upstream)
	t.Setenv(semanticLiveLocalBaseURLEnv, observer.URL())
	t.Setenv(semanticLiveLocalProviderEnv, "")
	t.Setenv(semanticLiveLocalModelEnv, semanticLiveDefaultLocalModel)

	ctx, cancel := context.WithTimeout(context.Background(), bug20260728012NormalChatTimeout)
	defer cancel()

	cfg := config.DefaultConfig()
	cfg.Storage.SQLite.Path = filepath.Join(home, ".hexclaw", "normal-chat.db")
	cfg.LLM.Cache.Enabled = false
	cfg.Knowledge.QueryExpand = false
	cfg.Knowledge.Rerank = false
	cfg.Knowledge.Contextual = false
	cfg.FileMemory.Enabled = false
	cfg.Memory.LongTerm.Enabled = false
	cfg.Memory.Vector.Enabled = false
	cfg.Compaction.Enabled = false

	governor, err := newProcessResourceGovernor(cfg.ResourceGovernor)
	if err != nil {
		t.Fatalf("create isolated resource governor: error_type=%T", err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)

	laneCfg, plan, localConfig := semanticLiveLocalPlan(t, ctx, cfg)
	if !plan.Ollama || plan.Model != semanticLiveDefaultLocalModel {
		t.Fatalf("embedding route model=%q ollama=%t, want qwen3-embedding:8b/local", plan.Model, plan.Ollama)
	}

	knowledgeDB, semanticRuntime, embeddingCounter, _ := semanticRAGDialogueBuildCorpus(
		t, ctx, laneCfg, plan, coordinator,
	)
	knowledgeStore := knowledge.NewSQLiteStore(knowledgeDB)
	hybrid := semanticRAGDialogueHybridConfig(laneCfg, plan.Model)
	hybrid.ExpandEnabled = false
	hybrid.RerankEnabled = false
	knowledgeManager := knowledge.NewManager(
		knowledgeStore,
		knowledgeStore,
		nil,
		knowledge.WithHybridConfig(hybrid),
		knowledge.WithRevisionSemanticSearcher(semanticRuntime.Searcher),
		knowledge.WithLocalInferenceCoordinator(coordinator),
	)

	chatProviderName := plan.Provider
	localConfig.Model = bug20260728012NormalChatModel
	localConfig.Models = []string{bug20260728012NormalChatModel}
	localConfig.ModelSpecsMode = config.LLMModelSpecsModeLegacy
	localConfig.ModelSpecs = nil
	localConfig.Locality = config.ProviderLocalityLocal
	localConfig.BaseURL = observer.URL()
	localConfig.NumCtx = 4096
	toolsDisabled := false
	localConfig.ToolsEnabled = &toolsDisabled
	cfg.LLM = config.LLMConfig{
		Default: chatProviderName,
		Providers: map[string]config.LLMProviderConfig{
			chatProviderName: localConfig,
		},
		Routing: config.LLMRoutingConfig{Enabled: false},
		Cache:   config.LLMCacheConfig{Enabled: false},
	}

	chatProvider := llmrouter.NewProviderFromConfig(chatProviderName, localConfig)
	router := llmrouter.NewWithProviders(
		cfg.LLM,
		map[string]hexagon.Provider{chatProviderName: chatProvider},
	)
	router.SetEgressPolicy(&egress.Policy{})
	router.SetLocalInferenceCoordinator(coordinator)

	messageStore, err := sqlitestore.New(cfg.Storage.SQLite.Path)
	if err != nil {
		t.Fatalf("open isolated chat SQLite: error_type=%T", err)
	}
	t.Cleanup(func() { _ = messageStore.Close() })
	if err := messageStore.Init(ctx); err != nil {
		t.Fatalf("initialize isolated chat SQLite: error_type=%T", err)
	}

	chatEngine := engine.NewReActEngine(cfg, router, messageStore, skill.NewRegistry())
	chatEngine.SetKnowledgeBase(knowledgeManager)

	embeddingBefore := observer.EmbeddingRequests()
	semanticEmbeddingBefore := len(embeddingCounter.snapshot())
	chatBefore := observer.ChatRequests()
	started := time.Now()
	reply, processErr := chatEngine.Process(ctx, &adapter.Message{
		ID:        "bug-20260728-012-normal-chat-message",
		Platform:  adapter.PlatformAPI,
		UserID:    "bug-20260728-012-isolated-user",
		ChatID:    "bug-20260728-012-isolated-chat",
		SessionID: "bug-20260728-012-isolated-session",
		Content:   "请只回答“今天也要轻松一点”，不要解释，也不要查询或引用任何资料。",
		Timestamp: time.Now(),
		Metadata: map[string]string{
			"provider":   chatProviderName,
			"model":      bug20260728012NormalChatModel,
			"thinking":   "off",
			"memory":     "off",
			"max_tokens": bug20260728012NormalChatMaxTokens,
		},
	})
	elapsed := time.Since(started)
	if processErr != nil {
		t.Fatalf("production normal chat failed: error_type=%T elapsed=%s", processErr, elapsed.Round(time.Millisecond))
	}
	if reply == nil || strings.TrimSpace(reply.Content) == "" {
		t.Fatalf("production normal chat returned an empty reply after %s", elapsed.Round(time.Millisecond))
	}

	embeddingDelta := observer.EmbeddingRequests() - embeddingBefore
	semanticEmbeddingDelta := int64(len(embeddingCounter.snapshot()) - semanticEmbeddingBefore)
	chatDelta := observer.ChatRequests() - chatBefore
	if embeddingDelta != 0 || semanticEmbeddingDelta != 0 {
		t.Fatalf(
			"normal chat embedding request delta observer=%d semantic=%d, want strict 0/0",
			embeddingDelta,
			semanticEmbeddingDelta,
		)
	}
	if chatDelta < 1 {
		t.Fatalf("normal chat qwen3.5:9b HTTP request delta=%d, want at least 1", chatDelta)
	}
	if other := observer.OtherModelRequests(); other != 0 {
		t.Fatalf("normal chat observed %d embedding/chat requests with an unexpected model", other)
	}

	t.Logf(
		"BUG-20260728-012 normal chat passed: chat_requests=%d embedding_requests=0 elapsed=%s isolated_home=true isolated_sqlite=true",
		chatDelta,
		elapsed.Round(time.Millisecond),
	)
}

type bug20260728012OllamaHTTPObserver struct {
	server             *httptest.Server
	reverseProxy       *httputil.ReverseProxy
	embeddingRequests  atomic.Int64
	chatRequests       atomic.Int64
	otherModelRequests atomic.Int64
}

func newBUG20260728012OllamaHTTPObserver(
	t *testing.T,
	upstream *url.URL,
) *bug20260728012OllamaHTTPObserver {
	t.Helper()
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "local Ollama transport failed", http.StatusBadGateway)
	}
	observer := &bug20260728012OllamaHTTPObserver{reverseProxy: proxy}
	observer.server = httptest.NewServer(observer)
	t.Cleanup(observer.server.Close)
	return observer
}

func (o *bug20260728012OllamaHTTPObserver) URL() string {
	if o == nil || o.server == nil {
		return ""
	}
	return o.server.URL
}

func (o *bug20260728012OllamaHTTPObserver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			var payload struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(body, &payload) == nil {
				o.observe(r.URL.Path, strings.TrimSpace(payload.Model))
			}
		}
	}
	o.reverseProxy.ServeHTTP(w, r)
}

func (o *bug20260728012OllamaHTTPObserver) observe(path, model string) {
	lowerPath := strings.ToLower(strings.TrimSpace(path))
	switch {
	case strings.HasSuffix(lowerPath, "/embeddings") || strings.HasSuffix(lowerPath, "/api/embed"):
		if model == semanticLiveDefaultLocalModel {
			o.embeddingRequests.Add(1)
		} else {
			o.otherModelRequests.Add(1)
		}
	case strings.HasSuffix(lowerPath, "/chat/completions") || strings.HasSuffix(lowerPath, "/api/chat"):
		if model == bug20260728012NormalChatModel {
			o.chatRequests.Add(1)
		} else {
			o.otherModelRequests.Add(1)
		}
	}
}

func (o *bug20260728012OllamaHTTPObserver) EmbeddingRequests() int64 {
	return o.embeddingRequests.Load()
}

func (o *bug20260728012OllamaHTTPObserver) ChatRequests() int64 {
	return o.chatRequests.Load()
}

func (o *bug20260728012OllamaHTTPObserver) OtherModelRequests() int64 {
	return o.otherModelRequests.Load()
}

func bug20260728012LocalOllamaURL(t *testing.T) *url.URL {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(semanticLiveLocalBaseURLEnv))
	if raw == "" {
		raw = "http://127.0.0.1:11434"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		t.Fatal("local Ollama base URL must be a valid loopback HTTP URL")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			t.Fatal("local Ollama base URL must resolve to a loopback host")
		}
	}
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/v1")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed
}
