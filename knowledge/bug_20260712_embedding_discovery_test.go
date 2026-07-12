package knowledge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-20260712-B1（嵌入模型开箱保证 · 自动发现）：真机取证——auto-config 选定
// ollama/nomic-embed-text，但用户 Ollama 里只有 qwen3.5:9b，该模型从未安装 →
// 每次 Embed 失败 → 知识库自动注入常年处于降级态（修复前注垃圾，修复后静默）。
//
// 契约：
//   - DetectOllamaEmbeddingModel 探测 /api/tags，返回**已安装**的首个嵌入能力模型
//     （零配置零下载即激活）；
//   - 无嵌入模型 / 端点不可达 → (_, false)，调用方保持默认 nomic-embed-text 接线
//     （用户一键安装后无需重启即生效）；
//   - IsEmbeddingModelName 只认嵌入家族，绝不把 chat 模型误判为嵌入模型。
func fakeOllama(t *testing.T, models ...string) *httptest.Server {
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

func TestBug20260712_DetectOllamaEmbeddingModel(t *testing.T) {
	ctx := context.Background()

	// 已装嵌入模型（bge-m3）→ 直接采用（零下载激活）
	srv := fakeOllama(t, "qwen3.5:9b", "bge-m3:latest")
	if got, ok := DetectOllamaEmbeddingModel(ctx, srv.URL); !ok || got != "bge-m3:latest" {
		t.Fatalf("应发现已装嵌入模型 bge-m3:latest, got %q ok=%v", got, ok)
	}

	// 只有 chat 模型（真机形态）→ 未发现
	srv2 := fakeOllama(t, "qwen3.5:9b")
	if got, ok := DetectOllamaEmbeddingModel(ctx, srv2.URL); ok {
		t.Fatalf("chat 模型不得被误判为嵌入模型, got %q", got)
	}

	// 端点不可达 → 静默未发现（不阻断启动）
	if _, ok := DetectOllamaEmbeddingModel(ctx, "http://127.0.0.1:1"); ok {
		t.Fatal("端点不可达应返回未发现")
	}
}

func TestBug20260712_IsEmbeddingModelName(t *testing.T) {
	yes := []string{"nomic-embed-text", "nomic-embed-text:latest", "bge-m3", "BGE-large:latest", "mxbai-embed-large", "snowflake-arctic-embed2", "all-minilm:l6", "granite-embedding"}
	no := []string{"qwen3.5:9b", "llama3.2", "deepseek-r1:7b", "glm-4.5", "gemma3:4b"}
	for _, n := range yes {
		if !IsEmbeddingModelName(n) {
			t.Fatalf("%q 应识别为嵌入模型", n)
		}
	}
	for _, n := range no {
		if IsEmbeddingModelName(n) {
			t.Fatalf("%q 不得误判为嵌入模型", n)
		}
	}
}

// OllamaModelInstalled：embedding-status 端点用它做 ready 判定（按冒号前基名匹配）。
func TestBug20260712_OllamaModelInstalled(t *testing.T) {
	ctx := context.Background()
	srv := fakeOllama(t, "nomic-embed-text:latest", "qwen3.5:9b")
	if !OllamaModelInstalled(ctx, srv.URL, "nomic-embed-text") {
		t.Fatal("nomic-embed-text 已装（:latest）应判 ready")
	}
	if OllamaModelInstalled(ctx, srv.URL, "bge-m3") {
		t.Fatal("未装模型不得判 ready")
	}
}
