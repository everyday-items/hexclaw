// Package knowledge 提供个人知识库管理

package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAIEmbedder 基于 OpenAI 兼容 API 的向量嵌入生成器
//
// 兼容所有支持 /v1/embeddings 端点的 Provider：
//   - OpenAI (text-embedding-3-small, text-embedding-3-large, text-embedding-ada-002)
//   - DeepSeek
//   - 通义千问 (text-embedding-v3)
//   - 任何 OpenAI 兼容的私有部署
//
// 使用方式：
//
//	embedder := knowledge.NewOpenAIEmbedder("sk-xxx", "text-embedding-3-small")
//	vectors, _ := embedder.Embed(ctx, []string{"hello world"})
type OpenAIEmbedder struct {
	apiKey    string
	model     string
	baseURL   string
	dimension int
	client    *http.Client
}

// EmbedderOption OpenAIEmbedder 配置选项
type EmbedderOption func(*OpenAIEmbedder)

// WithBaseURL 设置自定义 API 端点
//
// 用于连接中转代理或私有部署的 OpenAI 兼容服务。
// 传入的 URL 应为 API 前缀（如 "https://api.example.com/v1"），
// 实际请求会追加 "/embeddings" 路径。
func WithBaseURL(url string) EmbedderOption {
	return func(e *OpenAIEmbedder) { e.baseURL = url }
}

// WithDimension 设置向量维度
//
// 某些模型支持自定义维度（如 text-embedding-3-small 支持 256-3072）。
// 不设置则使用模型默认维度。
func WithDimension(dim int) EmbedderOption {
	return func(e *OpenAIEmbedder) { e.dimension = dim }
}

// WithHTTPClient 设置自定义 HTTP 客户端
//
// 用于需要代理、自定义 TLS 等场景。
func WithHTTPClient(client *http.Client) EmbedderOption {
	return func(e *OpenAIEmbedder) { e.client = client }
}

// NewOpenAIEmbedder 创建 OpenAI 兼容的 Embedder
//
// 参数：
//   - apiKey: API Key
//   - model: 模型名称（如 "text-embedding-3-small"）
//   - opts: 可选配置（自定义端点、维度等）
//
// 默认维度根据模型自动设置：
//   - text-embedding-3-small: 1536
//   - text-embedding-3-large: 3072
//   - text-embedding-ada-002: 1536
//   - 其他模型默认 1536
func NewOpenAIEmbedder(apiKey, model string, opts ...EmbedderOption) *OpenAIEmbedder {
	e := &OpenAIEmbedder{
		apiKey:  apiKey,
		model:   model,
		baseURL: "https://api.openai.com/v1",
		client:  &http.Client{Timeout: 30 * time.Second},
	}

	// 根据模型设置默认维度
	switch model {
	case "text-embedding-3-small":
		e.dimension = 1536
	case "text-embedding-3-large":
		e.dimension = 3072
	case "text-embedding-ada-002":
		e.dimension = 1536
	default:
		e.dimension = 1536 // 默认维度
	}

	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Embed 将一组文本转换为向量
//
// 调用 /v1/embeddings API，返回每段文本的向量表示。
// 支持批量请求，一次最多处理 2048 条文本（受 API 限制）。
//
// 返回值：
//   - [][]float32: 每段文本对应的向量，顺序与输入一致
//   - error: API 调用失败或响应解析失败时返回错误
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody := embeddingRequest{
		Model: e.model,
		Input: texts,
	}
	if e.dimension > 0 {
		reqBody.Dimensions = e.dimension
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	url := e.baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Embedding API 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding API 返回 %d: %s", resp.StatusCode, string(respBody))
	}

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	// 按 index 排序（API 可能乱序返回）
	vectors := make([][]float32, len(texts))
	for _, data := range result.Data {
		if data.Index < len(vectors) {
			vectors[data.Index] = data.Embedding
		}
	}

	return vectors, nil
}

// Dimension 返回向量维度
func (e *OpenAIEmbedder) Dimension() int {
	return e.dimension
}

// --- API 数据结构 ---

// embeddingRequest OpenAI Embedding API 请求体
type embeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

// embeddingResponse OpenAI Embedding API 响应体
type embeddingResponse struct {
	Data  []embeddingData `json:"data"`
	Model string          `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// embeddingData 单条 embedding 数据
type embeddingData struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}
