// Package engine 图片生成支持
//
// 通过 ai-core 的 ImageProvider 接口调用图片生成 API，
// 下载生成的图片并转为 data URI 内嵌到响应中（永不过期），
// 以 Markdown 格式返回到聊天流。
package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	mediaimg "github.com/hexagon-codes/ai-core/media/image"
	"github.com/hexagon-codes/hexclaw/httpua"
	"github.com/hexagon-codes/toolkit/net/httpx"
)

// imageModelPrefixes 已知的图片生成模型前缀（API 调用方的兼容回退）
var imageModelPrefixes = []string{
	"cogview",          // 智谱 CogView 系列
	"dall-e",           // OpenAI DALL-E 系列
	"gpt-image",        // OpenAI GPT Image
	"stable-diffusion", // Stability AI
}

// isImageGeneration 判断本次请求是否为图片生成
//
// 优先检查前端通过 metadata 传递的能力标记（metadata["image_generation"] == "true"），
// 回退到模型名前缀匹配（兼容 REST API 等未传 metadata 的场景）。
func isImageGeneration(model string, metadata map[string]string) bool {
	if metadata != nil && metadata["image_generation"] == "true" {
		return true
	}
	lower := strings.ToLower(model)
	for _, prefix := range imageModelPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// imageResult 图片生成结果
type imageResult struct {
	// DataURI base64 data URI（永不过期）
	DataURI string
	// RevisedPrompt 模型修正后的提示词
	RevisedPrompt string
}

// generateImage 通过 ai-core/media 的图片服务生成图片并内嵌为 data URI。
//
//  1. media 图片服务按 model 路由到对应 Provider（DALL-E / CogView / 兼容）
//  2. media Provider 默认请求 b64_json，直接内嵌为 data URI（不依赖外链寿命）；
//     若 Provider 只回 URL，则下载转 data URI，下载失败回退原始 URL。
func generateImage(ctx context.Context, svc *mediaimg.Service, model, prompt string) ([]imageResult, error) {
	if svc == nil || !svc.HasProvider() {
		return nil, fmt.Errorf("未配置图片生成服务（model=%s）", model)
	}

	res, err := svc.Generate(ctx, "", mediaimg.Request{Model: model, Prompt: prompt})
	if err != nil {
		return nil, err
	}
	if len(res.Images) == 0 {
		return nil, fmt.Errorf("图片生成 API 未返回图片")
	}

	results := make([]imageResult, 0, len(res.Images))
	for _, img := range res.Images {
		var dataURI string
		switch {
		case img.B64JSON != "":
			// media 默认 response_format=b64_json，OpenAI Images 返回 PNG。
			dataURI = "data:image/png;base64," + img.B64JSON
		case img.URL != "":
			if d, dlErr := downloadAsDataURI(ctx, img.URL); dlErr == nil {
				dataURI = d
			} else {
				dataURI = img.URL // 下载失败回退原始 URL
			}
		default:
			continue
		}
		results = append(results, imageResult{
			DataURI:       dataURI,
			RevisedPrompt: img.RevisedPrompt,
		})
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("图片生成未产生有效图像")
	}
	return results, nil
}

// imageHTTPClient 图片下载专用 HTTP client（复用连接池）
var imageHTTPClient = httpx.RawClient(httpx.WithRawTimeout(60 * time.Second))

// downloadAsDataURI 下载图片并转为 base64 data URI
func downloadAsDataURI(ctx context.Context, url string) (string, error) {
	client := imageHTTPClient
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	httpua.Set(req) // 默认浏览器 UA，避免反爬站对 Go 默认 UA 返回 HTML（AP-016）

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载图片失败: %s", resp.Status)
	}

	// 限制最大 10MB
	const maxSize = 10 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize))
	if err != nil {
		return "", err
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/png"
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf("data:%s;base64,%s", mime, encoded), nil
}

// formatImageMarkdown 将图片结果格式化为 Markdown
func formatImageMarkdown(results []imageResult) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		alt := r.RevisedPrompt
		if alt == "" {
			alt = "generated image"
		}
		b.WriteString(fmt.Sprintf("![%s](%s)", alt, r.DataURI))
	}
	return b.String()
}
