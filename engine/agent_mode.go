// agent_mode.go 提供 K12 场景下的 Agent 策略模式。
//
// 设计选择：HexClaw.ReActEngine 已是完整自研工具循环引擎（tool cache / budget /
// compactor / streaming / reasoning / multi-provider），不接入 hexagon Agent（那会
// 丢失所有企业级能力）。本模块通过**系统提示差异化**在 ReActEngine 上叠加模式：
//
//   - react          — 默认 ReAct，工具 + 推理交替（现有行为）
//   - plan-execute   — 先规划再执行（数学题 / 多步任务）
//   - reflection     — 答案产出后自我审查（判题 / 作文批改）
//   - tot            — Tree-of-Thought：列多条路径再择优（数学多解 / 开放题）
//   - self-reflect   — 强自查：每个关键步骤后立即审视（语文阅读 / 论证）
//   - mem-augmented  — 检索家庭/错题档案优先，再作答（个性化辅导）
//   - debate         — 双视角对辩后裁决（争议性判题 / 作文打分）
//   - auto           — 按消息启发式路由到上面具体模式
//
// 设计取舍：lightweight prompt-overlay 而非 hexagon agent factory。这样能在保留
// ReActEngine 全部能力（tool / streaming / budget / reasoning）的同时实现 K12
// 场景需要的 7 种策略。重型模式（多分支并行 / 多 agent 实例）需 v0.5.x 后扩展。
package engine

import (
	"regexp"
	"strings"

	"github.com/hexagon-codes/hexclaw/skill"
)

// AgentMode Agent 策略模式。
type AgentMode string

const (
	ModeReAct        AgentMode = "react"
	ModePlanExecute  AgentMode = "plan-execute"
	ModeReflection   AgentMode = "reflection"
	ModeToT          AgentMode = "tot"
	ModeSelfReflect  AgentMode = "self-reflect"
	ModeMemAugmented AgentMode = "mem-augmented"
	ModeDebate       AgentMode = "debate"
	ModeAuto         AgentMode = "auto"
)

// IsValid 检查是否合法枚举值。
func (m AgentMode) IsValid() bool {
	switch m {
	case ModeReAct, ModePlanExecute, ModeReflection,
		ModeToT, ModeSelfReflect, ModeMemAugmented, ModeDebate,
		ModeAuto:
		return true
	}
	return false
}

// DecodeMode 从 raw 字符串解码，未知或空返回 ModeAuto。
func DecodeMode(raw string) AgentMode {
	m := AgentMode(strings.TrimSpace(strings.ToLower(raw)))
	if m.IsValid() {
		return m
	}
	return ModeAuto
}

// modePromptPrefix 返回每种模式附加到 system prompt 的指令。
// 注意："react" 和 "auto" 在被路由解析后返回 "" —— 即不附加任何额外 prompt，保持默认行为。
func modePromptPrefix(m AgentMode) string {
	switch m {
	case ModePlanExecute:
		return `你在执行一个多步任务。请先分 3-5 步列出"计划"（用"计划："起头，每步一行），然后按计划依次执行，每一步用"第 N 步："起头。必要时调用工具。全部执行完后给出最终答案（用"答案："起头）。
`
	case ModeReflection:
		return `你在解答一道需要自查的题目。给出答案后，务必用"自查："起头回顾一遍：
- 关键条件是否都用到？
- 计算/推理是否有漏洞？
- 与常识是否冲突？
若发现问题，修正后以"最终答案："起头重新给出结论；若无问题，也要显式写出"自查通过"。
`
	case ModeToT:
		return `你在解一道可能多解的题目。请用 Tree-of-Thought 思路：
1. 用"思路 A："起头给出第一种解法（含关键步骤）。
2. 用"思路 B："起头给出第二种思路（与 A 不同的角度）。
3. 用"对比："起头评估两条思路的对错/优劣。
4. 用"最终答案："起头给出最稳妥的结论；如两条思路殊途同归，写出"两路殊途同归"。
`
	case ModeSelfReflect:
		return `你在解一道需要严格自我审视的题。请按以下结构作答：
1. 用"初步思考："给出原始想法（含假设）。
2. 用"反思："立即审视该想法可能的纰漏（前提是否成立？步骤是否跳跃？）。
3. 用"修正："给出改进后的推理。
4. 用"最终答案："给出确定结论。
若反思发现初步思考无问题，反思段也要明确写"反思通过"。
`
	case ModeMemAugmented:
		return `你在为一名熟悉的学生作答。请先做以下检索：
1. 用"档案回忆："起头，回顾该学生的薄弱点 / 历史错题 / 学习偏好（基于已注入的 memory 与 RAG 上下文；无信息则写"暂无档案"）。
2. 用"切入点："起头，说明本次作答如何结合档案（个性化讲解 / 避开混淆点）。
3. 用"答案："起头给出最终回答。
注意：档案信息只用于个性化讲解，不要把档案原文复述给学生。
`
	case ModeDebate:
		return `你需要用双视角辩论方式作答这道争议题：
1. 用"正方："起头，给出"答案是 X"的最强论据。
2. 用"反方："起头，给出"答案不是 X"的最强反驳。
3. 用"裁决："起头，权衡正反双方，指出哪方更有理由及关键依据。
4. 用"最终答案："起头给出结论；若双方势均力敌且无法判定，也要明确写"暂无定论 + 缺什么信息"。
`
	default:
		return ""
	}
}

// AutoRoute 对用户消息做启发式分类，返回推荐的 AgentMode。
//
// K12 场景分类规则（优先级从上到下）：
//  1. 双答案争议 / 打分类 → ModeDebate（双视角辩论）
//  2. 多解探索（"几种方法"/"还有别的解法"） → ModeToT
//  3. 个性化档案（"我之前"/"我孩子"/"以前错过") → ModeMemAugmented
//  4. 数学 / 多步计算 / 规划类 → ModePlanExecute（先列步骤再算）
//  5. 判题 / 对错争议 → ModeReflection（自我审查答案）
//  6. 兜底 → ModeReAct
//
// 返回值为具体模式（不返回 ModeAuto）。
func AutoRoute(userText string) AgentMode {
	text := strings.ToLower(userText)
	if containsDebateFeatures(text) {
		return ModeDebate
	}
	if containsToTFeatures(text) {
		return ModeToT
	}
	if containsMemoryFeatures(text) {
		return ModeMemAugmented
	}
	if containsMathFeatures(text) || containsPlanFeatures(text) {
		return ModePlanExecute
	}
	if containsJudgeFeatures(text) {
		return ModeReflection
	}
	return ModeReAct
}

var mathPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\d+\s*[\+\-\*\/×÷=]\s*\d+`),
	regexp.MustCompile(`\b(函数|方程|几何|求解|多少|等于|算式|计算)\b`),
	regexp.MustCompile(`\$[^$]+\$`), // LaTeX 公式
}

func containsMathFeatures(s string) bool {
	for _, p := range mathPatterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

func containsPlanFeatures(s string) bool {
	keywords := []string{"计划", "安排", "一周", "一个月", "复习", "备考", "时间表", "plan", "schedule"}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func containsJudgeFeatures(s string) bool {
	keywords := []string{"对不对", "对吗", "答案是", "正确吗", "哪个对", "判断下", "批改", "review", "correct"}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// containsDebateFeatures 双答案争议 / 双视角打分类
func containsDebateFeatures(s string) bool {
	keywords := []string{"两个答案", "都说", "争议", "有人说", "到底哪个", "评分", "打分", "评分理由", "辩一辩"}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// containsToTFeatures 多解探索类
func containsToTFeatures(s string) bool {
	keywords := []string{"几种方法", "几种解法", "多种思路", "另一种解法", "还有别的", "其他思路", "多个角度"}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// containsMemoryFeatures 个性化档案类
func containsMemoryFeatures(s string) bool {
	keywords := []string{"我之前", "我孩子", "以前错过", "上次", "我家", "之前那道", "错题本"}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// ResolveMode 将 meta 里的 agent_mode 解析为最终具体模式。
// mode="auto" 会基于 userText 启发式路由；显式指定则直接返回。
//
// 注意：这是无 Skill 上下文版本，不能利用 Skill.PreferredMode 覆盖路由。
// 调用方若有 Skill registry，请改用 ResolveModeWithSkillHint。
func ResolveMode(rawMode, userText string) AgentMode {
	m := DecodeMode(rawMode)
	if m == ModeAuto {
		return AutoRoute(userText)
	}
	return m
}

// ResolveModeWithSkillHint 在 ResolveMode 基础上让"Skill 声明的 PreferredMode"
// 优先级最高（B2 DoD 第二条）。
//
// 路由优先级（从高到低）：
//  1. 显式指定的非 auto rawMode（用户在 Settings 选了具体模式）
//  2. Top-1 召回 Skill 的 frontmatter `preferred_mode`（YAML 声明）
//  3. AutoRoute 启发式分类
//
// registry==nil 或 query=="" 时退化到 ResolveMode。
func ResolveModeWithSkillHint(rawMode, userText string, registry *skill.DefaultRegistry) AgentMode {
	m := DecodeMode(rawMode)
	if m != ModeAuto {
		return m
	}
	if registry != nil && strings.TrimSpace(userText) != "" {
		// 用 Top-1 召回；不附加 Activation（避免循环依赖：Activation.Mode 还没决定）
		if hits := registry.SelectByQuery(userText, 1); len(hits) > 0 {
			if pm := skill.PreferredMode(hits[0]); pm != "" {
				if mode := AgentMode(pm); mode.IsValid() && mode != ModeAuto {
					return mode
				}
			}
		}
	}
	return AutoRoute(userText)
}
