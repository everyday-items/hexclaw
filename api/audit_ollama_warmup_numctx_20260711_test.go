package api

import (
	"encoding/json"
	"testing"
)

// C3 预热 num_ctx 错位（AP-206）。
//
// handleOllamaLoad 的预热请求原样 {"model","prompt":"","keep_alive":"5m"} 不带
// options.num_ctx → Ollama 按默认 ctx(~4096) 加载 runner；真实对话经 ai-core llm/ollama
// 带分档 num_ctx（标准桌面聊天稳态 8192，真机取证 4096→8192）→ 首条消息触发 runner 重载，
// 预热白做。核心不变量：预热请求的 runner-affecting 参数(num_ctx)必须与真实对话一致。
//
// 真断言：解析 buildOllamaLoadBody 产出的 JSON，钉死 options.num_ctx 存在且等于真实对话
// 稳态档位，keep_alive 与请求级默认一致（30m，非 5m）。

func decodeLoadBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("预热请求体不是合法 JSON: %v (%s)", err, string(raw))
	}
	return m
}

func TestBuildOllamaLoadBody_CarriesNumCtx(t *testing.T) {
	// 默认档位（缺省 num_ctx → 稳态 8192），keep_alive 统一 30m。
	body := decodeLoadBody(t, buildOllamaLoadBody("qwen3:8b", resolveWarmupNumCtx(0), defaultWarmupKeepAlive))

	opts, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("预热请求体缺少 options（num_ctx 未下发）→ Ollama 按默认 ctx 加载 runner，真实对话重载：%v", body)
	}
	numCtx, ok := opts["num_ctx"].(float64)
	if !ok {
		t.Fatalf("options.num_ctx 缺失或非数值：%v", opts)
	}
	if int(numCtx) != defaultWarmupNumCtx {
		t.Fatalf("预热 num_ctx=%d 与真实对话稳态档 %d 不一致（AP-206 触发重载）", int(numCtx), defaultWarmupNumCtx)
	}
	if ka, _ := body["keep_alive"].(string); ka != defaultWarmupKeepAlive {
		t.Fatalf("预热 keep_alive=%q 与请求级默认 %q 不一致（预热提前失效）", ka, defaultWarmupKeepAlive)
	}
}

func TestBuildOllamaLoadBody_ExplicitNumCtxWins(t *testing.T) {
	// 前端显式下发与真实对话一致的档位应原样生效。
	body := decodeLoadBody(t, buildOllamaLoadBody("qwen3:8b", resolveWarmupNumCtx(16384), "1h"))
	opts, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("显式 num_ctx 下预热体仍缺 options：%v", body)
	}
	if got := int(opts["num_ctx"].(float64)); got != 16384 {
		t.Fatalf("显式 num_ctx 未原样生效：want 16384 got %d", got)
	}
	if ka, _ := body["keep_alive"].(string); ka != "1h" {
		t.Fatalf("显式 keep_alive 未原样生效：want 1h got %q", ka)
	}
}

func TestDefaultWarmupNumCtx_IsAiCoreTier(t *testing.T) {
	// 稳态默认档必须是 ai-core 分档表中的档位（8192），否则不可能与真实对话对齐。
	found := false
	for _, tier := range warmupNumCtxTiers {
		if tier == defaultWarmupNumCtx {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("defaultWarmupNumCtx=%d 不在 ai-core 分档表 %v 中", defaultWarmupNumCtx, warmupNumCtxTiers)
	}
}
