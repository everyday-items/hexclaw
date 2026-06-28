package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// multiAgentResultMaxChars 是 orchestrate/spawn 工具结果透传到前端时的展示上限。普通工具结果
// 截到 500（避免气泡臃肿），但多 Agent 结果尾部带 hexclaw-subagents 哨兵块（折叠面板数据源），
// 500 会切掉它，故放宽到此值；per-agent output 已被 subAgentOutputMaxRunes 约束、N≤并发上限，
// 整体可控（最坏 8 路 ≈ 18KB）。
const multiAgentResultMaxChars = 32000

// truncateToolResultForDisplay 给透传到前端的工具结果做展示截断：orchestrate/spawn 用更高上限
// 以保全尾部哨兵块，其余工具沿用 500。LLM 始终看到未截断的完整结果（截断只作用于发往前端的副本）。
func truncateToolResultForDisplay(name, content string) string {
	max := 500
	switch name {
	case "orchestrate", "spawn_agent":
		max = multiAgentResultMaxChars
	}
	return stringx.TruncateWithSuffix(content, max, "...")
}

// isDeadlineErr 判定一个错误是否因超时（ctx deadline / cancel）产生，用于把子 Agent 的
// status 从普通 error 细分为 timeout。
func isDeadlineErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	return strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "deadline")
}

// SubAgentReport 是一次子 Agent 执行的结构化回执，对齐 OpenClaw announce 的「结构化完成
// 通知」语义：每个子 Agent 的名字 / 状态 / 耗时 / 输出 / 错误。orchestrate / spawn_agent
// 把它编码进工具结果，供桌面端折叠协作面板渲染（完成后结构化展示，非逐字流式）。
type SubAgentReport struct {
	Agent    string `json:"agent"`
	Status   string `json:"status"` // ok | error | timeout
	Duration string `json:"duration"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

// 子 Agent 执行状态常量。
const (
	subAgentStatusRunning = "running"
	subAgentStatusOK      = "ok"
	subAgentStatusError   = "error"
	subAgentStatusTimeout = "timeout"
)

// subAgentSentinelLang 是嵌进工具结果的围栏代码块语言标签。前端检测到 orchestrate /
// spawn_agent 工具结果里的 ```hexclaw-subagents 块即解析渲染为协作面板，并隐藏原始 JSON。
// 用一个不会与真实代码块碰撞的私有标签，避免误伤用户内容里的普通 ```json。
const subAgentSentinelLang = "hexclaw-subagents"

// fmtSubAgentDuration 把执行耗时格式化为 "12.5s" / "850ms"，供面板直接展示。
func fmtSubAgentDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// subAgentOutputMaxRunes 限制单个子 Agent 回执里 output 的长度。回执会随工具结果透传到前端
// 折叠面板（受 multiAgentResultMaxChars 整体上限约束），逐个截断保证 N 个子 Agent 的哨兵块
// 不超整体上限、不被前端 500 截断切掉。1000 rune ≈ 一段完整结论，作为面板预览足够；面板已注明
// 「完成后结构化展示」，全文摘要由主 Agent 在正文给出。
const subAgentOutputMaxRunes = 1000

// capReportOutput 把回执 output 截到 subAgentOutputMaxRunes，超出加省略标记。
func capReportOutput(s string) string {
	r := []rune(s)
	if len(r) <= subAgentOutputMaxRunes {
		return s
	}
	return string(r[:subAgentOutputMaxRunes]) + "…(truncated)"
}

// newSubAgentReport 由一次执行结果构造回执，自动判定 status（区分超时 vs 普通错误）。
func newSubAgentReport(agent string, output string, err error, d time.Duration) SubAgentReport {
	r := SubAgentReport{Agent: agent, Status: subAgentStatusOK, Duration: fmtSubAgentDuration(d), Output: capReportOutput(output)}
	if err != nil {
		r.Output = ""
		r.Error = err.Error()
		r.Status = subAgentStatusError
		if isDeadlineErr(err) {
			r.Status = subAgentStatusTimeout
		}
	}
	return r
}

// encodeSubAgentReports 把回执序列化为追加在工具结果尾部的哨兵围栏块。前端解析它渲染面板；
// LLM 仍读工具结果正文（markdown）写最终汇总——正文与本块并存，互不替代。
func encodeSubAgentReports(reports []SubAgentReport) string {
	if len(reports) == 0 {
		return ""
	}
	b, err := json.Marshal(reports)
	if err != nil {
		return ""
	}
	return "\n\n```" + subAgentSentinelLang + "\n" + string(b) + "\n```"
}
