// goal.go 实现 Loop Engineering #1：**目标形式化**。
//
// 现状的 orchestrate 收敛环里，goal 是一个自然语言字符串，监工返回的 {"done":true}
// 是一句 LLM 主观判断——没有可机器判定的"完成"。控制论里那句话是硬的：
//
//	Control = Goal − Observed State
//
// 测不出 Observed State、定不清 Goal，收敛就无从谈起。本文件把"意图"和"完成判据"
// 拆开：
//
//   - Intent     给 maker 看——自然语言，描述要干什么。
//   - Acceptance 给 checker 用——一组**可机器判定**的验收断言；全过 = 达成。
//
// 这样 done 从"LLM 觉得差不多了"升级成"验收断言全过"，逼近 K8s 那句 actual==desired
// 的确定性。Acceptance 为空时，loop 退化到纯 LLM 裁判（行为不变，但会标注为
// "无形式化验收"以便观测）。
//
// Goal 是纯数据 + 可序列化的，方便落盘（#5 状态外置）与跨 tick 续跑（#6）。
package engine

import (
	"fmt"
	"strings"
	"time"
)

// AcceptanceKind 是一条验收断言的判定方式。全部可在本地确定性执行（llm_rubric 除外，
// 它需要一个注入的裁判）。
type AcceptanceKind string

const (
	// AcceptContains：产出（最终文本）包含某子串。
	AcceptContains AcceptanceKind = "contains"
	// AcceptRegex：产出匹配某正则。
	AcceptRegex AcceptanceKind = "regex"
	// AcceptJSONField：把产出当 JSON（或抠出其中第一个 JSON 对象），按点分 Path
	// 取字段，字符串化后与 Value 相等。
	AcceptJSONField AcceptanceKind = "json_field"
	// AcceptCommand：在工作区跑一条命令，要求 exit 0；Pattern 非空时还要求 stdout 匹配。
	// 命令是**目标作者声明的可信断言**（如 "go test ./..."），不是 LLM 现写的代码——
	// 后者绝不该走这条。引擎默认不带 CommandRunner，命令断言会被判 inconclusive。
	AcceptCommand AcceptanceKind = "command"
	// AcceptLLMRubric：让一个独立裁判按 rubric 对产出打 0-10 分，>= Threshold 算过。
	// 兜主观维度（"语气是否专业""结构是否清晰"），是确定性断言的补充、不是替代。
	AcceptLLMRubric AcceptanceKind = "llm_rubric"
)

// AcceptanceSpec 是一条声明式验收断言（可序列化、可落盘）。按 Kind 取用对应字段。
type AcceptanceSpec struct {
	Kind        AcceptanceKind `json:"kind"`
	Description string         `json:"description,omitempty"` // 人类可读：这条在验什么

	// 多义字段，按 Kind 解释：
	//   contains    → Value: 子串
	//   regex       → Value: 正则
	//   json_field  → Path: 点分字段路径；Value: 期望值（字符串比较）
	//   command     → Value: 命令；Pattern: stdout 断言正则（可选）
	//   llm_rubric  → Value: rubric 文本；Threshold: 及格分(0-10)
	Value     string `json:"value,omitempty"`
	Path      string `json:"path,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	Threshold int    `json:"threshold,omitempty"`

	// Weight 是该断言在加权得分里的权重（<=0 视作 1）。
	Weight int `json:"weight,omitempty"`
	// Optional 为真时，这条不过不致命——只影响 Score，不阻止 Passed。
	Optional bool `json:"optional,omitempty"`
}

// effectiveWeight 返回归一化后的权重（最小 1）。
func (s AcceptanceSpec) effectiveWeight() int {
	if s.Weight <= 0 {
		return 1
	}
	return s.Weight
}

// Validate 校验单条断言的字段齐备性。
func (s AcceptanceSpec) Validate() error {
	switch s.Kind {
	case AcceptContains, AcceptRegex, AcceptCommand, AcceptLLMRubric:
		if strings.TrimSpace(s.Value) == "" {
			return fmt.Errorf("acceptance kind %q requires a non-empty value", s.Kind)
		}
	case AcceptJSONField:
		if strings.TrimSpace(s.Path) == "" {
			return fmt.Errorf("acceptance kind json_field requires a path")
		}
	default:
		return fmt.Errorf("unknown acceptance kind %q", s.Kind)
	}
	if s.Kind == AcceptLLMRubric && (s.Threshold < 1 || s.Threshold > 10) {
		return fmt.Errorf("llm_rubric threshold must be in [1,10], got %d（0 会让 score>=0 恒过，fail-open）", s.Threshold)
	}
	return nil
}

// GoalBudget 是目标级的成本上限——回答"这件事最多花多少就停"。
// 任一项为 0 表示用 reconciler 的默认值（见 goalDefaults）。
type GoalBudget struct {
	MaxRounds int           `json:"max_rounds,omitempty"` // 最大迭代轮数
	MaxWall   time.Duration `json:"max_wall,omitempty"`   // 总墙钟
	MaxTokens int           `json:"max_tokens,omitempty"` // 目标级 token 预算（0 = 不计）
}

// EscalationPolicy 是 HITL（#8）升级策略——loop 处理不了时把决定权交回给人，
// 而不是静默截断或空转烧 token。
type EscalationPolicy struct {
	// NoProgressRounds：连续 N 轮无实质进展即升级（0 = 用默认）。
	NoProgressRounds int `json:"no_progress_rounds,omitempty"`
	// OnHighRisk：本轮触达高风险动作（被风险闸判 high）即升级。
	OnHighRisk bool `json:"on_high_risk,omitempty"`
	// MinConfidence：checker 置信度低于此即升级（0 = 不启用，取值 0..1）。
	MinConfidence float64 `json:"min_confidence,omitempty"`
}

// Goal 是一个可被 reconcile loop 驱动到"可机器判定完成"的结构化目标。
type Goal struct {
	ID         string            `json:"id"`
	Intent     string            `json:"intent"`               // 自然语言意图，喂给 maker
	Acceptance []AcceptanceSpec  `json:"acceptance,omitempty"` // 可机器判定的验收断言（全过 = 达成）
	Budget     GoalBudget        `json:"budget,omitempty"`
	Escalation EscalationPolicy  `json:"escalation,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// goalDefaults 是预算/升级的兜底默认值。保守取值，贴单机桌面规模。
var goalDefaults = struct {
	MaxRounds        int
	MaxWall          time.Duration
	NoProgressRounds int
}{
	MaxRounds:        5,
	MaxWall:          10 * time.Minute,
	NoProgressRounds: 2,
}

// Validate 校验 Goal 的最小完整性：必须有意图；每条验收断言字段齐备。
func (g *Goal) Validate() error {
	if g == nil {
		return fmt.Errorf("goal is nil")
	}
	if strings.TrimSpace(g.Intent) == "" {
		return fmt.Errorf("goal.intent is required")
	}
	for i, s := range g.Acceptance {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("goal.acceptance[%d]: %w", i, err)
		}
	}
	if g.Escalation.MinConfidence < 0 || g.Escalation.MinConfidence > 1 {
		return fmt.Errorf("goal.escalation.min_confidence must be in [0,1], got %v", g.Escalation.MinConfidence)
	}
	return nil
}

// WithDefaults 返回一个把零值预算/升级项填上默认值的副本（不改原对象）。
// reconciler 在跑之前调它，使后续逻辑无需到处判零。
func (g Goal) WithDefaults() Goal {
	out := g
	if out.Budget.MaxRounds <= 0 {
		out.Budget.MaxRounds = goalDefaults.MaxRounds
	}
	if out.Budget.MaxWall <= 0 {
		out.Budget.MaxWall = goalDefaults.MaxWall
	}
	if out.Escalation.NoProgressRounds <= 0 {
		out.Escalation.NoProgressRounds = goalDefaults.NoProgressRounds
	}
	return out
}

// HasFormalAcceptance 报告该目标是否带了至少一条可机器判定的验收断言。
// 没有时，loop 只能退化到纯 LLM 裁判——可观测层据此标注"无形式化验收"。
func (g *Goal) HasFormalAcceptance() bool {
	return g != nil && len(g.Acceptance) > 0
}
