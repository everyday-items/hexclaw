package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexagon"
)

// condition 节点 schema（node.Data）：
//
//	{
//	  "type": "condition",
//	  "data": {
//	    "source": "input" | "<nodeID>" | "",   // 取值来源；"" = 解析后的 inputText（上游合并输出）
//	    "conditions": [
//	      { "op": "eq|ne|contains|not_contains|gt|lt|gte|lte|regex|empty|not_empty",
//	        "value": "<字面量>", "target": "<下游 nodeID>" }
//	    ],
//	    "default": "<下游 nodeID>"              // 可选：无规则命中时激活的分支
//	  }
//	}
//
// 语义：按序求值 conditions，首个命中的规则 → 其 target 分支激活；未命中则激活 default（若配置）。
// 本 condition 节点所有「未激活」的出边被停用，其下游整枝在拓扑序被跳过（nodeDeactivatedByBranch）。
// conditions 为空 → 直通（所有出边保持激活，向后兼容占位节点）。

type conditionRule struct {
	op     string
	value  string
	target string
}

// executeCondition 求值 condition 节点，停用未选中出边，返回透传 output。
func (e *workflowExecutor) executeCondition(state hexagon.MapState, node *workflowNode, inputText string) (hexagon.MapState, string, error) {
	rules, defaultTarget, err := parseConditionRules(node)
	if err != nil {
		return state, "", err
	}

	outgoing := e.outgoing[node.ID]
	// 无条件规则 = 占位/直通：保持所有出边激活，透传输入（不停用任何分支）。
	if len(rules) == 0 && defaultTarget == "" {
		return state, inputText, nil
	}

	subject := e.conditionSubject(state, node, inputText)

	// 选出激活的 target（首个命中规则的 target，否则 default）。
	activeTarget := defaultTarget
	for _, r := range rules {
		if evalConditionOp(subject, r.op, r.value) {
			activeTarget = r.target
			break
		}
	}

	// 校验 activeTarget 确为本节点出边之一（若非空）；停用其余出边。
	activeValid := activeTarget == ""
	for _, target := range outgoing {
		if target == activeTarget {
			activeValid = true
			continue
		}
		state = e.deactivateEdge(state, node.ID, target)
	}
	if activeTarget != "" && !activeValid {
		return state, "", fmt.Errorf("condition 节点 %q 命中分支 target=%q 不是它的任何一条出边", node.ID, activeTarget)
	}
	// activeTarget 为空（无规则命中且无 default）→ 所有出边已停用，整个下游停枝。
	return state, inputText, nil
}

// conditionSubject 取待判定的字符串值：source=input → 工作流输入；source=<nodeID> → 该节点输出；
// 否则用解析后的 inputText（上游合并输出）。
func (e *workflowExecutor) conditionSubject(state hexagon.MapState, node *workflowNode, inputText string) string {
	source := strings.TrimSpace(stringValue(node.Data["source"]))
	switch source {
	case "":
		return inputText
	case "input":
		return stringStateValue(state, stateKeyInput)
	default:
		outputs := stringMapStateValue(state, stateKeyNodeOutputs)
		if v, ok := outputs[source]; ok {
			return v
		}
		return inputText
	}
}

func parseConditionRules(node *workflowNode) ([]conditionRule, string, error) {
	defaultTarget := strings.TrimSpace(stringValue(node.Data["default"]))
	raw, ok := node.Data["conditions"]
	if !ok || raw == nil {
		return nil, defaultTarget, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, "", fmt.Errorf("condition 节点 %q 的 conditions 必须是数组", node.ID)
	}
	rules := make([]conditionRule, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("condition 节点 %q 的 conditions[%d] 必须是对象", node.ID, i)
		}
		op := strings.TrimSpace(strings.ToLower(stringValue(m["op"])))
		if op == "" {
			return nil, "", fmt.Errorf("condition 节点 %q 的 conditions[%d] 缺少 op", node.ID, i)
		}
		if !validConditionOp(op) {
			return nil, "", fmt.Errorf("condition 节点 %q 的 conditions[%d] op=%q 不支持", node.ID, i, op)
		}
		target := strings.TrimSpace(stringValue(m["target"]))
		if target == "" {
			return nil, "", fmt.Errorf("condition 节点 %q 的 conditions[%d] 缺少 target（激活的下游 nodeID）", node.ID, i)
		}
		rules = append(rules, conditionRule{op: op, value: stringValue(m["value"]), target: target})
	}
	return rules, defaultTarget, nil
}

func validConditionOp(op string) bool {
	switch op {
	case "eq", "ne", "contains", "not_contains", "gt", "lt", "gte", "lte", "regex", "empty", "not_empty":
		return true
	}
	return false
}

// evalConditionOp 对 subject 求值单条运算符。数值运算符两侧可解析为数字时按数值比较，否则按字符串。
func evalConditionOp(subject, op, value string) bool {
	switch op {
	case "eq":
		return subject == value
	case "ne":
		return subject != value
	case "contains":
		return strings.Contains(subject, value)
	case "not_contains":
		return !strings.Contains(subject, value)
	case "empty":
		return strings.TrimSpace(subject) == ""
	case "not_empty":
		return strings.TrimSpace(subject) != ""
	case "regex":
		re, err := regexp.Compile(value)
		if err != nil {
			return false
		}
		return re.MatchString(subject)
	case "gt", "lt", "gte", "lte":
		return evalNumericOp(subject, op, value)
	}
	return false
}

func evalNumericOp(subject, op, value string) bool {
	sn, se := strconv.ParseFloat(strings.TrimSpace(subject), 64)
	vn, ve := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if se == nil && ve == nil {
		switch op {
		case "gt":
			return sn > vn
		case "lt":
			return sn < vn
		case "gte":
			return sn >= vn
		case "lte":
			return sn <= vn
		}
		return false
	}
	// 非数字 → 按字符串字典序比较（稳定、可预期）。
	switch op {
	case "gt":
		return subject > value
	case "lt":
		return subject < value
	case "gte":
		return subject >= value
	case "lte":
		return subject <= value
	}
	return false
}

// ── 分支存活/停用：基于 deadNodes + deactivatedEdges 的入边 liveness ──

func edgeKey(source, target string) string { return source + "|" + target }

// deactivateEdge 停用一条出边（source→target）。
func (e *workflowExecutor) deactivateEdge(state hexagon.MapState, source, target string) hexagon.MapState {
	return putStringMapStateValue(state, stateKeyDeactivatedEdges, edgeKey(source, target), "1")
}

// markNodeDead 把节点标记为 dead（被分支跳过），供下游 liveness 传播。
func (e *workflowExecutor) markNodeDead(state hexagon.MapState, nodeID string) hexagon.MapState {
	return putStringMapStateValue(state, stateKeyDeadNodes, nodeID, "1")
}

// nodeDeactivatedByBranch 判断节点是否应因 condition 分支而整枝跳过：
// 有入边但没有任何一条 live 入边。入边 src→node live 当且仅当 src 未 dead 且该边未被停用。
// 根节点（无入边）永远存活。
func (e *workflowExecutor) nodeDeactivatedByBranch(state hexagon.MapState, node *workflowNode) bool {
	preds := e.incoming[node.ID]
	if len(preds) == 0 {
		return false
	}
	dead := stringMapStateValue(state, stateKeyDeadNodes)
	deadEdges := stringMapStateValue(state, stateKeyDeactivatedEdges)
	for _, src := range preds {
		if dead[src] == "1" {
			continue
		}
		if deadEdges[edgeKey(src, node.ID)] == "1" {
			continue
		}
		return false // 找到一条 live 入边 → 节点存活
	}
	return true // 全部入边失效 → 整枝跳过
}
