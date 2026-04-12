package gateway

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// InputSafetyLayer 输入安全层 (Layer 4)
//
// 利用 hexagon/security/guard 进行输入安全检查：
//   - Prompt 注入检测
//   - PII 自动脱敏
//   - 内容过滤（有害/违法内容检测）
//
// 所有检查通过 hexagon 框架的 Guard 接口实现，
// 不重复造轮子。
type InputSafetyLayer struct {
	injectionGuard *hexagon.PromptInjectionGuard
	piiGuard       *hexagon.PIIGuard
	guardChain     *hexagon.GuardChain
	cfg            *config.SecurityConfig
}

// NewInputSafetyLayer 创建输入安全层
func NewInputSafetyLayer(cfg *config.SecurityConfig) *InputSafetyLayer {
	l := &InputSafetyLayer{cfg: cfg}

	var guards []hexagon.Guard

	// 注入检测
	if cfg.InjectionDetection.Enabled {
		l.injectionGuard = hexagon.NewPromptInjectionGuard()
		guards = append(guards, l.injectionGuard)
	}

	// PII 检测
	if cfg.PIIRedaction.Enabled {
		l.piiGuard = hexagon.NewPIIGuard()
		guards = append(guards, l.piiGuard)
	}

	// 内容过滤
	if cfg.ContentFilter.Enabled {
		cfGuard := newContentFilterGuard(cfg.ContentFilter.BlockCategories)
		guards = append(guards, cfGuard)
		logger.Info("ContentFilter 已启用，阻断类别", "categories", cfg.ContentFilter.BlockCategories)
	}

	// 组装守卫链
	if len(guards) > 0 {
		l.guardChain = hexagon.NewGuardChain(hexagon.ChainModeAll, guards...)
	}

	return l
}

func (l *InputSafetyLayer) Name() string { return "input_safety" }

// Check 执行输入安全检查
//
// 检查内容包括：
//  1. Prompt 注入攻击检测
//  2. PII 信息检测（不阻止，但记录日志）
//  3. 如果检测到高风险注入，拒绝请求
func (l *InputSafetyLayer) Check(ctx context.Context, msg *adapter.Message) error {
	if l.guardChain == nil || msg.Content == "" {
		return nil
	}

	result, err := l.guardChain.Check(ctx, msg.Content)
	if err != nil {
		logger.Error("error", "error", err)
		return &GatewayError{
			Layer:   "input_safety",
			Code:    "safety_check_error",
			Message: "安全检查服务异常，请稍后重试",
		}
	}

	if !result.Passed {
		return &GatewayError{
			Layer:   "input_safety",
			Code:    "unsafe_input",
			Message: fmt.Sprintf("输入内容未通过安全检查: %s", result.Reason),
		}
	}

	return nil
}

// contentFilterGuard 基于关键词的内容过滤守卫
//
// 实现 guard.Guard 接口，根据配置的 blockCategories 匹配预定义关键词。
// 这是一个基础的本地规则引擎；后续可扩展为调用外部审核 API。
type contentFilterGuard struct {
	categories map[string][]string // category → keywords
	enabled    []string
}

var defaultBlockKeywords = map[string][]string{
	"violence":  {"杀人", "炸弹", "枪支", "暗杀", "爆炸物", "how to kill", "make a bomb", "weapon"},
	"illegal":   {"贩毒", "洗钱", "走私", "伪造", "黑客攻击", "drug trafficking", "money laundering"},
	"adult":     {"色情", "裸体", "性交", "pornography", "explicit sexual"},
	"self_harm": {"自杀方法", "自残", "suicide method", "self-harm instructions"},
}

func newContentFilterGuard(blockCategories []string) *contentFilterGuard {
	enabled := blockCategories
	if len(enabled) == 0 {
		enabled = []string{"violence", "illegal", "self_harm"}
	}
	return &contentFilterGuard{
		categories: defaultBlockKeywords,
		enabled:    enabled,
	}
}

func (g *contentFilterGuard) Name() string  { return "content_filter" }
func (g *contentFilterGuard) Enabled() bool { return len(g.enabled) > 0 }

// normalizeForFilter strips zero-width/format characters and lowercases input
// to prevent Unicode-based filter bypass (e.g. zero-width joiners between keyword characters).
func normalizeForFilter(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.Is(unicode.Cf, r) { // Cf = format characters (zero-width, etc.)
			b.WriteRune(r)
		}
	}
	return strings.ToLower(b.String())
}

func (g *contentFilterGuard) Check(_ context.Context, input string) (*hexagon.CheckResult, error) {
	lower := normalizeForFilter(input)
	var findings []hexagon.GuardFinding

	for _, cat := range g.enabled {
		keywords, ok := g.categories[cat]
		if !ok {
			continue
		}
		for _, kw := range keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				findings = append(findings, hexagon.GuardFinding{
					Type:     cat,
					Text:     kw,
					Severity: "high",
				})
			}
		}
	}

	if len(findings) > 0 {
		return &hexagon.CheckResult{
			Passed:   false,
			Score:    1.0,
			Category: findings[0].Type,
			Reason:   fmt.Sprintf("内容触发 %s 类过滤规则", findings[0].Type),
			Findings: findings,
		}, nil
	}

	return &hexagon.CheckResult{Passed: true, Score: 0}, nil
}
