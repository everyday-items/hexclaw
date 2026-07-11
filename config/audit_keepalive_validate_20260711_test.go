package config

import (
	"strings"
	"testing"
)

// C4 keep_alive 全链零校验。
//
// PUT /config/llm 原样存 KeepAlive；config/validate.go 无 keep_alive 分支；llmrouter 原样
// 下发；ai-core WithKeepAlive 只赋值不 parse。非法值如 "banana" 直达 Ollama 首次聊天才 400。
// 边界应在配置校验拦住：Ollama 接受 Go duration（"30m"/"2h"）、纯秒整数（"3600"）、-1
// （永久驻留）、0（立即卸载）。
//
// 真断言：Config.Validate() 对非法 keep_alive 返回校验错误（字段路径命中 keep_alive），
// 合法值放行。

func baseConfigWithKeepAlive(ka string) *Config {
	c := DefaultConfig()
	c.LLM.Providers = map[string]LLMProviderConfig{
		"ollama": {BaseURL: "http://localhost:11434", Model: "qwen3:8b", KeepAlive: ka},
	}
	return c
}

func TestValidate_RejectsIllegalKeepAlive(t *testing.T) {
	c := baseConfigWithKeepAlive("banana")
	err := c.Validate()
	if err == nil {
		t.Fatalf("非法 keep_alive=\"banana\" 未被校验拦截（直达 Ollama 首聊才 400）")
	}
	if !strings.Contains(err.Error(), "keep_alive") {
		t.Fatalf("校验错误未指向 keep_alive 字段：%v", err)
	}
}

func TestValidate_AcceptsLegalKeepAlive(t *testing.T) {
	for _, ka := range []string{"", "30m", "2h", "-1", "0", "3600", "90s"} {
		c := baseConfigWithKeepAlive(ka)
		if err := c.Validate(); err != nil {
			t.Fatalf("合法 keep_alive=%q 被误拦：%v", ka, err)
		}
	}
}

func TestIsValidKeepAlive(t *testing.T) {
	legal := []string{"", "30m", "2h", "1h30m", "-1", "0", "3600", "90s", "-1m"}
	illegal := []string{"banana", "30 m", "abc", "m30", "3600x", "12.5"}
	for _, s := range legal {
		if !IsValidKeepAlive(s) {
			t.Fatalf("IsValidKeepAlive(%q) 应为合法", s)
		}
	}
	for _, s := range illegal {
		if IsValidKeepAlive(s) {
			t.Fatalf("IsValidKeepAlive(%q) 应为非法", s)
		}
	}
}
