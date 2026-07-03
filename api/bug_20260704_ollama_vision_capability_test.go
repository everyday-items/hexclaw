package api

import "testing"

// BUG-20260704：本地视觉模型（如 qwen3.5:9b）在「本地模型 (Ollama)」面板只显示「文本」，
// 不显示「视觉」。根因：后端 handleOllamaStatus 解析 /api/tags 时漏读 capabilities 字段
// （Ollama 新版 /api/tags 顶层已按模型上报 capabilities: [completion, vision, tools, thinking]），
// OllamaModel 不带能力 → 前端只能按模型名查静态表猜，qwen3.5:9b 不在表里被误判纯文本。
//
// 契约：parseOllamaTags 必须透出 /api/tags 上报的真实 capabilities，视觉模型带 "vision"。
func TestParseOllamaTags_CarriesRealCapabilities(t *testing.T) {
	// 真实 Ollama /api/tags 响应形状（qwen3.5:9b 实测 capabilities）。
	body := []byte(`{
		"models": [
			{
				"name": "qwen3.5:9b",
				"size": 6600000000,
				"modified_at": "2026-07-01T00:00:00Z",
				"capabilities": ["completion", "vision", "tools", "thinking"],
				"details": {"family": "qwen35", "parameter_size": "9B", "quantization_level": "Q4_K_M"}
			},
			{
				"name": "llama3.2:1b",
				"size": 1300000000,
				"modified_at": "2026-07-01T00:00:00Z",
				"capabilities": ["completion", "tools"],
				"details": {"family": "llama", "parameter_size": "1B", "quantization_level": "Q8_0"}
			}
		]
	}`)

	models := parseOllamaTags(body)
	if len(models) != 2 {
		t.Fatalf("应解析 2 个模型，实得 %d", len(models))
	}

	// 视觉模型：capabilities 必须含 vision（此前漏读 → 空 → 前端误判纯文本）。
	vision := models[0]
	if vision.Name != "qwen3.5:9b" {
		t.Fatalf("模型顺序错: %q", vision.Name)
	}
	if !hasCap(vision.Capabilities, "vision") {
		t.Fatalf("BUG-20260704: qwen3.5:9b 的 vision 能力丢失，capabilities=%v（视觉徽章不显示的根因）", vision.Capabilities)
	}
	if !hasCap(vision.Capabilities, "completion") {
		t.Fatalf("completion 能力也应透出，capabilities=%v", vision.Capabilities)
	}

	// 对照：纯文本模型不应凭空多出 vision。
	textModel := models[1]
	if hasCap(textModel.Capabilities, "vision") {
		t.Fatalf("llama3.2:1b 无 vision 却被标了视觉，capabilities=%v", textModel.Capabilities)
	}

	// 基础字段仍正确解析（capabilities 加字段不破坏原有）。
	if vision.Params != "9B" || vision.Family != "qwen35" || vision.Size != 6600000000 {
		t.Fatalf("基础字段解析回归: %+v", vision)
	}
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
