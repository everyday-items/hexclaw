package engine

// bug-20260703 视觉能力闸静态标记表缺 qwen3.5 系：
// Ollama /api/show 报告 qwen3.5:9b capabilities = [completion, vision, tools, thinking]
//（模型层直连识图实测通过，think 开/关均正确），但 modelSupportsImageInput 的
// 标记表只认 qwen-vl/qwen2-vl/…/qwen3-vl，qwen3.5 原生多模态命不中 →
// isKnownTextOnlyModel 兜底判文本 → 桌面会话发图 500「不支持图片附件」，
// 本地视觉链路整条被闸死。真机取证：POST /api/v1/chat 带图 500，
// 同图直连 Ollama 18.3s 正确识别并计算。

import (
	"context"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestBug20260703_Qwen35VisionGate(t *testing.T) {
	img := []adapter.Attachment{{Type: "image", Name: "q.png", Mime: "image/png", Data: "aGk="}}
	provider := modelFeatureProvider{models: []llm.ModelInfo{{
		ID:       "qwen3.5:9b",
		Name:     "qwen3.5:9b",
		Features: []string{llm.FeatureVision, llm.FeatureStreaming},
	}}}
	if shouldRejectImageAttachmentsForProvider(provider, "ollama", "qwen3.5:9b", img) {
		t.Error("vision-capable provider model should allow image attachments")
	}
	// 回归保护：真文本模型仍应拒图
	if !shouldRejectImageAttachments("deepseek", "deepseek-chat", img) {
		t.Error("deepseek-chat 应保持拒绝图片附件（已知文本模型）")
	}
}

type modelFeatureProvider struct {
	models []llm.ModelInfo
}

func (p modelFeatureProvider) Name() string { return "ollama" }

func (p modelFeatureProvider) Complete(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return &llm.CompletionResponse{Content: "ok"}, nil
}

func (p modelFeatureProvider) Stream(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
	return nil, nil
}

func (p modelFeatureProvider) Models() []llm.ModelInfo {
	return p.models
}

func (p modelFeatureProvider) CountTokens([]llm.Message) (int, error) {
	return 0, nil
}
