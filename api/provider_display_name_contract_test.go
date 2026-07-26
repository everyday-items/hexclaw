package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestProviderDisplayNameContract_PutPersistsAndGetReturnsExactCase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	eng := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, eng, nil, nil)

	put := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"hexclaw-gpt",
		"providers":{"hexclaw-gpt":{
			"display_name":"HexClaw-GPT",
			"api_key":"",
			"base_url":"https://example.test/v1",
			"model":"gpt-5.6-sol",
			"compatible":"openai"
		}}
	}`))
	putRec := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}

	configFile := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	persisted, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if !strings.Contains(string(persisted), "display_name: HexClaw-GPT") {
		t.Fatalf("persisted config lost exact provider display_name")
	}

	getRec := httptest.NewRecorder()
	srv.handleGetLLMConfig(
		getRec,
		httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil),
	)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var wire struct {
		Providers map[string]struct {
			DisplayName string `json:"display_name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if got := wire.Providers["hexclaw-gpt"].DisplayName; got != "HexClaw-GPT" {
		t.Fatalf("GET display_name=%q, want exact configured case", got)
	}
}
