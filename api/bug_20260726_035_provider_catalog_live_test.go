package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

const bug20260726035ProviderCatalogLiveGate = "HEXCLAW_REAL_PROVIDER_CATALOG_035"

// TestBUG20260726035ConfiguredModelExistsInProviderCatalog 通过生产模型目录路径执行一次真实查询。
// 测试仅输出脱敏后的 HTTP 类别、目录数量和配置模型是否存在，不记录目录内容或连接信息。
func TestBUG20260726035ConfiguredModelExistsInProviderCatalog(t *testing.T) {
	if strings.TrimSpace(os.Getenv(bug20260726035ProviderCatalogLiveGate)) != "1" {
		t.Skip()
	}

	cfg, err := config.Load(bug20260728DesktopConfigPath(t))
	if err != nil {
		t.Fatal("http_category=none catalog_count=0 configured_model_present=false")
	}
	providerKey, providerInstanceID := bug20260728FindLiveProvider(t, cfg)
	configuredModel := strings.TrimSpace(cfg.LLM.Providers[providerKey].Model)

	payload, err := json.Marshal(map[string]string{
		"provider_instance_id": providerInstanceID,
	})
	if err != nil {
		t.Fatal("http_category=none catalog_count=0 configured_model_present=false")
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/api/v1/config/llm/models",
		strings.NewReader(string(payload)),
	)
	(&Server{cfg: cfg}).handleFetchProviderModels(recorder, request)

	var response struct {
		Models []providerModelInfo `json:"models"`
		Error  string              `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf(
			"http_category=%s catalog_count=0 configured_model_present=false",
			bug20260726035HTTPCategory(recorder.Code, ""),
		)
	}

	configuredModelPresent := false
	for _, model := range response.Models {
		if strings.TrimSpace(model.ID) == configuredModel {
			configuredModelPresent = true
			break
		}
	}
	httpCategory := bug20260726035HTTPCategory(recorder.Code, response.Error)
	if response.Error != "" || recorder.Code < http.StatusOK || recorder.Code >= http.StatusMultipleChoices || !configuredModelPresent {
		t.Fatalf(
			"http_category=%s catalog_count=%d configured_model_present=%t",
			httpCategory,
			len(response.Models),
			configuredModelPresent,
		)
	}
	t.Logf(
		"http_category=%s catalog_count=%d configured_model_present=%t",
		httpCategory,
		len(response.Models),
		configuredModelPresent,
	)
}

func bug20260726035HTTPCategory(statusCode int, upstreamError string) string {
	for _, marker := range []struct {
		prefix   string
		category string
	}{
		{prefix: "HTTP 2", category: "2xx"},
		{prefix: "HTTP 3", category: "3xx"},
		{prefix: "HTTP 4", category: "4xx"},
		{prefix: "HTTP 5", category: "5xx"},
	} {
		if strings.Contains(upstreamError, marker.prefix) {
			return marker.category
		}
	}
	if upstreamError != "" {
		return "none"
	}
	switch {
	case statusCode >= 200 && statusCode < 300:
		return "2xx"
	case statusCode >= 300 && statusCode < 400:
		return "3xx"
	case statusCode >= 400 && statusCode < 500:
		return "4xx"
	case statusCode >= 500 && statusCode < 600:
		return "5xx"
	default:
		return "none"
	}
}
