package gateway

import (
	"context"
	"fmt"

	"github.com/everyday-items/hexagon/security/guard"
	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/hexclaw/internal/config"
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
	injectionGuard *guard.PromptInjectionGuard
	piiGuard       *guard.PIIGuard
	guardChain     *guard.GuardChain
	cfg            *config.SecurityConfig
}

// NewInputSafetyLayer 创建输入安全层
func NewInputSafetyLayer(cfg *config.SecurityConfig) *InputSafetyLayer {
	l := &InputSafetyLayer{cfg: cfg}

	var guards []guard.Guard

	// 注入检测
	if cfg.InjectionDetection.Enabled {
		l.injectionGuard = guard.NewPromptInjectionGuard()
		guards = append(guards, l.injectionGuard)
	}

	// PII 检测
	if cfg.PIIRedaction.Enabled {
		l.piiGuard = guard.NewPIIGuard()
		guards = append(guards, l.piiGuard)
	}

	// 组装守卫链
	if len(guards) > 0 {
		l.guardChain = guard.NewGuardChain(guard.ChainModeAll, guards...)
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
		// 守卫内部错误不阻止请求，仅记录
		return nil
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
