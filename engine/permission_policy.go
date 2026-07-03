// permission_policy.go 实现 v0.4.0 H2 Permission Policy Engine（默认启用，可由 feature flag 关闭）。
//
// 老路径（PermissionHook.classifyRisk）只支持"硬编码工具名 → 风险等级"的二元分类，
// 表达力有限：无法基于参数、来源、会话元数据做精细决策；无法被用户在 settings 中
// 配置；无法对动态注册的 MCP 工具生效。
//
// PermissionPolicy 引入声明式规则：
//   - 每条规则匹配工具名 glob、source（skill/mcp）、参数 glob 模式
//   - 命中后产出 Action：allow / deny / require_approval（带 Risk 与 Reason）
//   - 多条规则按声明顺序，第一条命中即 short-circuit
//   - 未命中任何规则 → defaultAction（建议默认 allow，保留老 classifyRisk 黑名单兜底）
//
// feature flag `tool.policy.engine` 默认 ON。PermissionHook 优先调 PolicyEngine
// 评估；显式 OFF 时退化为老 classifyRisk 路径，用于回退。
package engine

import (
	"fmt"
	"path"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagToolPolicyEngine 控制 H2 PolicyEngine 是否生效。
const FlagToolPolicyEngine = "tool.policy.engine"

func init() {
	featureflag.Register(featureflag.Flag{
		Name: FlagToolPolicyEngine,
		// v0.4.3 §11.10 统一安全闸：GA 默认 ON —— PermissionPolicy 成为单一权限闸
		// （DefaultBaselinePolicy 与老 classifyRisk 黑名单等价 + send/media/publish 规则），
		// 配合无人值守风险顾问，取代 skill 层 cronConfirmer。
		Default:      true,
		Description:  "Use declarative PermissionPolicy rules instead of hardcoded dangerous/sensitive tool lists.",
		Stage:        featureflag.StageGA,
		SinceVersion: "0.4.0",
	})
}

// PolicyAction 是一条规则命中后建议采取的动作。
type PolicyAction string

const (
	// ActionAllow 直接放行，不询问用户。
	ActionAllow PolicyAction = "allow"
	// ActionDeny 直接拒绝，工具不会被执行。
	ActionDeny PolicyAction = "deny"
	// ActionRequireApproval 需要用户审批（走 PermissionHub）。
	ActionRequireApproval PolicyAction = "require_approval"
)

// PolicyRule 是一条权限规则。匹配 = 工具名 glob 命中 ∧ source 匹配 ∧ 所有 arg pattern 命中。
type PolicyRule struct {
	// Name 规则名，仅供审计 / 日志用。
	Name string
	// ToolPattern 工具名 glob：支持 path.Match 风格（"*" / "fs.*" / "shell"）；空串 = 任意工具。
	ToolPattern string
	// SourceMatch 工具来源："skill" | "mcp"；空串 = 任意来源。
	SourceMatch string
	// ArgumentMatch 参数键 → 值 glob；多个 key 之间是 AND 关系。
	// arg 不存在或值不匹配视作不命中此规则。
	// nil/empty 表示不约束参数。
	ArgumentMatch map[string]string
	// Action 命中后采取的动作。
	Action PolicyAction
	// Risk 命中时打到 PermissionRequest.Risk（"dangerous"/"sensitive"/"safe"）。
	Risk string
	// Reason 命中时打到 PermissionRequest.Reason，给用户看的解释。
	Reason string
}

// PermissionPolicy 是一组规则 + 默认动作的策略引擎。
//
// 用法：
//
//	p := engine.NewPermissionPolicy(engine.ActionAllow,
//	    engine.PolicyRule{Name: "block-shell", ToolPattern: "shell*", Action: engine.ActionDeny, Risk: "dangerous", Reason: "shell 执行不允许"},
//	    engine.PolicyRule{Name: "approve-fs-write", ToolPattern: "fs.write", Action: engine.ActionRequireApproval, Risk: "sensitive", Reason: "文件系统写入"},
//	)
//	dec := p.Evaluate(&engine.ToolCallInfo{Name: "shell", Source: "skill"})
//	// dec.Action == ActionDeny
type PermissionPolicy struct {
	rules         []PolicyRule
	defaultAction PolicyAction
}

// NewPermissionPolicy 构造一个 PermissionPolicy。
//
// defaultAction 是任何规则都未命中时的 fallback：通常用 ActionAllow（保留老
// classifyRisk 黑名单兜底）；如果你想要 default-deny 风格，传 ActionDeny 并
// 显式声明所有 allow 规则。
func NewPermissionPolicy(defaultAction PolicyAction, rules ...PolicyRule) *PermissionPolicy {
	if defaultAction == "" {
		defaultAction = ActionAllow
	}
	return &PermissionPolicy{rules: rules, defaultAction: defaultAction}
}

// PolicyDecision 是 Evaluate 的结果。
type PolicyDecision struct {
	Action      PolicyAction
	Risk        string
	Reason      string
	MatchedRule string // 命中规则的 Name；未命中时为 "<default>"
}

// Evaluate 按声明顺序匹配规则，第一个命中即返回；都不命中返回 defaultAction。
func (p *PermissionPolicy) Evaluate(call *ToolCallInfo) PolicyDecision {
	for _, r := range p.rules {
		if matchRule(r, call) {
			return PolicyDecision{
				Action:      r.Action,
				Risk:        r.Risk,
				Reason:      r.Reason,
				MatchedRule: r.Name,
			}
		}
	}
	return PolicyDecision{
		Action:      p.defaultAction,
		Risk:        "safe",
		MatchedRule: "<default>",
	}
}

func matchRule(r PolicyRule, call *ToolCallInfo) bool {
	if r.ToolPattern != "" && !globMatch(r.ToolPattern, call.Name) {
		return false
	}
	if r.SourceMatch != "" && r.SourceMatch != call.Source {
		return false
	}
	for argName, pattern := range r.ArgumentMatch {
		raw, ok := call.Arguments[argName]
		if !ok {
			return false
		}
		s, ok := raw.(string)
		if !ok {
			// 非字符串 arg 转 fmt 比对
			s = toString(raw)
		}
		if !globMatch(pattern, s) {
			return false
		}
	}
	return true
}

// globMatch 是 path.Match 的容错包装：pattern 非法时退化为 string equality。
// 注意 path.Match 的 "*" 不跨越 "/"；调用方需要跨段匹配时请用 "**"-style 自定义实现。
func globMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	matched, err := path.Match(pattern, s)
	if err != nil {
		return pattern == s
	}
	return matched
}

// toString 把任意类型转字符串用于 arg 模式比对（int/bool/数组等）。
func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
