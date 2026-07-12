package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// BUG-20260712-B1：GET /api/v1/knowledge/embedding-status 契约。
// 前端知识库页据此渲染状态横幅——把「嵌入模型没装 → 自动注入静默休眠」这个隐形悬崖
// 变成可见状态 + 一键安装动作。
func fakeOllamaTags(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := `{"models":[`
		for i, m := range models {
			if i > 0 {
				body += ","
			}
			body += `{"name":"` + m + `"}`
		}
		body += `]}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getEmbeddingStatus(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/embedding-status", nil)
	rec := httptest.NewRecorder()
	srv.handleKnowledgeEmbeddingStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	return out
}

func TestBug20260712_EmbeddingStatus_LocalNotInstalled(t *testing.T) {
	ollama := fakeOllamaTags(t, "qwen3.5:9b") // 真机形态：只有 chat 模型
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetKnowledgeEmbeddingInfo(KnowledgeEmbeddingInfo{
		Enabled: true, Provider: "ollama", Model: "nomic-embed-text", BaseURL: ollama.URL, Local: true,
	})
	out := getEmbeddingStatus(t, srv)
	if out["configured"] != true || out["local"] != true {
		t.Fatalf("configured/local 契约不符: %v", out)
	}
	if out["ready"] != false {
		t.Fatalf("模型未装应 ready=false（自动注入休眠可见化）: %v", out)
	}
	if out["model"] != "nomic-embed-text" {
		t.Fatalf("model 字段供前端一键安装用: %v", out)
	}
}

func TestBug20260712_EmbeddingStatus_LocalInstalled(t *testing.T) {
	ollama := fakeOllamaTags(t, "nomic-embed-text:latest", "qwen3.5:9b")
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetKnowledgeEmbeddingInfo(KnowledgeEmbeddingInfo{
		Enabled: true, Provider: "ollama", Model: "nomic-embed-text", BaseURL: ollama.URL, Local: true,
	})
	out := getEmbeddingStatus(t, srv)
	if out["ready"] != true {
		t.Fatalf("模型已装（:latest 基名匹配）应 ready=true: %v", out)
	}
}

func TestBug20260712_EmbeddingStatus_Unconfigured(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	out := getEmbeddingStatus(t, srv) // 未注入 info（KB 关闭/无 provider）
	if out["configured"] != false || out["ready"] != false {
		t.Fatalf("未配置应 configured=false ready=false: %v", out)
	}
}
