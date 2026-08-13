package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

const (
	bug20260728LiveProviderProbeGate = "HEXCLAW_REAL_PROVIDER_PROBE"
)

// TestBUG20260728ProviderProbeReceipt_RealHexClawGPT 是显式启用的测试：
// 它通过生产连接测试链路，仅将本地明确配置的 Provider 加载到进程内存中。
// 测试不会记录、序列化或写入凭据、端点及 Provider 标识。
func TestBUG20260728ProviderProbeReceipt_RealHexClawGPT(t *testing.T) {
	if strings.TrimSpace(os.Getenv(bug20260728LiveProviderProbeGate)) != "1" {
		t.Skip("set HEXCLAW_REAL_PROVIDER_PROBE=1 to run the real HexClaw-GPT provider probe")
	}

	// api/TestMain 会特意将 HOME 替换为临时目录，因此这里显式定位用户批准的本地配置；
	// Provider 仅在内存中使用，此探针不会写入 YAML 或凭据存储。
	cfg, err := config.Load(bug20260728DesktopConfigPath(t))
	if err != nil {
		t.Fatal("load local HexClaw configuration failed (details withheld to protect credentials)")
	}
	providerKey, providerInstanceID := bug20260728FindLiveProvider(t, cfg)
	provider := cfg.LLM.Providers[providerKey]
	if strings.TrimSpace(provider.APIKey) == "" {
		t.Fatal("the required local HexClaw-GPT provider has no usable runtime credential")
	}

	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "provider-probe-live.db"))
	if err != nil {
		t.Fatalf("open isolated receipt store: error_type=%T", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init isolated receipt store: error_type=%T", err)
	}

	first := &Server{cfg: cfg, store: store}
	probe := bug20260728RunLiveProviderProbe(t, first, providerInstanceID)
	if ok, _ := probe["ok"].(bool); !ok {
		t.Fatal("real HexClaw-GPT gpt-5.6-sol connection probe did not succeed")
	}
	if persisted, _ := probe["persisted"].(bool); !persisted {
		t.Fatal("real provider probe did not persist its receipt")
	}
	if model, _ := probe["model"].(string); model != "gpt-5.6-sol" {
		t.Fatalf("real provider probe model=%q, want gpt-5.6-sol", model)
	}
	if testedAt, _ := probe["tested_at"].(float64); testedAt <= 0 {
		t.Fatal("real provider probe response has no tested_at receipt timestamp")
	}

	// 在同一持久化存储上创建新 Server，用于模拟 Sidecar 重启边界；
	// 其 GET 投影必须在不再次探测的情况下恢复已确认的事实。
	restarted := &Server{cfg: cfg, store: store}
	restored := bug20260728GetLiveProvider(t, restarted, providerKey)
	receipt, ok := restored["probe_receipt"].(map[string]any)
	if !ok {
		t.Fatal("restarted provider config did not restore probe_receipt")
	}
	if outcome, _ := receipt["outcome"].(string); outcome != "passed" {
		t.Fatalf("restored receipt outcome=%q, want passed", outcome)
	}
	if receiptID, _ := receipt["provider_instance_id"].(string); receiptID != providerInstanceID {
		t.Fatal("restored receipt is not bound to the tested provider instance")
	}
	if testedAt, _ := receipt["tested_at"].(float64); testedAt <= 0 {
		t.Fatal("restored receipt has no tested_at timestamp")
	}

	t.Logf("REAL_PROVIDER_PROBE_PASS provider=HexClaw-GPT model=gpt-5.6-sol latency_ms=%.0f", probe["latency_ms"])
}

func bug20260728DesktopConfigPath(t *testing.T) string {
	t.Helper()
	account, err := user.Current()
	if err != nil || strings.TrimSpace(account.HomeDir) == "" {
		t.Fatal("resolve the signed-in macOS account for the desktop configuration failed")
	}
	return filepath.Join(account.HomeDir, ".hexclaw", "hexclaw.yaml")
}

func bug20260728FindLiveProvider(t *testing.T, cfg *config.Config) (string, string) {
	t.Helper()
	if cfg == nil {
		t.Fatal("local HexClaw configuration is unavailable")
	}
	var providerKey, providerInstanceID string
	for key, provider := range cfg.LLM.Providers {
		if !strings.EqualFold(strings.TrimSpace(key), "hexclaw-gpt") ||
			strings.TrimSpace(provider.Model) != "gpt-5.6-sol" {
			continue
		}
		if providerKey != "" {
			t.Fatal("multiple local HexClaw-GPT gpt-5.6-sol providers are configured")
		}
		providerKey = key
		providerInstanceID = config.EffectiveProviderInstanceID(key, provider)
	}
	if providerKey == "" || providerInstanceID == "" {
		t.Fatal("the required local HexClaw-GPT gpt-5.6-sol provider is not configured")
	}
	return providerKey, providerInstanceID
}

func bug20260728RunLiveProviderProbe(
	t *testing.T,
	srv *Server,
	providerInstanceID string,
) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"provider": map[string]string{"provider_instance_id": providerInstanceID},
	})
	if err != nil {
		t.Fatalf("encode live provider probe request: %v", err)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(string(body)))
	srv.handleTestLLMConfig(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("live provider probe status=%d", recorder.Code)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode live provider probe response: %v", err)
	}
	return response
}

func bug20260728GetLiveProvider(
	t *testing.T,
	srv *Server,
	providerKey string,
) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	srv.handleGetLLMConfig(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/config/llm", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("get restarted provider config status=%d", recorder.Code)
	}
	var response struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode restarted provider config: %v", err)
	}
	provider, ok := response.Providers[providerKey]
	if !ok {
		t.Fatal("restarted provider config did not contain the tested provider")
	}
	return provider
}
