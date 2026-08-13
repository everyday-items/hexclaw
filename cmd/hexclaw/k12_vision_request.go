package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"

	"github.com/hexagon-codes/hexclaw/egress"
)

// completeK12VisionRequest 是生产代码和真实模型探针共用的唯一 K12 图片与提示词到
// Provider 请求边界。调用方负责路由和出网用途；该函数负责与线路相关的请求契约，
// 避免强类型识题策略、操作安全性、MIME 处理和持久 deadline 在不同入口间漂移。
func completeK12VisionRequest(
	ctx context.Context,
	provider llm.Provider,
	model string,
	image []byte,
	prompt string,
) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("K12 vision provider is not configured")
	}
	if len(image) == 0 {
		return "", fmt.Errorf("K12 vision image is empty")
	}
	ctx = egress.WithRequest(
		ctx,
		egress.PurposeVisionOCR,
		"",
		egress.ClassSensitiveMedia,
	)

	requestMetadata, reasoningPolicyScope, err := k12VisionRequestMetadata(ctx)
	if err != nil {
		return "", err
	}
	mime := http.DetectContentType(image)
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		mime = "image/png"
	}
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)

	// 调用方的持久 deadline 始终具有权威性。这里只把相同的剩余预算传入受保护的
	// 请求局部 HTTP transport；绝不修改共享客户端，也不为旧调用虚构 deadline。
	ctx = withK12StageResponseHeaderDeadline(ctx)
	response, err := provider.Complete(k12NonIdempotentLLMContext(ctx), llm.CompletionRequest{
		Model:                model,
		Metadata:             requestMetadata,
		ReasoningPolicyScope: reasoningPolicyScope,
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			MultiContent: []llm.ContentPart{
				llm.NewTextPart(prompt),
				llm.NewImageURLPart(dataURL, "high"),
			},
		}},
	})
	if err != nil {
		return "", err
	}
	if response == nil {
		return "", fmt.Errorf("K12 vision provider returned an empty response")
	}
	return response.Content, nil
}
