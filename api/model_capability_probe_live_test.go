package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/config"
)

const liveModelCapabilityProbeGate = "HEXCLAW_LIVE_MODEL_CAPABILITY_PROBE"

// TestLiveModelCapabilityProbe 使用已保存配置验证当前源码的单次模型能力探测。
// 默认跳过，避免常规测试访问真实 Provider；失败日志只输出安全诊断元数据。
func TestLiveModelCapabilityProbe(t *testing.T) {
	if strings.TrimSpace(os.Getenv(liveModelCapabilityProbeGate)) != "1" {
		t.Skip("set HEXCLAW_LIVE_MODEL_CAPABILITY_PROBE=1 to run a real model capability probe")
	}
	configPath := strings.TrimSpace(os.Getenv("HEXCLAW_LIVE_PROBE_CONFIG"))
	providerInstanceID := strings.TrimSpace(os.Getenv("HEXCLAW_LIVE_PROBE_PROVIDER_INSTANCE_ID"))
	modelID := strings.TrimSpace(os.Getenv("HEXCLAW_LIVE_PROBE_MODEL"))
	probeKind := strings.TrimSpace(os.Getenv("HEXCLAW_LIVE_PROBE_KIND"))
	if configPath == "" || providerInstanceID == "" || modelID == "" || probeKind == "" {
		t.Fatal("live probe config, provider instance ID, model, and kind are required")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load live probe config: %v", err)
	}
	srv := NewServer(cfg, &mockEngine{}, nil, nil)
	candidate, err := srv.modelCapabilityProbeCandidate(providerInstanceID, modelID)
	if err != nil {
		t.Fatalf("prepare live probe candidate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelCapabilityProbeTimeout)
	defer cancel()
	err = srv.executeModelCapabilityProbe(ctx, candidate, probeKind)
	if err == nil {
		return
	}
	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) && providerErr != nil {
		var upstream struct {
			Error struct {
				Code    string `json:"code"`
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(providerErr.Body), &upstream)
		t.Fatalf(
			"live probe failed: provider_status=%d provider_body_len=%d request_id_present=%t upstream_code=%q upstream_type=%q upstream_message_len=%d upstream_message_signals=%v",
			providerErr.StatusCode, len(providerErr.Body), providerErr.RequestID != "",
			upstream.Error.Code, upstream.Error.Type, len(upstream.Error.Message),
			liveProbeMessageSignals(upstream.Error.Message),
		)
	}
	t.Fatalf("live probe failed: error_type=%T error_len=%d", err, len(err.Error()))
}

// liveProbeMessageSignals 只导出固定错误类别，用于真实探测诊断，绝不输出上游原文。
func liveProbeMessageSignals(message string) []string {
	lower := strings.ToLower(message)
	checks := map[string]string{
		"base64":     "base64",
		"content":    "content",
		"data_uri":   "data:",
		"decode":     "decode",
		"dimension":  "dimension",
		"detail":     "detail",
		"fetch":      "fetch",
		"file":       "file",
		"format":     "format",
		"height":     "height",
		"image":      "image",
		"image_url":  "image_url",
		"input":      "input",
		"invalid":    "invalid",
		"max_tokens": "max_tokens",
		"mime":       "mime",
		"minimum":    "minimum",
		"model":      "model",
		"parameter":  "parameter",
		"pixel":      "pixel",
		"png":        "png",
		"resolution": "resolution",
		"size":       "size",
		"supported":  "support",
		"url":        "url",
		"vision":     "vision",
		"width":      "width",
	}
	result := make([]string, 0, len(checks))
	for signal, needle := range checks {
		if strings.Contains(lower, needle) {
			result = append(result, signal)
		}
	}
	sort.Strings(result)
	return result
}
