package autonomy

import (
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/engine"
)

// PreflightRequest 创建流静默预检的入参。
//
// Tools 是可静态确定的工具名（workflow 节点分析最准确）；prompt 驱动的
// cron/webhook 任务没有确定工具清单，只做启发式预估——预估口径由 UI 明示，
// 漏判由运行时审批兜底，多判不产生任何授权。
type PreflightRequest struct {
	Source  string   `json:"source"`
	TaskRef string   `json:"task_ref,omitempty"`
	Prompt  string   `json:"prompt,omitempty"`
	Deliver bool     `json:"deliver,omitempty"`
	Tools   []string `json:"tools,omitempty"`
}

// CapabilityVerdict 一个能力类别在当前策略下的判定。
type CapabilityVerdict struct {
	Category string   `json:"category"`
	Tools    []string `json:"tools"`
	State    string   `json:"state"` // auto | granted | approval
}

// ToolVerdict 不属于内置类别的具体工具（MCP/连接器）的判定。
type ToolVerdict struct {
	Tool  string `json:"tool"`
	State string `json:"state"` // auto | granted | approval
}

// PreflightResult 预检结果：确定性判定 + 预估使用面。
//
// AllClear 的语义是「预估范围内没有需要人工处理的项」，不是「运行时绝不会
// 触发审批」——basis=heuristic 时预估可能漏判（GO-10），运行时审批兜底。
type PreflightResult struct {
	Source        string              `json:"source"`
	Profile       string              `json:"profile"`
	Capabilities  []CapabilityVerdict `json:"capabilities"`
	ExtraTools    []ToolVerdict       `json:"extra_tools,omitempty"`
	Estimated     []string            `json:"estimated"`
	NeedsDecision []string            `json:"needs_decision"`
	AllClear      bool                `json:"all_clear"`
	// Basis 预估口径：static=显式工具清单静态分析（确定性）；heuristic=含
	// prompt 关键词预估成分（可能漏判）。消费方据此区分两种「全绿」。
	Basis string `json:"basis"`
}

// Preflight 对照当前 Profile 矩阵与任务级授权做确定性求值。
// grants 可为 nil（创建流首次预检还没有 task_ref）。
func Preflight(policy *engine.SystemDispatchPolicy, grants *GrantStore, req PreflightRequest) PreflightResult {
	if policy == nil {
		policy = engine.DefaultSystemDispatchPolicy()
	}
	source := strings.ToLower(strings.TrimSpace(req.Source))

	res := PreflightResult{
		Source: source,
		// FS-7：前端把这两个字段声明为非空数组，初始化空切片避免 JSON null。
		Estimated:     []string{},
		NeedsDecision: []string{},
		Profile:       policy.Profile(),
		Basis:         "heuristic",
	}
	if len(req.Tools) > 0 && strings.TrimSpace(req.Prompt) == "" {
		// 只有显式工具清单、无 prompt 预估成分才是确定性口径。
		res.Basis = "static"
	}

	categoryState := map[string]string{}
	for _, category := range engine.SystemDispatchCategories() {
		state := "approval"
		if policy.AllowsCategory(source, category) {
			state = "auto"
		} else if grants != nil && req.TaskRef != "" && grantCoversCategory(grants, source, req.TaskRef, category) {
			state = "granted"
		}
		categoryState[category] = state
		res.Capabilities = append(res.Capabilities, CapabilityVerdict{
			Category: category,
			Tools:    engine.SystemDispatchCategoryTools(category),
			State:    state,
		})
	}

	// 显式工具清单（workflow 静态分析）：能归类的并入类别预估，
	// 归不了类的（MCP/连接器工具）单独判定——矩阵没有它们的类别，
	// 只有显式条目/grant/全功能 "*" 才会自动放行。
	estimated := map[string]bool{}
	for _, tool := range req.Tools {
		tool = strings.TrimSpace(tool)
		if tool == "" {
			continue
		}
		if category := engine.SystemDispatchToolCategory(tool); category != "" {
			estimated[category] = true
			continue
		}
		state := "approval"
		if policy.Allows(source, tool) {
			state = "auto"
		} else if grants != nil && req.TaskRef != "" && grants.GrantAllows(source, req.TaskRef, tool) {
			state = "granted"
		}
		res.ExtraTools = append(res.ExtraTools, ToolVerdict{Tool: tool, State: state})
	}

	for _, category := range estimateCategories(req) {
		estimated[category] = true
	}

	res.Estimated = sortedCategories(estimated)
	for _, category := range res.Estimated {
		if categoryState[category] == "approval" {
			res.NeedsDecision = append(res.NeedsDecision, category)
		}
	}
	for _, tv := range res.ExtraTools {
		if tv.State == "approval" {
			res.NeedsDecision = append(res.NeedsDecision, tv.Tool)
		}
	}
	res.AllClear = len(res.NeedsDecision) == 0
	return res
}

// grantCoversCategory 判断任务级授权是否覆盖整个类别（类别内每个工具模式都命中）。
func grantCoversCategory(grants *GrantStore, source, taskRef, category string) bool {
	tools := engine.SystemDispatchCategoryTools(category)
	if len(tools) == 0 {
		return false
	}
	for _, tool := range tools {
		if !grants.GrantAllows(source, taskRef, tool) {
			return false
		}
	}
	return true
}

// estimateCategories 从任务描述启发式预估会用到的能力类别。
// 这是预估不是承诺：漏判由运行时审批兜底，多判不产生授权。
func estimateCategories(req PreflightRequest) []string {
	out := []string{"read"}
	prompt := strings.ToLower(req.Prompt)

	// prompt 驱动的任务（无显式工具清单）按 agent 模式预估沙箱计算。
	if len(req.Tools) == 0 && strings.TrimSpace(req.Prompt) != "" {
		out = append(out, "exec_sandboxed")
	}
	if req.Deliver || containsAny(prompt, "发送", "发到", "推送", "通知", "send", "notify") {
		out = append(out, "delivery")
	}
	if containsAny(prompt, "发布", "公众号", "publish") {
		out = append(out, "publish")
	}
	// GO-10：漏判 = AllClear 虚假承诺，多判不产生授权——高信号同义词要覆盖。
	if containsAny(prompt, "网页", "浏览器", "browser", "浏览", "搜索", "抓取", "爬取", "search", "crawl") {
		out = append(out, "browser")
	}
	if containsAny(prompt, "文件", "file", "下载", "保存", "存档", "归档", "导出", "写入", "download", "save", "export") {
		out = append(out, "files")
	}
	if containsAny(prompt, "图片", "图像", "画一", "生成图", "配图", "视频", "语音", "image", "video", "draw", "tts") {
		out = append(out, "media")
	}
	return out
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func sortedCategories(set map[string]bool) []string {
	// 按矩阵稳定顺序输出，未知类别（理论不会有）排尾部字典序。
	order := engine.SystemDispatchCategories()
	pos := make(map[string]int, len(order))
	for i, c := range order {
		pos[c] = i + 1
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := pos[out[i]], pos[out[j]]
		if pi == 0 && pj == 0 {
			return out[i] < out[j]
		}
		if pi == 0 || pj == 0 {
			return pj == 0
		}
		return pi < pj
	})
	return out
}

// MatrixCell 生效策略矩阵中一个（来源 × 类别）单元。
type MatrixCell struct {
	Category string `json:"category"`
	State    string `json:"state"` // auto | approval
}

// MatrixRow 一个来源的整行判定。
type MatrixRow struct {
	Source string       `json:"source"`
	Cells  []MatrixCell `json:"cells"`
}

// MatrixView 生效策略矩阵快照（设置页只读诊断视图）。
type MatrixView struct {
	Profile    string      `json:"profile"`
	Categories []string    `json:"categories"`
	Rows       []MatrixRow `json:"rows"`
}

// MatrixSnapshot 从当前策略生成矩阵快照。
func MatrixSnapshot(policy *engine.SystemDispatchPolicy) MatrixView {
	if policy == nil {
		policy = engine.DefaultSystemDispatchPolicy()
	}
	view := MatrixView{
		Profile:    policy.Profile(),
		Categories: engine.SystemDispatchCategories(),
	}
	for _, source := range engine.SystemDispatchSources() {
		row := MatrixRow{Source: source}
		for _, category := range view.Categories {
			state := "approval"
			if policy.AllowsCategory(source, category) {
				state = "auto"
			}
			row.Cells = append(row.Cells, MatrixCell{Category: category, State: state})
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}
