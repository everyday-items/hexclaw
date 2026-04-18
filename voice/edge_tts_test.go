package voice

import (
	"testing"
)

// TestEdgeTTS_Fix_v0_3_12_H5 回归测试：
// 修复前 cmd/hexclaw/main.go 的 TTS 初始化分支只处理 OpenAI/Azure（靠 LLM Provider 的 API Key），
// edge-tts 这种无需 Key 的 Provider 会因 `pc.APIKey != ""` 条件判定失败而静默跳过，
// 结果日志显示 "TTS=" 为空字符串，家长无法给孩子读答案。
// 修复后 main.go 优先识别 "edge-tts" / "edge" 别名直接构造，不需 LLM Provider 配置。
func TestEdgeTTS_Fix_v0_3_12_H5(t *testing.T) {
	t.Run("before_fix_behavior_edge_tts_requires_apikey_check", func(t *testing.T) {
		// 修复前逻辑：extractLLMName("edge-tts") → "edge-tts"
		// 但 cfg.LLM.Providers["edge-tts"] 不存在 → 条件不满足 → tts = nil
		t.Logf("修复前：cfg.Voice.TTS.Provider=\"edge-tts\" 走 OpenAI 分支，因 LLM Providers 里没对应项静默跳过")
	})

	t.Run("after_fix_constructor_works_without_apikey", func(t *testing.T) {
		// Edge TTS 免费，NewEdgeTTS() 必须无参可用
		tts := NewEdgeTTS()
		if tts == nil {
			t.Fatal("NewEdgeTTS() 返回 nil")
		}
		if tts.Name() != "edge-tts" {
			t.Errorf("Provider 名应为 edge-tts，实际 %q", tts.Name())
		}
		if tts.defaultVoice == "" {
			t.Error("默认音色不应为空（预期 zh-CN-XiaoxiaoNeural）")
		}
	})

	t.Run("after_fix_custom_voice_option", func(t *testing.T) {
		tts := NewEdgeTTS(EdgeTTSWithVoice("zh-CN-YunxiNeural"))
		if tts.defaultVoice != "zh-CN-YunxiNeural" {
			t.Errorf("自定义音色未生效：got=%q want=%q", tts.defaultVoice, "zh-CN-YunxiNeural")
		}
	})

	t.Run("after_fix_exposes_chinese_voices_for_k12", func(t *testing.T) {
		// K12 场景主要是小学生中文读题，至少要有 4 个以上中文音色可选
		tts := NewEdgeTTS()
		voices := tts.Voices()
		zhCount := 0
		for _, v := range voices {
			if v.Language == "zh-CN" || v.Language == "zh-TW" {
				zhCount++
			}
		}
		if zhCount < 4 {
			t.Errorf("中文音色数量不足（K12 场景至少 4 个）：实际 %d", zhCount)
		}
		t.Logf("修复后：Edge TTS 暴露 %d 个中文音色 ✓", zhCount)
	})

	t.Run("after_fix_supports_mp3_format", func(t *testing.T) {
		tts := NewEdgeTTS()
		formats := tts.SupportedFormats()
		hasMp3 := false
		for _, f := range formats {
			if f == FormatMP3 {
				hasMp3 = true
				break
			}
		}
		if !hasMp3 {
			t.Error("应支持 MP3 格式（Tauri Audio 最通用）")
		}
	})

	t.Run("after_fix_empty_text_rejected", func(t *testing.T) {
		tts := NewEdgeTTS()
		_, err := tts.Synthesize(nil, "", SynthesizeOptions{})
		if err == nil {
			t.Error("空文本应返回 error")
		}
	})
}
