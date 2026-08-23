package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/storage"
)

type modelCapabilityProbeRecordingProvider struct {
	requests []hexagon.CompletionRequest
}

type modelCapabilityProbeBlockingProvider struct {
	started chan struct{}
	release chan struct{}
}

func TestModelCapabilityProbeFailureCode_IdentifiesTruncatedOutput(t *testing.T) {
	got := modelCapabilityProbeFailureCode(modelCapabilityProbeResponseError(&hexagon.CompletionResponse{
		FinishReason: "length",
	}))
	if got != "PROBE_OUTPUT_TRUNCATED" {
		t.Fatalf("failure code=%q, want PROBE_OUTPUT_TRUNCATED", got)
	}
}

func TestModelCapabilityProbeRequestsDisableOptionalThinking(t *testing.T) {
	requests := []hexagon.CompletionRequest{
		modelCapabilityProbeTextRequest("qwen3.5:9b"),
		modelCapabilityProbeVisionRequest("qwen3.5:9b"),
		modelCapabilityProbeToolsRequest("qwen3.5:9b"),
	}
	for _, request := range requests {
		if got, ok := request.Metadata["think"].(bool); !ok || got {
			t.Fatalf("probe request thinking metadata=%#v, want think=false", request.Metadata)
		}
	}
}

func TestModelCapabilityProbeRequestsReserveVisibleOutputBudget(t *testing.T) {
	requests := []hexagon.CompletionRequest{
		modelCapabilityProbeTextRequest("openrouter/free"),
		modelCapabilityProbeVisionRequest("openrouter/free"),
		modelCapabilityProbeToolsRequest("openrouter/free"),
	}
	for _, request := range requests {
		if request.MaxTokens != 128 {
			t.Fatalf("probe max tokens=%d, want 128", request.MaxTokens)
		}
	}
}

func (p *modelCapabilityProbeBlockingProvider) Complete(
	ctx context.Context,
	_ hexagon.CompletionRequest,
) (*hexagon.CompletionResponse, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	select {
	case <-p.release:
		return &hexagon.CompletionResponse{Content: "OK"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func runModelCapabilityProbe(
	t *testing.T,
	srv *Server,
	kinds ...string,
) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(LLMModelCapabilityProbeRequest{
		ProviderInstanceID: bug20260728ProviderInstanceID,
		Model:              "gpt-5.6-terra",
		Kinds:              kinds,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/probe", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer model-probe-capability-token")
	req.RemoteAddr = "127.0.0.1:4321"
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func (p *modelCapabilityProbeRecordingProvider) Complete(
	_ context.Context,
	req hexagon.CompletionRequest,
) (*hexagon.CompletionResponse, error) {
	p.requests = append(p.requests, req)
	return &hexagon.CompletionResponse{Content: "OK"}, nil
}

// TestModelCapabilityProbe_VisionBypassesStaticRouteGateForExplicitProbe 锁定静态路由授权与
// 逐模型能力实验的边界。声明为 text-only 的模型在普通聊天路由中仍保持 text-only，
// 但显式、已保存配置的 vision probe 必须到达 Provider 并独立记录结果。
func TestModelCapabilityProbe_VisionBypassesStaticRouteGateForExplicitProbe(t *testing.T) {
	oldFactory := llmTestProviderFactory
	recording := &modelCapabilityProbeRecordingProvider{}
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider { return recording }
	defer func() { llmTestProviderFactory = oldFactory }()

	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	cfg.LLM.Providers["custom"] = config.LLMProviderConfig{
		ProviderInstanceID: bug20260728ProviderInstanceID,
		APIKey:             "sk-model-probe-red",
		BaseURL:            "https://provider.example.test/v1",
		Model:              "gpt-5.6-terra",
		Models:             []string{"gpt-5.6-terra"},
		ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{{
			ID:           "gpt-5.6-terra",
			Capabilities: []string{config.LLMModelCapabilityText},
		}},
		Compatible: "openai",
		Locality:   config.ProviderLocalityCloud,
	}
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	srv.SetSidecarCapabilityToken("model-probe-capability-token")

	rec := runModelCapabilityProbe(t, srv, modelCapabilityProbeKindVision)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(recording.requests) != 1 {
		t.Fatalf("provider calls=%d, want one explicit vision probe", len(recording.requests))
	}
	if len(recording.requests[0].Messages) != 1 || len(recording.requests[0].Messages[0].MultiContent) != 2 {
		t.Fatalf("vision probe did not carry one text part and one image part: %+v", recording.requests[0].Messages)
	}
	image := recording.requests[0].Messages[0].MultiContent[1].ImageURL
	if image == nil || image.Detail != "auto" {
		t.Fatalf("vision probe image detail=%+v, want protocol-neutral auto", image)
	}
	encoded := strings.TrimPrefix(image.URL, "data:image/png;base64,")
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode fixed probe image: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode fixed probe PNG: %v", err)
	}
	if decoded.Bounds().Dx() < 64 || decoded.Bounds().Dy() < 64 {
		t.Fatalf("probe image dimensions=%dx%d, want a non-degenerate 64px fixture", decoded.Bounds().Dx(), decoded.Bounds().Dy())
	}

	get := httptest.NewRecorder()
	srv.handleGetLLMConfig(get, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
	var response struct {
		Providers map[string]struct {
			EffectiveModels []struct {
				ID            string   `json:"id"`
				Capabilities  []string `json:"capabilities"`
				ProbeReceipts []struct {
					ProbeKind string `json:"probe_kind"`
					Outcome   string `json:"outcome"`
				} `json:"probe_receipts"`
			} `json:"effective_models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	models := response.Providers["custom"].EffectiveModels
	if len(models) != 1 || models[0].ID != "gpt-5.6-terra" {
		t.Fatalf("effective_models=%+v, want the saved model", models)
	}
	if len(models[0].Capabilities) != 1 || models[0].Capabilities[0] != config.LLMModelCapabilityText {
		t.Fatalf("probe must not rewrite static route capabilities: %+v", models[0].Capabilities)
	}
	if len(models[0].ProbeReceipts) != 1 || models[0].ProbeReceipts[0].ProbeKind != "vision" || models[0].ProbeReceipts[0].Outcome != "passed" {
		t.Fatalf("model-specific vision receipt=%+v, want persisted passed vision receipt", models[0].ProbeReceipts)
	}
}

func TestModelCapabilityProbe_TextDoesNotVerifyVision(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &modelCapabilityProbeRecordingProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	provider := cfg.LLM.Providers["custom"]
	provider.Model = "gpt-5.6-terra"
	provider.Models = []string{"gpt-5.6-terra"}
	provider.ModelSpecs = []config.LLMProviderModelSpec{{
		ID:           "gpt-5.6-terra",
		Capabilities: []string{config.LLMModelCapabilityText},
	}}
	cfg.LLM.Providers["custom"] = provider
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	srv.SetSidecarCapabilityToken("model-probe-capability-token")

	if rec := runModelCapabilityProbe(t, srv, modelCapabilityProbeKindText); rec.Code != http.StatusOK {
		t.Fatalf("text probe status=%d body=%s", rec.Code, rec.Body.String())
	}

	get := httptest.NewRecorder()
	srv.handleGetLLMConfig(get, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	var response struct {
		Providers map[string]struct {
			EffectiveModels []struct {
				CapabilityStates map[string]string `json:"capability_states"`
				ProbeReceipts    []struct {
					ProbeKind string `json:"probe_kind"`
				} `json:"probe_receipts"`
			} `json:"effective_models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	model := response.Providers["custom"].EffectiveModels[0]
	if model.CapabilityStates[modelCapabilityProbeKindText] != "verified" || model.CapabilityStates[modelCapabilityProbeKindVision] != "unknown" {
		t.Fatalf("capability states=%+v, text must not verify vision", model.CapabilityStates)
	}
	if len(model.ProbeReceipts) != 1 || model.ProbeReceipts[0].ProbeKind != modelCapabilityProbeKindText {
		t.Fatalf("probe receipts=%+v, want only text", model.ProbeReceipts)
	}
}

func TestModelCapabilityProbe_ConfigFingerprintControlsProjection(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &modelCapabilityProbeRecordingProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	provider := cfg.LLM.Providers["custom"]
	provider.Model = "gpt-5.6-terra"
	provider.Models = []string{"gpt-5.6-terra", "gpt-5.6-luna"}
	provider.ModelSpecs = []config.LLMProviderModelSpec{
		{ID: "gpt-5.6-terra", Capabilities: []string{config.LLMModelCapabilityText}},
		{ID: "gpt-5.6-luna", Capabilities: []string{config.LLMModelCapabilityText}},
	}
	cfg.LLM.Providers["custom"] = provider
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	srv.SetSidecarCapabilityToken("model-probe-capability-token")

	if rec := runModelCapabilityProbe(t, srv, modelCapabilityProbeKindVision); rec.Code != http.StatusOK {
		t.Fatalf("vision probe status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 切换默认模型不属于 Terra 的物理执行配置，历史回执必须仍可投影。
	srv.cfgMu.Lock()
	provider = srv.cfg.LLM.Providers["custom"]
	provider.Model = "gpt-5.6-luna"
	srv.cfg.LLM.Providers["custom"] = provider
	srv.cfgMu.Unlock()
	providerResponse := bug20260728GetProvider(t, srv)
	effectiveModels, ok := providerResponse["effective_models"].([]any)
	if !ok || len(effectiveModels) != 2 {
		t.Fatalf("effective_models=%#v", providerResponse["effective_models"])
	}
	terra := effectiveModels[0].(map[string]any)
	if len(terra["probe_receipts"].([]any)) != 1 {
		t.Fatalf("default-model switch hid Terra receipt: %#v", terra)
	}

	// endpoint 改变后同一历史记录保留在 SQLite，但不得继续投影为当前事实。
	srv.cfgMu.Lock()
	provider = srv.cfg.LLM.Providers["custom"]
	provider.BaseURL = "https://changed.example.test/v1"
	srv.cfg.LLM.Providers["custom"] = provider
	srv.cfgMu.Unlock()
	providerResponse = bug20260728GetProvider(t, srv)
	effectiveModels = providerResponse["effective_models"].([]any)
	terra = effectiveModels[0].(map[string]any)
	if len(terra["probe_receipts"].([]any)) != 0 {
		t.Fatalf("endpoint change exposed stale receipt: %#v", terra)
	}
}

func TestModelCapabilityProbe_PolicyVersionControlsProjection(t *testing.T) {
	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	provider := cfg.LLM.Providers["custom"]
	provider.Model = "gpt-5.6-terra"
	provider.Models = []string{"gpt-5.6-terra"}
	provider.ModelSpecs = []config.LLMProviderModelSpec{{
		ID:           "gpt-5.6-terra",
		Capabilities: []string{config.LLMModelCapabilityText},
	}}
	cfg.LLM.Providers["custom"] = provider
	if _, err := store.SaveModelCapabilityProbeReceipt(context.Background(), &storage.ModelCapabilityProbeReceipt{
		ProviderInstanceID: bug20260728ProviderInstanceID,
		ModelID:            "gpt-5.6-terra",
		ProbeKind:          modelCapabilityProbeKindVision,
		ConfigFingerprint:  modelCapabilityProbeConfigFingerprint(canonicalProviderProbeType("custom", provider), provider, "gpt-5.6-terra"),
		ProbePolicyVersion: "v0",
		Outcome:            "passed",
		TestedAt:           2,
		ProbeStartedAt:     1,
	}); err != nil {
		t.Fatalf("save stale-policy receipt: %v", err)
	}
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	providerResponse := bug20260728GetProvider(t, srv)
	effectiveModels := providerResponse["effective_models"].([]any)
	model := effectiveModels[0].(map[string]any)
	if len(model["probe_receipts"].([]any)) != 0 {
		t.Fatalf("stale-policy receipt must not project: %#v", model)
	}
}

func TestModelCapabilityProbe_StaleConfigDoesNotPersistReceipt(t *testing.T) {
	oldFactory := llmTestProviderFactory
	blocking := &modelCapabilityProbeBlockingProvider{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider { return blocking }
	defer func() { llmTestProviderFactory = oldFactory }()

	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	provider := cfg.LLM.Providers["custom"]
	provider.Model = "gpt-5.6-terra"
	provider.Models = []string{"gpt-5.6-terra"}
	provider.ModelSpecs = []config.LLMProviderModelSpec{{
		ID:           "gpt-5.6-terra",
		Capabilities: []string{config.LLMModelCapabilityText},
	}}
	cfg.LLM.Providers["custom"] = provider
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	srv.SetSidecarCapabilityToken("model-probe-capability-token")

	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- runModelCapabilityProbe(t, srv, modelCapabilityProbeKindVision) }()
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("vision probe did not reach the provider")
	}
	srv.cfgMu.Lock()
	provider = srv.cfg.LLM.Providers["custom"]
	provider.BaseURL = "https://changed.example.test/v1"
	srv.cfg.LLM.Providers["custom"] = provider
	srv.cfgMu.Unlock()
	close(blocking.release)

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("stale probe did not finish")
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), CodeProbeConfigStale) {
		t.Fatalf("stale probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	providerResponse := bug20260728GetProvider(t, srv)
	effectiveModels := providerResponse["effective_models"].([]any)
	terra := effectiveModels[0].(map[string]any)
	if len(terra["probe_receipts"].([]any)) != 0 {
		t.Fatalf("stale probe wrote a current receipt: %#v", terra)
	}
}
