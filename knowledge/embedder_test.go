package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/internal/testutil/httpmock"
)

// TestNewOpenAIEmbedder_DefaultDimensions 测试不同模型的默认维度
func TestNewOpenAIEmbedder_DefaultDimensions(t *testing.T) {
	tests := []struct {
		model   string
		wantDim int
	}{
		{"text-embedding-3-small", 1536},
		{"text-embedding-3-large", 3072},
		{"text-embedding-ada-002", 1536},
		{"unknown-model", 1536}, // 未知模型默认 1536
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			e := NewOpenAIEmbedder("test-key", tt.model)
			if e.Dimension() != tt.wantDim {
				t.Errorf("Dimension() = %d, want %d", e.Dimension(), tt.wantDim)
			}
		})
	}
}

// TestNewOpenAIEmbedder_Options 测试配置选项
func TestNewOpenAIEmbedder_Options(t *testing.T) {
	e := NewOpenAIEmbedder("test-key", "text-embedding-3-small",
		WithBaseURL("https://custom.api.com/v1"),
		WithDimension(256),
	)

	if e.baseURL != "https://custom.api.com/v1" {
		t.Errorf("baseURL = %q, want %q", e.baseURL, "https://custom.api.com/v1")
	}
	if e.Dimension() != 256 {
		t.Errorf("Dimension() = %d, want %d", e.Dimension(), 256)
	}
	if e.apiKey != "test-key" {
		t.Errorf("apiKey = %q, want %q", e.apiKey, "test-key")
	}
	if e.model != "text-embedding-3-small" {
		t.Errorf("model = %q, want %q", e.model, "text-embedding-3-small")
	}
}

// TestNewOpenAIEmbedder_WithHTTPClient 测试自定义 HTTP 客户端
func TestNewOpenAIEmbedder_WithHTTPClient(t *testing.T) {
	customClient := &http.Client{}
	e := NewOpenAIEmbedder("test-key", "test-model", WithHTTPClient(customClient))
	if e.client != customClient {
		t.Error("WithHTTPClient 未正确设置自定义客户端")
	}
}

// TestOpenAIEmbedder_Embed_Success 测试正常的 Embedding 调用
func TestOpenAIEmbedder_Embed_Success(t *testing.T) {
	// 模拟 OpenAI API 返回
	e := NewOpenAIEmbedder("test-key", "text-embedding-3-small",
		WithBaseURL("https://embedder.test"),
		WithHTTPClient(httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 验证请求
			if r.Method != "POST" {
				t.Errorf("Method = %q, want POST", r.Method)
			}
			if r.URL.Path != "/embeddings" {
				t.Errorf("Path = %q, want /embeddings", r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), "Bearer test-key")
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
			}

			// 解析请求体
			var req embeddingRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("解析请求体失败: %v", err)
			}
			if req.Model != "text-embedding-3-small" {
				t.Errorf("Model = %q, want text-embedding-3-small", req.Model)
			}
			if len(req.Input) != 2 {
				t.Errorf("Input len = %d, want 2", len(req.Input))
			}

			// 返回模拟响应（故意乱序返回以测试 index 排序）
			resp := embeddingResponse{
				Data: []embeddingData{
					{Index: 1, Embedding: []float32{0.4, 0.5, 0.6}},
					{Index: 0, Embedding: []float32{0.1, 0.2, 0.3}},
				},
				Model: "text-embedding-3-small",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))),
	)

	vectors, err := e.Embed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}

	if len(vectors) != 2 {
		t.Fatalf("vectors len = %d, want 2", len(vectors))
	}

	// 验证按 index 正确排序
	if vectors[0][0] != 0.1 || vectors[0][1] != 0.2 || vectors[0][2] != 0.3 {
		t.Errorf("vectors[0] = %v, want [0.1, 0.2, 0.3]", vectors[0])
	}
	if vectors[1][0] != 0.4 || vectors[1][1] != 0.5 || vectors[1][2] != 0.6 {
		t.Errorf("vectors[1] = %v, want [0.4, 0.5, 0.6]", vectors[1])
	}
}

// TestOpenAIEmbedder_Embed_EmptyInput 测试空输入
func TestOpenAIEmbedder_Embed_EmptyInput(t *testing.T) {
	e := NewOpenAIEmbedder("test-key", "test-model")

	vectors, err := e.Embed(context.Background(), []string{})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if vectors != nil {
		t.Errorf("vectors = %v, want nil", vectors)
	}

	vectors, err = e.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed(nil) error = %v", err)
	}
	if vectors != nil {
		t.Errorf("vectors = %v, want nil for nil input", vectors)
	}
}

// TestOpenAIEmbedder_Embed_APIError 测试 API 返回非 200 状态码
func TestOpenAIEmbedder_Embed_APIError(t *testing.T) {
	e := NewOpenAIEmbedder("test-key", "test-model",
		WithBaseURL("https://embedder.test"),
		WithHTTPClient(httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error": {"message": "Rate limit exceeded"}}`))
		}))),
	)

	_, err := e.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("Embed() expected error, got nil")
	}

	// 验证错误信息包含状态码和响应体
	errMsg := err.Error()
	if !contains(errMsg, "429") {
		t.Errorf("error should contain status code 429, got: %s", errMsg)
	}
	if !contains(errMsg, "Rate limit exceeded") {
		t.Errorf("error should contain API error message, got: %s", errMsg)
	}
}

// TestOpenAIEmbedder_Embed_ContextCanceled 测试上下文取消
func TestOpenAIEmbedder_Embed_ContextCanceled(t *testing.T) {
	e := NewOpenAIEmbedder("test-key", "test-model",
		WithBaseURL("https://embedder.test"),
		WithHTTPClient(&http.Client{
			Transport: httpmock.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}),
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	_, err := e.Embed(ctx, []string{"test"})
	if err == nil {
		t.Fatal("Embed() expected error for canceled context, got nil")
	}
	if !errors.Is(err, context.Canceled) && !contains(err.Error(), "canceled") {
		t.Fatalf("Embed() error = %v, want context canceled", err)
	}
}

// TestOpenAIEmbedder_Embed_InvalidJSON 测试 API 返回无效 JSON
func TestOpenAIEmbedder_Embed_InvalidJSON(t *testing.T) {
	e := NewOpenAIEmbedder("test-key", "test-model",
		WithBaseURL("https://embedder.test"),
		WithHTTPClient(httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{invalid json`))
		}))),
	)

	_, err := e.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("Embed() expected error for invalid JSON, got nil")
	}
	if !contains(err.Error(), "解析响应失败") {
		t.Errorf("error should mention parse failure, got: %s", err.Error())
	}
}

// TestOpenAIEmbedder_Dimension 测试 Dimension 方法
func TestOpenAIEmbedder_Dimension(t *testing.T) {
	e := NewOpenAIEmbedder("key", "text-embedding-3-small")
	if e.Dimension() != 1536 {
		t.Errorf("Dimension() = %d, want 1536", e.Dimension())
	}

	e = NewOpenAIEmbedder("key", "text-embedding-3-small", WithDimension(512))
	if e.Dimension() != 512 {
		t.Errorf("Dimension() = %d, want 512 (custom)", e.Dimension())
	}
}

// TestOpenAIEmbedder_ImplementsEmbedder 编译时验证实现 Embedder 接口
var _ Embedder = (*OpenAIEmbedder)(nil)

// contains 简单的字符串包含检查
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
