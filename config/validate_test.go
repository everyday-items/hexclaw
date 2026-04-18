package config

import (
	"errors"
	"strings"
	"testing"
)

// TestConfigValidate_Fix_v0_3_12_H6 回归测试：
// 修复前 YAML 解析成功就直接启动；家长把 port 写成 999999 / provider 拼写错了 / duration 格式非法
// 都只在运行时报一个模糊的错，不告诉是哪行。
// 修复后 Validate() 在 Load() 末尾跑，一次性列出所有违规 + 字段路径 + 建议修正。
func TestConfigValidate_Fix_v0_3_12_H6(t *testing.T) {
	t.Run("before_fix_behavior_no_validation", func(t *testing.T) {
		t.Logf("修复前：YAML 解析 OK = 启动 OK；端口越界/duration 非法要等到运行时才报错")
	})

	t.Run("after_fix_valid_config_passes", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Server.MCPPort = 16070
		cfg.Server.Host = "127.0.0.1"
		cfg.Budget.MaxDuration = "30m"
		cfg.Budget.MaxCost = 5.0
		if err := cfg.Validate(); err != nil {
			t.Errorf("合法配置不应报错：%v", err)
		}
	})

	t.Run("after_fix_rejects_out_of_range_port", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 999999
		err := cfg.Validate()
		if err == nil {
			t.Fatal("端口 999999 应报错")
		}
		if !strings.Contains(err.Error(), "server.port") {
			t.Errorf("错误应指出字段 server.port：%v", err)
		}
		if !strings.Contains(err.Error(), "999999") {
			t.Errorf("错误应显示违规值 999999：%v", err)
		}
		t.Logf("修复后错误消息：\n%s", err)
	})

	t.Run("after_fix_rejects_duplicate_ports", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Server.MCPPort = 16060 // 冲突
		err := cfg.Validate()
		if err == nil {
			t.Fatal("port 与 mcp_port 相同应报错")
		}
		if !strings.Contains(err.Error(), "server.mcp_port") {
			t.Errorf("错误应指出字段：%v", err)
		}
	})

	t.Run("after_fix_rejects_invalid_duration_format", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Budget.MaxDuration = "30 分钟" // 非法：不是 Go duration 格式
		err := cfg.Validate()
		if err == nil {
			t.Fatal("duration 非法应报错")
		}
		if !strings.Contains(err.Error(), "budget.max_duration") {
			t.Errorf("错误应指出字段：%v", err)
		}
	})

	t.Run("after_fix_rejects_invalid_host", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Server.Host = "127.0.0.1;evil.com" // 含分号
		err := cfg.Validate()
		if err == nil {
			t.Fatal("非法 host 应报错")
		}
	})

	t.Run("after_fix_rejects_unknown_tts_provider", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "edge-tss" // 典型 typo
		err := cfg.Validate()
		if err == nil {
			t.Fatal("未知 TTS provider 应报错")
		}
		if !strings.Contains(err.Error(), "voice.tts.provider") {
			t.Errorf("错误应指出字段：%v", err)
		}
		if !strings.Contains(err.Error(), "edge-tts") {
			t.Errorf("错误应建议正确值 edge-tts：%v", err)
		}
	})

	t.Run("after_fix_accepts_edge_tts", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "edge-tts"
		if err := cfg.Validate(); err != nil {
			t.Errorf("edge-tts 应通过：%v", err)
		}
	})

	t.Run("after_fix_rejects_negative_budget", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Budget.MaxCost = -1.5
		err := cfg.Validate()
		if err == nil {
			t.Fatal("负 MaxCost 应报错")
		}
	})

	t.Run("after_fix_aggregates_multiple_errors", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 999999        // 错 1
		cfg.Budget.MaxDuration = "乱七八糟" // 错 2
		cfg.Budget.MaxCost = -10        // 错 3
		err := cfg.Validate()
		if err == nil {
			t.Fatal("应收集到多个错误")
		}
		var ves ValidationErrors
		if !errors.As(err, &ves) {
			t.Fatalf("期望 ValidationErrors 类型：%T", err)
		}
		if len(ves) < 3 {
			t.Errorf("期望至少 3 个错误，实际 %d", len(ves))
		}
		t.Logf("修复后：一次性报告 %d 个问题，不是报一个就停 ✓", len(ves))
	})

	t.Run("after_fix_router_default_agent_must_match_list", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Router.Agents = []AgentStaticConfig{{Name: "assistant"}, {Name: "coder"}}
		cfg.Router.DefaultAgent = "tutor" // 不在列表
		err := cfg.Validate()
		if err == nil {
			t.Fatal("default_agent 未在 agents 列表应报错")
		}
		if !strings.Contains(err.Error(), "router.default_agent") {
			t.Errorf("错误应指出字段：%v", err)
		}
	})

	t.Run("after_fix_router_default_agent_matches_list_ok", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Router.Agents = []AgentStaticConfig{{Name: "assistant"}, {Name: "coder"}}
		cfg.Router.DefaultAgent = "assistant"
		if err := cfg.Validate(); err != nil {
			t.Errorf("合法的 default_agent 应通过：%v", err)
		}
	})

	t.Run("after_fix_router_enabled_no_agents_allowed", func(t *testing.T) {
		// router 可以启用但 agents 为空（运行时 API 注入），不应报错
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Router.Enabled = true
		if err := cfg.Validate(); err != nil {
			t.Errorf("router 启用但无静态 agent 应通过（允许运行时注入）：%v", err)
		}
	})

	// F7 补强：Port=0 / TTS openai 简写 / Voice ID 格式
	t.Run("f7_rejects_port_zero", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.MCPPort = 16070 // 只留 Port=0
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Port=0 应拦截（YAML 显式写 0 基本都是手误）")
		}
		if !strings.Contains(err.Error(), "server.port") {
			t.Errorf("错误应指出 server.port 字段：%v", err)
		}
	})

	t.Run("f7_rejects_tts_openai_shorthand_without_llm", func(t *testing.T) {
		// 家长填 voice.tts.provider: "openai" 但 llm.providers 里没配 openai → 静默失败
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "openai" // 裸 "openai" 而不是 "openai-tts"
		cfg.LLM.Providers = map[string]LLMProviderConfig{
			"deepseek": {}, // 有 deepseek 没 openai
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("provider=\"openai\" 且 llm.providers 无 openai 时应拦截")
		}
	})

	t.Run("f7_accepts_tts_openai_tts_with_llm_configured", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "openai-tts"
		cfg.LLM.Providers = map[string]LLMProviderConfig{
			"openai": {APIKey: "sk-xxx"},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("openai-tts + llm.providers.openai 齐全应通过：%v", err)
		}
	})

	t.Run("f7_rejects_invalid_voice_id_format", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "edge-tts"
		cfg.Voice.TTS.Voice = "晓晓" // 中文名不是 ID
		err := cfg.Validate()
		if err == nil {
			t.Fatal("中文音色名应拦截")
		}
		if !strings.Contains(err.Error(), "voice.tts.voice") {
			t.Errorf("错误应指出字段：%v", err)
		}
	})

	t.Run("f7_accepts_proper_voice_id", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "edge-tts"
		cfg.Voice.TTS.Voice = "zh-CN-XiaoxiaoNeural"
		if err := cfg.Validate(); err != nil {
			t.Errorf("合法 voice ID 应通过：%v", err)
		}
	})

	// G1 修复：放宽 isPlausibleVoiceID 以兼容 OpenAI 短名
	t.Run("g1_accepts_openai_short_voice_names", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "openai-tts"
		cfg.LLM.Providers = map[string]LLMProviderConfig{
			"openai": {APIKey: "sk-xxx"},
		}
		for _, v := range []string{"alloy", "echo", "fable", "onyx", "nova", "shimmer"} {
			cfg.Voice.TTS.Voice = v
			if err := cfg.Validate(); err != nil {
				t.Errorf("OpenAI 短名 %q 应通过：%v", v, err)
			}
		}
	})

	t.Run("g1_rejects_voice_with_injection_chars", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "edge-tts"
		for _, bad := range []string{`foo"bar`, `foo;bar`, `foo\bar`} {
			cfg.Voice.TTS.Voice = bad
			if err := cfg.Validate(); err == nil {
				t.Errorf("含注入字符的 voice %q 应被拦截", bad)
			}
		}
	})

	t.Run("g1_rejects_over_long_voice", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "edge-tts"
		cfg.Voice.TTS.Voice = strings.Repeat("a", 200)
		if err := cfg.Validate(); err == nil {
			t.Fatal("过长 voice ID 应被拦截")
		}
	})

	// G2 修复：isValidTTSProvider 后缀语义检查
	t.Run("g2_rejects_tts_provider_without_tts_suffix", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.LLM.Providers = map[string]LLMProviderConfig{
			"deepseek": {APIKey: "sk-xxx"},
		}
		// deepseek-stt 是 STT 不是 TTS，即使 llm.providers["deepseek"] 存在也应拒绝
		cfg.Voice.TTS.Provider = "deepseek-stt"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("deepseek-stt 语义错位（不是 TTS 后缀），应被拦截")
		}
		if !strings.Contains(err.Error(), "voice.tts.provider") {
			t.Errorf("错误应指出字段：%v", err)
		}
	})

	t.Run("g2_accepts_llm_prefixed_tts", func(t *testing.T) {
		cfg := &Config{}
		cfg.Server.Port = 16060
		cfg.Server.MCPPort = 16070
		cfg.Voice.Enabled = true
		cfg.Voice.TTS.Provider = "deepseek-tts"
		cfg.LLM.Providers = map[string]LLMProviderConfig{
			"deepseek": {APIKey: "sk-xxx"},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("deepseek-tts + providers.deepseek 应通过：%v", err)
		}
	})
}
