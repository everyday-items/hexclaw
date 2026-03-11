package audit

import (
	"strings"
	"testing"

	"github.com/everyday-items/hexclaw/config"
)

// TestChecker_AllPass 测试全部通过的安全配置
func TestChecker_AllPass(t *testing.T) {
	cfg := secureConfig()

	checker := NewChecker(cfg)
	report := checker.Run()

	if report.HasCritical() {
		t.Error("安全配置应无 Critical 发现")
	}

	critical := report.CountBySeverity(SeverityCritical)
	high := report.CountBySeverity(SeverityHigh)
	if critical > 0 || high > 0 {
		t.Errorf("安全配置不应有 Critical/High: critical=%d, high=%d", critical, high)
	}
}

// TestChecker_HardcodedAPIKey 测试检测硬编码 API Key
func TestChecker_HardcodedAPIKey(t *testing.T) {
	cfg := secureConfig()
	cfg.LLM.Providers["deepseek"] = config.LLMProviderConfig{
		APIKey: "sk-abcdefghijklmnopqrstuvwxyz123456", // 硬编码 API Key
	}

	checker := NewChecker(cfg)
	report := checker.Run()

	if !report.HasCritical() {
		t.Error("硬编码 API Key 应触发 Critical 发现")
	}

	found := false
	for _, f := range report.Findings {
		if strings.Contains(f.ID, "api-key-hardcoded") {
			found = true
			if f.Severity != SeverityCritical {
				t.Errorf("硬编码 API Key 严重等级应为 Critical，实际 %s", f.Severity)
			}
		}
	}
	if !found {
		t.Error("应包含 api-key-hardcoded 发现")
	}
}

// TestChecker_EnvVarAPIKey 测试环境变量引用不触发告警
func TestChecker_EnvVarAPIKey(t *testing.T) {
	cfg := secureConfig()
	cfg.LLM.Providers["deepseek"] = config.LLMProviderConfig{
		APIKey: "${DEEPSEEK_API_KEY}",
	}

	checker := NewChecker(cfg)
	report := checker.Run()

	for _, f := range report.Findings {
		if strings.Contains(f.ID, "api-key-hardcoded") {
			t.Error("环境变量引用不应触发硬编码 API Key 告警")
		}
	}
}

// TestChecker_ServerBindAll 测试检测 0.0.0.0 绑定
func TestChecker_ServerBindAll(t *testing.T) {
	cfg := secureConfig()
	cfg.Server.Host = "0.0.0.0"

	checker := NewChecker(cfg)
	report := checker.Run()

	found := false
	for _, f := range report.Findings {
		if f.ID == "server-bind-all" {
			found = true
			if f.Severity != SeverityHigh {
				t.Errorf("服务绑定所有接口应为 High，实际 %s", f.Severity)
			}
		}
	}
	if !found {
		t.Error("应检测到 0.0.0.0 绑定")
	}
}

// TestChecker_AuthDisabled 测试检测认证未启用
func TestChecker_AuthDisabled(t *testing.T) {
	cfg := secureConfig()
	cfg.Security.Auth.Enabled = false

	checker := NewChecker(cfg)
	report := checker.Run()

	found := false
	for _, f := range report.Findings {
		if f.ID == "auth-disabled" {
			found = true
			if f.Severity != SeverityHigh {
				t.Errorf("认证未启用应为 High，实际 %s", f.Severity)
			}
		}
	}
	if !found {
		t.Error("应检测到认证未启用")
	}
}

// TestChecker_SandboxDisabled 测试检测沙箱未启用
func TestChecker_SandboxDisabled(t *testing.T) {
	cfg := secureConfig()
	cfg.Skill.Sandbox.Enabled = false

	checker := NewChecker(cfg)
	report := checker.Run()

	found := false
	for _, f := range report.Findings {
		if f.ID == "sandbox-disabled" {
			found = true
			if f.Severity != SeverityHigh {
				t.Errorf("沙箱未启用应为 High，实际 %s", f.Severity)
			}
		}
	}
	if !found {
		t.Error("应检测到沙箱未启用")
	}
}

// TestChecker_RateLimitDisabled 测试检测未配置速率限制
func TestChecker_RateLimitDisabled(t *testing.T) {
	cfg := secureConfig()
	cfg.Security.RateLimit.RequestsPerMinute = 0
	cfg.Security.RateLimit.RequestsPerHour = 0

	checker := NewChecker(cfg)
	report := checker.Run()

	found := false
	for _, f := range report.Findings {
		if f.ID == "rate-limit-disabled" {
			found = true
			if f.Severity != SeverityMedium {
				t.Errorf("未配置速率限制应为 Medium，实际 %s", f.Severity)
			}
		}
	}
	if !found {
		t.Error("应检测到未配置速率限制")
	}
}

// TestChecker_InjectionDisabled 测试检测注入检测未启用
func TestChecker_InjectionDisabled(t *testing.T) {
	cfg := secureConfig()
	cfg.Security.InjectionDetection.Enabled = false

	checker := NewChecker(cfg)
	report := checker.Run()

	found := false
	for _, f := range report.Findings {
		if f.ID == "injection-disabled" {
			found = true
		}
	}
	if !found {
		t.Error("应检测到注入检测未启用")
	}
}

// TestChecker_NoBudget 测试检测未设置成本预算
func TestChecker_NoBudget(t *testing.T) {
	cfg := secureConfig()
	cfg.Security.Cost.BudgetGlobal = 0
	cfg.Security.Cost.BudgetPerUser = 0

	checker := NewChecker(cfg)
	report := checker.Run()

	found := false
	for _, f := range report.Findings {
		if f.ID == "cost-no-budget" {
			found = true
		}
	}
	if !found {
		t.Error("应检测到未设置成本预算")
	}
}

// TestReport_Summary 测试报告摘要生成
func TestReport_Summary(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Host: "0.0.0.0"},
		LLM:    config.LLMConfig{Providers: map[string]config.LLMProviderConfig{}},
	}

	checker := NewChecker(cfg)
	report := checker.Run()
	summary := report.Summary()

	// 应包含标题和统计
	if !strings.Contains(summary, "安全审计报告") {
		t.Error("摘要应包含标题")
	}
	if !strings.Contains(summary, "Critical:") {
		t.Error("摘要应包含 Critical 统计")
	}
	if !strings.Contains(summary, "审计时间:") {
		t.Error("摘要应包含审计时间")
	}
}

// TestReport_CountBySeverity 测试按严重等级统计
func TestReport_CountBySeverity(t *testing.T) {
	report := &Report{
		Findings: []Finding{
			{Severity: SeverityCritical},
			{Severity: SeverityHigh},
			{Severity: SeverityHigh},
			{Severity: SeverityMedium},
			{Severity: SeverityPass},
		},
	}

	if report.CountBySeverity(SeverityCritical) != 1 {
		t.Error("Critical 应为 1")
	}
	if report.CountBySeverity(SeverityHigh) != 2 {
		t.Error("High 应为 2")
	}
	if report.CountBySeverity(SeverityLow) != 0 {
		t.Error("Low 应为 0")
	}
}

// TestIsLikelyAPIKey 测试 API Key 识别
func TestIsLikelyAPIKey(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"sk-abcdefghijklmnopqrstuvwxyz", true},
		{"key-abcdefghijklmnopqrst12345", true},
		{"gsk_abcdefghijklmnopqrst12345", true},
		{"AIzaabcdefghijklmnopqrstuvw", true},
		{"short", false},                            // 太短
		{"${DEEPSEEK_API_KEY}", false},              // 不以已知前缀开头
		{"hello-world-not-a-key-really", false},     // 不以已知前缀开头
	}

	for _, tt := range tests {
		result := isLikelyAPIKey(tt.input)
		if result != tt.expected {
			t.Errorf("isLikelyAPIKey(%q) = %v, want %v", tt.input, result, tt.expected)
		}
	}
}

// secureConfig 创建一个安全的默认配置（所有安全选项都已启用）
func secureConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 6060,
			Mode: "production",
		},
		LLM: config.LLMConfig{
			Providers: map[string]config.LLMProviderConfig{
				"deepseek": {APIKey: "${DEEPSEEK_API_KEY}"},
			},
		},
		Security: config.SecurityConfig{
			Auth: config.AuthConfig{
				Enabled:        true,
				AllowAnonymous: false,
			},
			InjectionDetection: config.InjectionConfig{Enabled: true},
			PIIRedaction:       config.PIIRedactionConfig{Enabled: true},
			ContentFilter:      config.ContentFilterConfig{Enabled: true},
			Cost:               config.CostConfig{BudgetGlobal: 100, BudgetPerUser: 10},
			RateLimit:          config.RateLimitConfig{RequestsPerMinute: 60},
		},
		Skill: config.SkillConfig{
			Sandbox: config.SandboxConfig{
				Enabled: true,
				Network: config.SandboxNetwork{
					AllowedDomains: []string{"api.example.com"},
				},
				Filesystem: config.SandboxFilesystem{
					AllowedPaths: []string{"/tmp"},
				},
			},
		},
	}
}
