// Package audit 提供安全审计能力
//
// 通过 `hexclaw security audit` 命令执行一键安全检查，
// 涵盖配置安全、网络暴露、工具权限、凭证泄露等维度。
//
// 审计结果按严重等级分类：
//   - Critical: 必须立即修复
//   - High: 强烈建议修复
//   - Medium: 建议修复
//   - Low: 可选优化
//
// 对标 OpenClaw `openclaw security audit`。
//
// 用法：
//
//	checker := audit.NewChecker(cfg)
//	report := checker.Run()
//	fmt.Println(report.Summary())
package audit

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/internal/config"
)

// Severity 严重等级
type Severity string

const (
	SeverityCritical Severity = "critical" // 必须立即修复
	SeverityHigh     Severity = "high"     // 强烈建议修复
	SeverityMedium   Severity = "medium"   // 建议修复
	SeverityLow      Severity = "low"      // 可选优化
	SeverityPass     Severity = "pass"     // 检查通过
)

// Category 检查类别
type Category string

const (
	CategoryConfig    Category = "config"    // 配置安全
	CategoryNetwork   Category = "network"   // 网络暴露
	CategoryAuth      Category = "auth"      // 认证授权
	CategoryTool      Category = "tool"      // 工具权限
	CategoryData      Category = "data"      // 数据安全
	CategoryRuntime   Category = "runtime"   // 运行时安全
)

// Finding 审计发现
type Finding struct {
	ID          string   `json:"id"`          // 检查项 ID
	Title       string   `json:"title"`       // 标题
	Description string   `json:"description"` // 详细描述
	Severity    Severity `json:"severity"`    // 严重等级
	Category    Category `json:"category"`    // 检查类别
	Fix         string   `json:"fix"`         // 修复建议
}

// Report 审计报告
type Report struct {
	Timestamp time.Time `json:"timestamp"`  // 审计时间
	Findings  []Finding `json:"findings"`   // 发现列表
	Duration  int64     `json:"duration_ms"` // 审计耗时（毫秒）
}

// Summary 生成人类可读的报告摘要
func (r *Report) Summary() string {
	var sb strings.Builder

	sb.WriteString("╔══════════════════════════════════════╗\n")
	sb.WriteString("║       HexClaw 安全审计报告            ║\n")
	sb.WriteString("╚══════════════════════════════════════╝\n\n")

	sb.WriteString(fmt.Sprintf("审计时间: %s\n", r.Timestamp.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("耗时: %dms\n\n", r.Duration))

	// 统计各等级数量
	counts := map[Severity]int{}
	for _, f := range r.Findings {
		counts[f.Severity]++
	}

	sb.WriteString("== 概要 ==\n")
	sb.WriteString(fmt.Sprintf("  Critical: %d\n", counts[SeverityCritical]))
	sb.WriteString(fmt.Sprintf("  High:     %d\n", counts[SeverityHigh]))
	sb.WriteString(fmt.Sprintf("  Medium:   %d\n", counts[SeverityMedium]))
	sb.WriteString(fmt.Sprintf("  Low:      %d\n", counts[SeverityLow]))
	sb.WriteString(fmt.Sprintf("  Pass:     %d\n\n", counts[SeverityPass]))

	// 按严重等级排序输出
	for _, sev := range []Severity{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow} {
		for _, f := range r.Findings {
			if f.Severity != sev {
				continue
			}
			icon := severityIcon(f.Severity)
			sb.WriteString(fmt.Sprintf("%s [%s] %s\n", icon, f.Category, f.Title))
			sb.WriteString(fmt.Sprintf("   %s\n", f.Description))
			if f.Fix != "" {
				sb.WriteString(fmt.Sprintf("   修复: %s\n", f.Fix))
			}
			sb.WriteString("\n")
		}
	}

	total := counts[SeverityCritical] + counts[SeverityHigh] + counts[SeverityMedium] + counts[SeverityLow]
	if total == 0 {
		sb.WriteString("所有检查项均通过，配置安全。\n")
	} else {
		sb.WriteString(fmt.Sprintf("共发现 %d 个安全问题，请及时修复。\n", total))
	}

	return sb.String()
}

// HasCritical 是否有 Critical 级别的发现
func (r *Report) HasCritical() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// CountBySeverity 按严重等级统计
func (r *Report) CountBySeverity(sev Severity) int {
	count := 0
	for _, f := range r.Findings {
		if f.Severity == sev {
			count++
		}
	}
	return count
}

func severityIcon(sev Severity) string {
	switch sev {
	case SeverityCritical:
		return "[CRITICAL]"
	case SeverityHigh:
		return "[HIGH]    "
	case SeverityMedium:
		return "[MEDIUM]  "
	case SeverityLow:
		return "[LOW]     "
	default:
		return "[PASS]    "
	}
}

// ============== 检查器 ==============

// Checker 安全检查器
//
// 执行一系列安全检查规则，生成审计报告。
type Checker struct {
	cfg *config.Config
}

// NewChecker 创建安全检查器
func NewChecker(cfg *config.Config) *Checker {
	return &Checker{cfg: cfg}
}

// Run 执行安全审计
//
// 运行所有检查规则，返回审计报告。
func (c *Checker) Run() *Report {
	start := time.Now()

	var findings []Finding

	// 1. 配置安全检查
	findings = append(findings, c.checkAPIKeys()...)
	findings = append(findings, c.checkServerConfig()...)

	// 2. 认证授权检查
	findings = append(findings, c.checkAuth()...)

	// 3. 网络暴露检查
	findings = append(findings, c.checkNetwork()...)

	// 4. 工具权限检查
	findings = append(findings, c.checkToolPermissions()...)

	// 5. 数据安全检查
	findings = append(findings, c.checkDataSecurity()...)

	// 6. 运行时安全检查
	findings = append(findings, c.checkRuntime()...)

	return &Report{
		Timestamp: time.Now(),
		Findings:  findings,
		Duration:  time.Since(start).Milliseconds(),
	}
}

// ============== 检查规则 ==============

// checkAPIKeys 检查 API Key 配置安全
func (c *Checker) checkAPIKeys() []Finding {
	var findings []Finding

	for name, provider := range c.cfg.LLM.Providers {
		if provider.APIKey == "" {
			continue
		}

		// 检查硬编码 API Key（非环境变量引用）
		if !strings.HasPrefix(provider.APIKey, "${") && !strings.HasPrefix(provider.APIKey, "$") {
			// 检查是否是明文 Key（非环境变量）
			if isLikelyAPIKey(provider.APIKey) {
				findings = append(findings, Finding{
					ID:          "api-key-hardcoded-" + name,
					Title:       fmt.Sprintf("API Key 硬编码: %s", name),
					Description: fmt.Sprintf("Provider %q 的 API Key 疑似直接写在配置文件中", name),
					Severity:    SeverityCritical,
					Category:    CategoryConfig,
					Fix:         "使用环境变量: api_key: ${" + strings.ToUpper(name) + "_API_KEY}",
				})
			}
		}
	}

	return findings
}

// checkServerConfig 检查服务器配置
func (c *Checker) checkServerConfig() []Finding {
	var findings []Finding

	// 检查是否绑定到 0.0.0.0
	if c.cfg.Server.Host == "0.0.0.0" || c.cfg.Server.Host == "" {
		findings = append(findings, Finding{
			ID:          "server-bind-all",
			Title:       "服务绑定到所有接口",
			Description: "服务器监听 0.0.0.0，暴露在所有网络接口上",
			Severity:    SeverityHigh,
			Category:    CategoryNetwork,
			Fix:         "生产环境建议绑定到 127.0.0.1 或内网 IP",
		})
	}

	// 检查运行模式
	if c.cfg.Server.Mode == "development" || c.cfg.Server.Mode == "" {
		findings = append(findings, Finding{
			ID:          "server-dev-mode",
			Title:       "开发模式运行",
			Description: "服务器以开发模式运行，可能暴露调试信息",
			Severity:    SeverityMedium,
			Category:    CategoryRuntime,
			Fix:         "生产环境设置 mode: production",
		})
	}

	return findings
}

// checkAuth 检查认证配置
func (c *Checker) checkAuth() []Finding {
	var findings []Finding

	if !c.cfg.Security.Auth.Enabled {
		findings = append(findings, Finding{
			ID:          "auth-disabled",
			Title:       "认证未启用",
			Description: "API 端点无认证保护，任何人都可以访问",
			Severity:    SeverityHigh,
			Category:    CategoryAuth,
			Fix:         "启用认证: security.auth.enabled: true",
		})
	}

	if c.cfg.Security.Auth.AllowAnonymous {
		findings = append(findings, Finding{
			ID:          "auth-anonymous",
			Title:       "允许匿名访问",
			Description: "允许未认证用户访问 API",
			Severity:    SeverityMedium,
			Category:    CategoryAuth,
			Fix:         "生产环境禁用匿名访问: allow_anonymous: false",
		})
	}

	return findings
}

// checkNetwork 检查网络暴露
func (c *Checker) checkNetwork() []Finding {
	var findings []Finding

	// 检查速率限制
	if c.cfg.Security.RateLimit.RequestsPerMinute == 0 && c.cfg.Security.RateLimit.RequestsPerHour == 0 {
		findings = append(findings, Finding{
			ID:          "rate-limit-disabled",
			Title:       "未配置速率限制",
			Description: "API 未设置请求频率限制，可能遭受 DDoS 或滥用",
			Severity:    SeverityMedium,
			Category:    CategoryNetwork,
			Fix:         "设置速率限制: requests_per_minute: 60",
		})
	}

	return findings
}

// checkToolPermissions 检查工具权限
func (c *Checker) checkToolPermissions() []Finding {
	var findings []Finding

	// 检查沙箱
	if c.cfg.Skill.Sandbox.Enabled {
		// 沙箱已启用 - 检查配置细节
		if len(c.cfg.Skill.Sandbox.Network.AllowedDomains) == 0 {
			findings = append(findings, Finding{
				ID:          "sandbox-network-open",
				Title:       "沙箱网络无限制",
				Description: "Skill 沙箱未限制网络访问域名",
				Severity:    SeverityLow,
				Category:    CategoryTool,
				Fix:         "配置网络白名单: allowed_domains: [api.example.com]",
			})
		}

		if len(c.cfg.Skill.Sandbox.Filesystem.AllowedPaths) == 0 {
			findings = append(findings, Finding{
				ID:          "sandbox-fs-open",
				Title:       "沙箱文件系统无限制",
				Description: "Skill 沙箱未限制文件系统访问路径",
				Severity:    SeverityLow,
				Category:    CategoryTool,
				Fix:         "配置文件白名单: allowed_paths: [/tmp]",
			})
		}

		findings = append(findings, Finding{
			ID:       "sandbox-enabled",
			Title:    "Skill 沙箱已启用",
			Severity: SeverityPass,
			Category: CategoryTool,
		})
	} else {
		findings = append(findings, Finding{
			ID:          "sandbox-disabled",
			Title:       "Skill 沙箱未启用",
			Description: "Skill 在无沙箱保护的环境中执行",
			Severity:    SeverityHigh,
			Category:    CategoryTool,
			Fix:         "启用沙箱: skill.sandbox.enabled: true",
		})
	}

	return findings
}

// checkDataSecurity 检查数据安全
func (c *Checker) checkDataSecurity() []Finding {
	var findings []Finding

	// 检查注入检测
	if !c.cfg.Security.InjectionDetection.Enabled {
		findings = append(findings, Finding{
			ID:          "injection-disabled",
			Title:       "Prompt 注入检测未启用",
			Description: "未启用 Prompt 注入检测，Agent 可能被恶意 prompt 劫持",
			Severity:    SeverityMedium,
			Category:    CategoryData,
			Fix:         "启用注入检测: injection_detection.enabled: true",
		})
	}

	// 检查 PII 脱敏
	if !c.cfg.Security.PIIRedaction.Enabled {
		findings = append(findings, Finding{
			ID:          "pii-disabled",
			Title:       "PII 脱敏未启用",
			Description: "个人身份信息可能被发送到 LLM Provider",
			Severity:    SeverityMedium,
			Category:    CategoryData,
			Fix:         "启用 PII 脱敏: pii_redaction.enabled: true",
		})
	}

	// 检查成本控制
	if c.cfg.Security.Cost.BudgetGlobal == 0 && c.cfg.Security.Cost.BudgetPerUser == 0 {
		findings = append(findings, Finding{
			ID:          "cost-no-budget",
			Title:       "未设置成本预算",
			Description: "未设置 LLM 调用预算限制，可能产生意外高额费用",
			Severity:    SeverityMedium,
			Category:    CategoryData,
			Fix:         "设置预算: budget_global: 100.0, budget_per_user: 10.0",
		})
	}

	return findings
}

// checkRuntime 检查运行时安全
func (c *Checker) checkRuntime() []Finding {
	var findings []Finding

	// 检查数据库路径权限
	if c.cfg.Storage.SQLite.Path != "" {
		info, err := os.Stat(c.cfg.Storage.SQLite.Path)
		if err == nil {
			mode := info.Mode()
			if mode&0o077 != 0 {
				findings = append(findings, Finding{
					ID:          "db-permissions",
					Title:       "数据库文件权限过宽",
					Description: fmt.Sprintf("数据库文件 %s 权限为 %s，其他用户可能读取", c.cfg.Storage.SQLite.Path, mode),
					Severity:    SeverityMedium,
					Category:    CategoryRuntime,
					Fix:         fmt.Sprintf("chmod 600 %s", c.cfg.Storage.SQLite.Path),
				})
			}
		}
	}

	// 检查内容过滤
	if !c.cfg.Security.ContentFilter.Enabled {
		findings = append(findings, Finding{
			ID:          "content-filter-disabled",
			Title:       "内容过滤未启用",
			Description: "未启用内容过滤，Agent 可能输出不当内容",
			Severity:    SeverityLow,
			Category:    CategoryRuntime,
			Fix:         "启用内容过滤: content_filter.enabled: true",
		})
	}

	return findings
}

// isLikelyAPIKey 判断字符串是否看起来像 API Key
func isLikelyAPIKey(s string) bool {
	// API Key 通常包含字母数字和横线，长度 > 20
	if len(s) < 20 {
		return false
	}
	// 常见的 API Key 前缀
	prefixes := []string{"sk-", "key-", "ak-", "gsk_", "AIza"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
