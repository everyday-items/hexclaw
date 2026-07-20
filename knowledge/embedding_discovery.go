// Package knowledge —— 嵌入模型自动发现（BUG-20260712-B1，嵌入模型开箱保证）。
//
// 背景：auto-config 默认选 ollama/nomic-embed-text，但从未有人保证它真的安装了——
// 未安装时每次 Embed 失败，知识库自动注入常年降级（fail-closed 后=静默休眠）。
// 本文件把「假设装了」变成「装了什么用什么」：探测 Ollama 已安装的嵌入能力模型，
// 命中即零配置零下载激活；未命中保持默认接线（用户一键安装后无需重启即生效）。
//
// 分层备注（20260712 评审定案）：本文件是**机制**（无产品语义、无包内依赖的独立签名），
// 属未来下沉 hexagon（rag/embedder 配套）的候选——届时整文件搬移。
package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
)

// embeddingNameMarkers 嵌入模型名家族（Ollama 生态惯例）：命中任一即视为嵌入能力模型。
// 依据名字判定而非 API 探测——Ollama /api/tags 不暴露模型能力，名字家族是生态事实标准。
var embeddingNameMarkers = []string{"embed", "bge-", "gte-", "all-minilm"}

// IsEmbeddingModelName 报告模型名是否属于嵌入模型家族（大小写不敏感）。
func IsEmbeddingModelName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	for _, marker := range embeddingNameMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

const ollamaProbeTimeout = 2 * time.Second

const ollamaPullResponseHeaderTimeout = 4 * time.Hour

func normalizedOllamaEmbeddingBaseURL(baseURL string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	// 兼容传入 OpenAI 形态的 /v1 前缀（provider BaseURL 常见写法）
	return strings.TrimSuffix(base, "/v1")
}

func ollamaEmbeddingHTTPClient(baseURL string, responseHeaderTimeout time.Duration) (*http.Client, error) {
	return egress.NewProviderHTTPClient(
		baseURL,
		config.ProviderPrivateNetworkAccess{},
		egress.WithProviderResponseHeaderTimeout(responseHeaderTimeout),
	)
}

// ollamaTags 探测 baseURL 的已安装模型名列表。第二个返回值区分「服务可用但
// 尚无模型」和「服务不可达」，避免启动装配把不可达端点当成可调用能力。
func ollamaTags(ctx context.Context, baseURL string) ([]string, bool) {
	base := normalizedOllamaEmbeddingBaseURL(baseURL)
	pctx, cancel := context.WithTimeout(ctx, ollamaProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return nil, false
	}
	client, err := ollamaEmbeddingHTTPClient(base, ollamaProbeTimeout)
	if err != nil {
		return nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, false
	}
	names := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return names, true
}

// InspectOllamaEmbedding performs one Ollama tags probe and reports both the
// first installed embedding model and whether the service itself is reachable.
func InspectOllamaEmbedding(ctx context.Context, baseURL string) (model string, serviceAvailable bool) {
	names, available := ollamaTags(ctx, baseURL)
	if !available {
		return "", false
	}
	for _, name := range names {
		if IsEmbeddingModelName(name) {
			return name, true
		}
	}
	return "", true
}

// DetectOllamaEmbeddingModel 返回 Ollama 已安装的首个嵌入能力模型（零配置零下载激活）。
// 无嵌入模型 / 端点不可达 → ("", false)，调用方保持默认接线。
func DetectOllamaEmbeddingModel(ctx context.Context, baseURL string) (string, bool) {
	model, _ := InspectOllamaEmbedding(ctx, baseURL)
	return model, model != ""
}

// OllamaServiceAvailable reports whether the Ollama native API is reachable,
// including the valid zero-model state.
func OllamaServiceAvailable(ctx context.Context, baseURL string) bool {
	_, available := ollamaTags(ctx, baseURL)
	return available
}

// OllamaModelMatches applies Ollama's tag-defaulting contract without treating
// all tags as interchangeable. An explicitly configured tag must match exactly;
// an untagged configuration accepts only the bare name or the implicit latest.
func OllamaModelMatches(installed, configured string) bool {
	installedBase, installedTag, installedTagged := splitOllamaModelTag(installed)
	configuredBase, configuredTag, configuredTagged := splitOllamaModelTag(configured)
	if installedBase == "" || configuredBase == "" || installedBase != configuredBase {
		return false
	}
	if configuredTagged {
		return installedTagged && installedTag == configuredTag
	}
	return !installedTagged || installedTag == "latest"
}

func splitOllamaModelTag(model string) (base, tag string, tagged bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return "", "", false
	}
	// A registry port may contain a colon before the final slash. Only a colon
	// after that slash denotes an Ollama model tag.
	lastSlash := strings.LastIndexByte(model, '/')
	lastColon := strings.LastIndexByte(model, ':')
	if lastColon > lastSlash {
		return model[:lastColon], model[lastColon+1:], true
	}
	return model, "", false
}

// OllamaModelInstalled reports whether the configured model reference is
// installed according to OllamaModelMatches. embedding-status uses it for the
// ready state, so versioned configurations must never activate another tag.
func OllamaModelInstalled(ctx context.Context, baseURL, model string) bool {
	if strings.TrimSpace(model) == "" {
		return false
	}
	names, _ := ollamaTags(ctx, baseURL)
	for _, name := range names {
		if OllamaModelMatches(name, model) {
			return true
		}
	}
	return false
}

// PullOllamaModel 阻塞式拉取模型（丢弃流式进度，只关心结果）。首启后台静默安装用；
// 前端手动路径走 api 的 SSE 进度端点，不经此函数。
func PullOllamaModel(ctx context.Context, baseURL, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("ollama pull: model is required")
	}
	base := normalizedOllamaEmbeddingBaseURL(baseURL)
	payload, err := json.Marshal(struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}{Model: model, Stream: false})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/pull", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client, err := ollamaEmbeddingHTTPClient(base, ollamaPullResponseHeaderTimeout)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama pull %s: HTTP %d", model, resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// EnsureOllamaEmbeddingModel 保证嵌入模型就位（BUG-20260712-B1 静默预置，幂等）：
// 已装 → no-op 返回 true；未装 → 拉取（成功 true / 失败 false，调用方据此置状态，
// 失败即前端知识库页浮出手动重试横幅——异常驱动披露，成功路径用户零感知）。
func EnsureOllamaEmbeddingModel(ctx context.Context, baseURL, model string) (bool, error) {
	if OllamaModelInstalled(ctx, baseURL, model) {
		return true, nil
	}
	if err := PullOllamaModel(ctx, baseURL, model); err != nil {
		return false, err
	}
	return OllamaModelInstalled(ctx, baseURL, model), nil
}
