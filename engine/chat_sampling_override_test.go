package engine

import (
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestApplyCompletionOverrides_RequestSamplingWinsAgentDefaults(t *testing.T) {
	req := hexagon.CompletionRequest{}
	metadata := map[string]string{
		"agent_temperature":   "0.7",
		"agent_max_tokens":    "4096",
		"request_temperature": "0",
		"request_max_tokens":  "128",
	}

	applyCompletionOverrides(&req, metadata)

	if req.Temperature == nil || *req.Temperature != 0 {
		t.Fatalf("请求级 temperature 未覆盖 Agent 默认值，实际 %v", req.Temperature)
	}
	if req.MaxTokens != 128 {
		t.Fatalf("请求级 max_tokens 未覆盖 Agent 默认值，实际 %d", req.MaxTokens)
	}
}

func TestRequestSamplingOverridesBypassSemanticCache(t *testing.T) {
	msg := &adapter.Message{
		Content:  "解释一下 Go context",
		Metadata: map[string]string{"request_temperature": "0"},
	}
	if !shouldBypassSemanticCache(msg) {
		t.Fatal("显式请求级采样参数应绕过语义缓存，确保参数真正作用于本次模型调用")
	}
}

func TestApplyCompletionOverrides_RejectsInvalidMetadataSamplingValues(t *testing.T) {
	baseTemperature := 0.4
	for _, tc := range []struct {
		name     string
		metadata map[string]string
	}{
		{name: "agent out of range", metadata: map[string]string{"agent_temperature": "3", "agent_max_tokens": "1000001"}},
		{name: "request non finite and negative", metadata: map[string]string{"request_temperature": "NaN", "request_max_tokens": "-1"}},
		{name: "request excessive", metadata: map[string]string{"request_temperature": "+Inf", "request_max_tokens": "1000001"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := hexagon.CompletionRequest{Temperature: &baseTemperature, MaxTokens: 256}
			applyCompletionOverrides(&req, tc.metadata)
			if req.Temperature == nil || *req.Temperature != baseTemperature {
				t.Fatalf("非法 temperature 覆盖了安全默认值: %v", req.Temperature)
			}
			if req.MaxTokens != 256 {
				t.Fatalf("非法 max_tokens 覆盖了安全默认值: %d", req.MaxTokens)
			}
		})
	}
}
