package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/featureflag"
)

type mockCompletionProvider struct {
	err error
}

type semanticProviderARecorder struct {
	revoked bool
	calls   int
}

func (r *semanticProviderARecorder) call() {
	if !r.revoked {
		r.calls++
	}
}

type failingConfigApplier struct{ err error }

func (a failingConfigApplier) Apply(context.Context, *config.Config) (config.RollbackFn, error) {
	return nil, a.err
}

type mockEngineConfigApplier struct{ engine *mockEngine }

func (a mockEngineConfigApplier) Apply(ctx context.Context, candidate *config.Config) (config.RollbackFn, error) {
	previous := a.engine.ActiveLLMConfig()
	if err := a.engine.ReloadLLMConfig(ctx, candidate.LLM); err != nil {
		return nil, err
	}
	return func(rollbackCtx context.Context) error {
		return a.engine.ReloadLLMConfig(rollbackCtx, previous)
	}, nil
}

type configApplyContextObservation struct {
	canceled    bool
	hasDeadline bool
}

type contextCheckingEngineConfigApplier struct {
	engine       *mockEngine
	observations []configApplyContextObservation
}

func (a *contextCheckingEngineConfigApplier) Apply(ctx context.Context, candidate *config.Config) (config.RollbackFn, error) {
	_, hasDeadline := ctx.Deadline()
	a.observations = append(a.observations, configApplyContextObservation{
		canceled: ctx.Err() != nil, hasDeadline: hasDeadline,
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	previous := a.engine.ActiveLLMConfig()
	if err := a.engine.ReloadLLMConfig(ctx, candidate.LLM); err != nil {
		return nil, err
	}
	return func(rollbackCtx context.Context) error {
		return a.engine.ReloadLLMConfig(rollbackCtx, previous)
	}, nil
}

func (m *mockCompletionProvider) Complete(_ context.Context, _ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &hexagon.CompletionResponse{Content: "OK"}, nil
}

func TestHandleTestLLMConfig_Success(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(cfg llmConnectionTestProvider) completionProvider {
		if cfg.Type != "openai" || cfg.Model != "gpt-4o-mini" || cfg.APIKey != "sk-test" {
			t.Fatalf("工厂收到错误参数: %+v", cfg)
		}
		return &mockCompletionProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{"provider":{"type":"openai","api_key":"sk-test","model":"gpt-4o-mini"}}`))
	w := httptest.NewRecorder()

	srv.handleTestLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp LLMConnectionTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.OK {
		t.Fatalf("期望 ok=true，实际 %+v", resp)
	}
	if resp.Provider != "openai" || resp.Model != "gpt-4o-mini" {
		t.Fatalf("provider/model 不正确: %+v", resp)
	}
}

func TestHandleTestLLMConfig_ReturnsFailurePayload(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &mockCompletionProvider{err: errors.New("unauthorized")}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{"provider":{"type":"openai","api_key":"sk-test","model":"gpt-4o-mini"}}`))
	w := httptest.NewRecorder()

	srv.handleTestLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp LLMConnectionTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.OK {
		t.Fatalf("期望 ok=false，实际 %+v", resp)
	}
	if !strings.Contains(resp.Message, "unauthorized") {
		t.Fatalf("失败信息不正确: %+v", resp)
	}
}

func TestHandleTestLLMConfig_OllamaAllowsEmptyAPIKey(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(cfg llmConnectionTestProvider) completionProvider {
		if cfg.Type != "ollama" || cfg.Model != "llama3.1" || cfg.APIKey != "" {
			t.Fatalf("工厂收到错误参数: %+v", cfg)
		}
		return &mockCompletionProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{"provider":{"type":"ollama","base_url":"http://localhost:11434/v1","api_key":"","model":"llama3.1"}}`))
	w := httptest.NewRecorder()

	srv.handleTestLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp LLMConnectionTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.OK {
		t.Fatalf("期望 ok=true，实际 %+v", resp)
	}
}

func TestHandleTestLLMConfig_RejectsEmptyAPIKeyForOpenAI(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{"provider":{"type":"openai","api_key":"","model":"gpt-4o-mini"}}`))
	w := httptest.NewRecorder()

	srv.handleTestLLMConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetLLMConfig_UsesPersistedControlPlaneConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
	}

	runtimeCfg := config.LLMConfig{
		Default: "智谱",
		Providers: map[string]config.LLMProviderConfig{
			"智谱": {
				APIKey: "sk-zhipu", BaseURL: "https://open.bigmodel.cn/api/paas/v4",
				Model: "glm-5", KeepAlive: "15m", Locality: config.ProviderLocalityCloud, NumCtx: 8192,
			},
		},
	}

	srv := NewServer(cfg, &mockEngine{activeLLM: runtimeCfg}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil)
	w := httptest.NewRecorder()

	srv.handleGetLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp LLMConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if resp.Default != "openai" {
		t.Fatalf("期望默认 provider 为完整持久配置，实际 %q", resp.Default)
	}
	if _, ok := resp.Providers["openai"]; !ok {
		t.Fatalf("期望返回持久 provider，实际 %+v", resp.Providers)
	}
	if _, ok := resp.Providers["智谱"]; ok {
		t.Fatalf("运行时路由快照不应覆盖设置控制面，实际 %+v", resp.Providers)
	}
	var wire struct {
		Providers map[string]struct {
			KeepAlive string `json:"keep_alive"`
			Locality  string `json:"locality"`
			NumCtx    int    `json:"num_ctx"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
		t.Fatalf("解析 wire 响应失败: %v", err)
	}
	if wire.Providers["openai"].KeepAlive != "" {
		t.Fatalf("GET 持久配置 keep_alive 漂移，实际响应 %s", w.Body.String())
	}
	if wire.Providers["openai"].Locality != "" {
		t.Fatalf("GET 持久配置 locality 漂移，实际响应 %s", w.Body.String())
	}
	if wire.Providers["openai"].NumCtx != 0 {
		t.Fatalf("GET 持久配置 num_ctx 漂移，实际响应 %s", w.Body.String())
	}
}

func TestHandleUpdateLLMConfig_HotReloadsAndPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
	}

	eng := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, eng, nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"智谱",
		"providers":{
			"智谱":{"api_key":"sk-zhipu","base_url":"https://open.bigmodel.cn/api/paas/v4","model":"glm-5","compatible":"openai","locality":"cloud","keep_alive":"5m","num_ctx":4096}
		}
	}`))
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if eng.reloadCalls != 1 {
		t.Fatalf("期望热更新 1 次，实际 %d", eng.reloadCalls)
	}
	if eng.activeLLM.Default != "智谱" {
		t.Fatalf("引擎未热更新到新默认 provider，实际 %q", eng.activeLLM.Default)
	}
	if eng.activeLLM.Providers["智谱"].KeepAlive != "5m" {
		t.Fatalf("引擎热更新丢失 keep_alive，实际 %+v", eng.activeLLM.Providers["智谱"])
	}
	if got := eng.activeLLM.Providers["智谱"]; got.Locality != config.ProviderLocalityCloud || got.NumCtx != 4096 {
		t.Fatalf("引擎热更新丢失 locality/num_ctx，实际 %+v", got)
	}
	if srv.cfg.LLM.Default != "智谱" {
		t.Fatalf("服务端内存配置未更新，实际 %q", srv.cfg.LLM.Default)
	}

	configFile := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("读取持久化配置失败: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "glm-5") || !strings.Contains(content, "智谱") ||
		!strings.Contains(content, "keep_alive: 5m") || !strings.Contains(content, "locality: cloud") ||
		!strings.Contains(content, "num_ctx: 4096") {
		t.Fatalf("配置文件未写入新 provider: %s", content)
	}
}

func TestHandleUpdateLLMConfig_LegacyRevokesOldSemanticProviderBeforeSuccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "cloud-a"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-a": {APIKey: "sk-a", BaseURL: "https://a.example.test/v1", Model: "embed-a"},
	}
	recorder := &semanticProviderARecorder{}
	recorder.call()
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	srv.SetSemanticRuntimeInvalidator(func(context.Context) error {
		if _, changed := srv.cfg.LLM.Providers["cloud-b"]; changed {
			t.Fatal("new in-memory LLM config became visible before semantic revoke")
		}
		recorder.revoked = true
		return nil
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"providers":{"cloud-b":{"api_key":"sk-b","base_url":"https://b.example.test/v1","model":"embed-b","locality":"cloud"}}
	}`))
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	recorder.call()
	if recorder.calls != 1 || !recorder.revoked {
		t.Fatalf("provider A recorder after 200 = %+v, want no post-success call", recorder)
	}
}

func TestHandleUpdateLLMConfig_TransactionRevokesOldSemanticProviderBeforeSuccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "cloud-a"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-a": {APIKey: "sk-a", BaseURL: "https://a.example.test/v1", Model: "embed-a"},
	}
	recorder := &semanticProviderARecorder{}
	recorder.call()
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	srv.SetConfigTxManager(config.NewTransactionManager(cfg, nil, nil))
	srv.SetSemanticRuntimeInvalidator(func(context.Context) error {
		if _, changed := srv.cfg.LLM.Providers["cloud-b"]; changed {
			t.Fatal("new in-memory LLM config became visible before transactional semantic revoke")
		}
		recorder.revoked = true
		return nil
	})
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{config.FlagConfigTxHotloadV1: true})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"providers":{"cloud-b":{"api_key":"sk-b","base_url":"https://b.example.test/v1","model":"embed-b","locality":"cloud"}}
	}`))
	req = req.WithContext(featureflag.WithContext(req.Context(), flags))
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	recorder.call()
	if recorder.calls != 1 || !recorder.revoked {
		t.Fatalf("provider A recorder after transactional 200 = %+v, want no post-success call", recorder)
	}
}

func TestHandleUpdateLLMConfig_ReloadsSemanticRuntimeWithNextProvidersBeforeSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "cloud-a"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-a": {APIKey: "sk-a", BaseURL: "https://a.example.test/v1", Model: "chat-a"},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	reloads := 0
	srv.SetSemanticRuntimeReloader(func(_ context.Context, next config.LLMConfig) error {
		reloads++
		if _, ok := next.Providers["cloud-b"]; !ok {
			t.Fatalf("semantic reload did not receive next providers: %+v", next.Providers)
		}
		if _, visible := srv.cfg.LLM.Providers["cloud-b"]; visible {
			t.Fatal("next in-memory config became visible before semantic runtime replacement")
		}
		return nil
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"cloud-b",
		"providers":{"cloud-b":{"api_key":"sk-b","base_url":"https://b.example.test/v1","model":"chat-b","models":["chat-b"]}}
	}`))
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusOK || reloads != 1 {
		t.Fatalf("status=%d reloads=%d body=%s", w.Code, reloads, w.Body.String())
	}
	if _, ok := srv.cfg.LLM.Providers["cloud-b"]; !ok {
		t.Fatalf("new providers not visible after successful response path: %+v", srv.cfg.LLM.Providers)
	}
}

func TestHandleUpdateLLMConfig_DoesNotRevokeForNonProviderChangesOrFailedApply(t *testing.T) {
	t.Run("routing and cache only", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		cfg := config.DefaultConfig()
		revocations := 0
		srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
		srv.SetSemanticRuntimeInvalidator(func(context.Context) error { revocations++; return nil })
		req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
			"routing":{"enabled":true,"strategy":"quality-first"},
			"cache":{"enabled":true,"similarity":0.9,"ttl":"1h","max_entries":100}
		}`))
		w := httptest.NewRecorder()
		srv.handleUpdateLLMConfig(w, req)
		if w.Code != http.StatusOK || revocations != 0 {
			t.Fatalf("status=%d revocations=%d body=%s", w.Code, revocations, w.Body.String())
		}
	})

	t.Run("transaction apply failure", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		cfg := config.DefaultConfig()
		cfg.LLM.Providers = map[string]config.LLMProviderConfig{
			"cloud-a": {APIKey: "sk-a", BaseURL: "https://a.example.test/v1", Model: "embed-a"},
		}
		revocations := 0
		srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
		srv.SetConfigTxManager(config.NewTransactionManager(cfg, nil, []config.Applier{
			failingConfigApplier{err: errors.New("apply failed")},
		}))
		srv.SetSemanticRuntimeInvalidator(func(context.Context) error { revocations++; return nil })
		flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{config.FlagConfigTxHotloadV1: true})
		req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
			"providers":{"cloud-b":{"api_key":"sk-b","base_url":"https://b.example.test/v1","model":"embed-b","locality":"cloud"}}
		}`))
		req = req.WithContext(featureflag.WithContext(req.Context(), flags))
		w := httptest.NewRecorder()
		srv.handleUpdateLLMConfig(w, req)
		if w.Code != http.StatusInternalServerError || revocations != 0 {
			t.Fatalf("status=%d revocations=%d body=%s", w.Code, revocations, w.Body.String())
		}
	})
}

func TestHandleUpdateLLMConfig_SemanticReloadFailureRollsBackEveryConfigTruthAndRetainsSecretOnMaskedRetry(t *testing.T) {
	for _, transactional := range []bool{false, true} {
		name := "legacy"
		if transactional {
			name = "transaction"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			cfg := config.DefaultConfig()
			cfg.LLM.Default = "cloud-a"
			cfg.LLM.Providers = map[string]config.LLMProviderConfig{
				"cloud-a": {
					ProviderInstanceID: "pvd_v1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					APIKey:             "sk-original-secret", BaseURL: "https://a.example.test/v1",
					Model: "chat-a", Locality: config.ProviderLocalityCloud,
				},
			}
			if err := config.Save(cfg, ""); err != nil {
				t.Fatal(err)
			}
			eng := &mockEngine{activeLLM: cfg.LLM}
			srv := NewServer(cfg, eng, nil, nil)
			if transactional {
				srv.SetConfigTxManager(config.NewTransactionManager(cfg, nil, []config.Applier{
					mockEngineConfigApplier{engine: eng},
				}))
			}
			semanticState := cfg.LLM
			failNext := true
			srv.SetSemanticRuntimeReloader(func(_ context.Context, next config.LLMConfig) error {
				if _, replacing := next.Providers["cloud-b"]; replacing && failNext {
					failNext = false
					return errors.New("semantic replacement failed")
				}
				semanticState = next
				return nil
			})
			req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
				"default":"cloud-b",
				"providers":{"cloud-b":{"api_key":"sk-next-secret","base_url":"https://b.example.test/v1","model":"chat-b","locality":"cloud"}}
			}`))
			if transactional {
				flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{config.FlagConfigTxHotloadV1: true})
				req = req.WithContext(featureflag.WithContext(req.Context(), flags))
			}
			w := httptest.NewRecorder()
			srv.handleUpdateLLMConfig(w, req)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if _, changed := srv.cfg.LLM.Providers["cloud-b"]; changed {
				t.Fatalf("new in-memory config exposed after failed drain: %+v", srv.cfg.LLM.Providers)
			}
			if eng.activeLLM.Providers["cloud-a"].APIKey != "sk-original-secret" ||
				semanticState.Providers["cloud-a"].APIKey != "sk-original-secret" {
				t.Fatalf("runtime truths did not roll back: engine=%+v semantic=%+v", eng.activeLLM, semanticState)
			}
			persisted, err := config.Load(filepath.Join(home, ".hexclaw", "hexclaw.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if persisted.LLM.Providers["cloud-a"].APIKey != "sk-original-secret" {
				t.Fatalf("disk truth did not roll back with original secret: %+v", persisted.LLM.Providers)
			}

			getRecorder := httptest.NewRecorder()
			srv.handleGetLLMConfig(getRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
			if getRecorder.Code != http.StatusOK {
				t.Fatalf("GET status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
			}
			var visible LLMConfigResponse
			if err := json.Unmarshal(getRecorder.Body.Bytes(), &visible); err != nil {
				t.Fatal(err)
			}
			masked := visible.Providers["cloud-a"].APIKey
			if masked != config.MaskAPIKey("sk-original-secret") {
				t.Fatalf("GET masked key=%q, want original secret projection", masked)
			}
			if got := visible.Providers["cloud-a"].APIKeyLength; got != len("sk-original-secret") {
				t.Fatalf("GET api_key_length=%d, want %d（等长掩码元数据，仅长度不含 Key 内容）", got, len("sk-original-secret"))
			}

			retryBody, err := json.Marshal(LLMConfigUpdateRequest{
				Default: "cloud-a",
				Providers: map[string]LLMProviderConfigUpdateItem{
					"cloud-a": {
						ProviderInstanceID: "pvd_v1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						APIKey:             masked, BaseURL: "https://a.example.test/v1", Model: "chat-a",
						Locality: config.ProviderLocalityCloud,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			retry := httptest.NewRecorder()
			retryReq := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(string(retryBody)))
			if transactional {
				flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{config.FlagConfigTxHotloadV1: true})
				retryReq = retryReq.WithContext(featureflag.WithContext(retryReq.Context(), flags))
			}
			srv.handleUpdateLLMConfig(retry, retryReq)
			if retry.Code != http.StatusOK {
				t.Fatalf("masked retry status=%d body=%s", retry.Code, retry.Body.String())
			}
			persisted, err = config.Load(filepath.Join(home, ".hexclaw", "hexclaw.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if got := persisted.LLM.Providers["cloud-a"].APIKey; got != "sk-original-secret" {
				t.Fatalf("masked retry persisted %q, want original secret", got)
			}
		})
	}
}

func TestHandleUpdateLLMConfig_TransactionCompensationOutlivesCanceledRequest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "cloud-a"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-a": {
			ProviderInstanceID: "pvd_v1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			APIKey:             "sk-a", BaseURL: "https://a.example.test/v1", Model: "chat-a",
			Locality: config.ProviderLocalityCloud,
		},
	}
	if err := config.Save(cfg, ""); err != nil {
		t.Fatal(err)
	}

	eng := &mockEngine{activeLLM: cfg.LLM}
	applier := &contextCheckingEngineConfigApplier{engine: eng}
	txManager := config.NewTransactionManager(cfg, nil, []config.Applier{applier})
	srv := NewServer(cfg, eng, nil, nil)
	srv.SetConfigTxManager(txManager)

	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		config.FlagConfigTxHotloadV1: true,
	})
	requestCtx, cancelRequest := context.WithCancel(featureflag.WithContext(context.Background(), flags))
	defer cancelRequest()
	semanticState := cfg.LLM
	srv.SetSemanticRuntimeReloader(func(_ context.Context, next config.LLMConfig) error {
		if _, replacing := next.Providers["cloud-b"]; replacing {
			// Simulate the client disconnecting exactly after the forward config
			// transaction committed and while semantic replacement fails.
			cancelRequest()
			return errors.New("semantic replacement failed after client disconnect")
		}
		semanticState = next
		return nil
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"cloud-b",
		"providers":{"cloud-b":{"api_key":"sk-b","base_url":"https://b.example.test/v1","model":"chat-b","locality":"cloud"}}
	}`)).WithContext(requestCtx)
	w := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(applier.observations) != 2 {
		t.Fatalf("apply observations=%d, want forward plus compensation", len(applier.observations))
	}
	compensation := applier.observations[1]
	if compensation.canceled || !compensation.hasDeadline {
		t.Fatalf("compensation context canceled=%v deadline=%v, want independent bounded lifecycle", compensation.canceled, compensation.hasDeadline)
	}
	if got := txManager.Current().LLM.Default; got != "cloud-a" {
		t.Fatalf("transaction manager retained %q after compensation, want cloud-a", got)
	}
	if got := eng.activeLLM.Default; got != "cloud-a" {
		t.Fatalf("engine retained %q after compensation, want cloud-a", got)
	}
	if got := semanticState.Default; got != "cloud-a" {
		t.Fatalf("semantic runtime retained %q after compensation, want cloud-a", got)
	}
	if got := srv.cfg.LLM.Default; got != "cloud-a" {
		t.Fatalf("server config exposed %q after compensation, want cloud-a", got)
	}
	persisted, err := config.Load(filepath.Join(home, ".hexclaw", "hexclaw.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.LLM.Default; got != "cloud-a" {
		t.Fatalf("disk retained %q after compensation, want cloud-a", got)
	}
}

func TestHandleUpdateLLMConfig_RejectsMaskedKeyForUnknownProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{}
	srv := NewServer(cfg, &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"providers":{"new-provider":{"api_key":"****cret","base_url":"https://new.example.test/v1","model":"chat"}}
	}`))
	w := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "脱敏") {
		t.Fatalf("status=%d body=%s, want masked placeholder rejection", w.Code, w.Body.String())
	}
}

func TestHandleUpdateLLMConfig_PersistFailureDoesNotRevoke(t *testing.T) {
	blockedHome := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedHome, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blockedHome)
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-a": {APIKey: "sk-a", BaseURL: "https://a.example.test/v1", Model: "embed-a"},
	}
	revocations := 0
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	srv.SetSemanticRuntimeInvalidator(func(context.Context) error { revocations++; return nil })
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"providers":{"cloud-b":{"api_key":"sk-b","base_url":"https://b.example.test/v1","model":"embed-b","locality":"cloud"}}
	}`))
	w := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusInternalServerError || revocations != 0 {
		t.Fatalf("status=%d revocations=%d body=%s", w.Code, revocations, w.Body.String())
	}
}

func TestHandleUpdateLLMConfig_RejectsInvalidLocality(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"providers":{
			"openai":{"api_key":"sk-test","base_url":"http://localhost:18080/v1","model":"gpt-5.6-sol","locality":"somewhere"}
		}
	}`))
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid locality, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "locality") {
		t.Fatalf("error must identify locality: %s", w.Body.String())
	}
}

func TestHandleUpdateLLMConfig_RollsBackPersistedFileOnReloadFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
	}
	if err := config.Save(cfg, ""); err != nil {
		t.Fatalf("写入初始配置失败: %v", err)
	}

	eng := &mockEngine{
		activeLLM: cfg.LLM,
		reloadErr: errors.New("reload failed"),
	}
	srv := NewServer(cfg, eng, nil, nil)
	revocations := 0
	srv.SetSemanticRuntimeInvalidator(func(context.Context) error { revocations++; return nil })
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"智谱",
		"providers":{
			"智谱":{"api_key":"sk-zhipu","base_url":"https://open.bigmodel.cn/api/paas/v4","model":"glm-5","compatible":"openai"}
		}
	}`))
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d: %s", w.Code, w.Body.String())
	}
	if eng.reloadCalls != 1 {
		t.Fatalf("期望热更新 1 次，实际 %d", eng.reloadCalls)
	}
	if srv.cfg.LLM.Default != "openai" {
		t.Fatalf("热更新失败后不应污染内存配置，实际 %q", srv.cfg.LLM.Default)
	}
	if eng.activeLLM.Default != "openai" {
		t.Fatalf("引擎活跃配置不应变化，实际 %q", eng.activeLLM.Default)
	}
	if revocations != 0 {
		t.Fatalf("failed legacy reload revoked semantic runtime %d times", revocations)
	}

	configFile := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("读取回滚后的配置失败: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "openai") || strings.Contains(content, "glm-5") {
		t.Fatalf("配置文件未回滚到旧配置: %s", content)
	}
}
