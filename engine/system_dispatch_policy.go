package engine

import (
	"path"
	"strings"

	"github.com/hexagon-codes/hexclaw/config"
)

const (
	SystemDispatchProfileFunctionFirst = "function_first"
	SystemDispatchProfileBalanced      = "balanced"
	SystemDispatchProfileStrict        = "strict"
	SystemDispatchProfileFullAccess    = "full_access"
)

var systemDispatchToolCategories = map[string][]string{
	"read": {
		"search", "web_search", "knowledge_ingest", "app_introspect", "list_agents",
	},
	"browser": {
		"browser",
	},
	// exec 拆两级信任：exec_sandboxed 是进沙箱执行（code_exec，走执行边界+资源限额+
	// 危险拦截），exec_host 是宿主机直执行（shell/code，不进沙箱、不过 broker）。
	// 二者绝不再塞进同一个开关——外部不可信触发源只应自动放行沙箱执行，宿主直执行
	// 必须留给内部编排（workflow/spawn）或显式审批。
	"exec_sandboxed": {
		"code_exec",
	},
	"exec_host": {
		"shell", "code",
	},
	"files": {
		"file_edit", "file_ops",
	},
	"automation": {
		"cron_task",
	},
	"delivery": {
		"send_message",
	},
	"media": {
		"media_generate",
	},
	"heal": {
		"app_heal",
	},
	"capability": {
		"create_skill", "manage_skill", "patch_skill", "manage_skill_pending", "manage_mcp_server",
	},
	"publish": {
		"publish_*",
	},
}

// systemDispatchCategoryOrder is the stable presentation order of tool
// categories. UI/preflight consumers iterate this order so the matrix renders
// identically across runs (map iteration order would shuffle it).
var systemDispatchCategoryOrder = []string{
	"read", "browser", "exec_sandboxed", "exec_host", "files",
	"automation", "delivery", "media", "heal", "capability", "publish",
}

// systemDispatchSourceOrder is the stable presentation order of dispatch
// sources for matrix/preflight consumers.
var systemDispatchSourceOrder = []string{
	cronDispatchSource, webhookDispatchSource, heartbeatDispatchSource,
	workflowDispatchSource, spawnDispatchSource, solveDispatchSource,
}

// SystemDispatchCategories returns the tool category names in stable order.
func SystemDispatchCategories() []string {
	return append([]string(nil), systemDispatchCategoryOrder...)
}

// SystemDispatchSources returns the dispatch source names in stable order.
func SystemDispatchSources() []string {
	return append([]string(nil), systemDispatchSourceOrder...)
}

// SystemDispatchCategoryTools returns the tool patterns of a category
// (exact names or globs like "publish_*"), or nil for an unknown category.
func SystemDispatchCategoryTools(category string) []string {
	return append([]string(nil), systemDispatchToolCategories[strings.ToLower(strings.TrimSpace(category))]...)
}

// SystemDispatchToolCategory reverse-maps a tool name to its category, or ""
// when the tool belongs to no built-in category (e.g. MCP connector tools).
func SystemDispatchToolCategory(toolName string) string {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return ""
	}
	for _, category := range systemDispatchCategoryOrder {
		for _, pattern := range systemDispatchToolCategories[category] {
			if systemDispatchPatternMatches(pattern, toolName) {
				return category
			}
		}
	}
	return ""
}

// SystemDispatchEntryAllowsTool reports whether a matrix/grant entry (category
// name, exact tool name, or glob) authorizes the given tool. Exported so the
// task-grant store can share the exact matching semantics of the matrix.
func SystemDispatchEntryAllowsTool(entry, toolName string) bool {
	return systemDispatchEntryAllows(entry, toolName)
}

// SystemDispatchPolicy is the unattended automation auto-approval matrix.
//
// It answers only one question: when a non-interactive source hits a tool that
// would otherwise require approval, may this source auto-approve it? Explicit
// PermissionPolicy ActionDeny still wins before this policy is consulted.
type SystemDispatchPolicy struct {
	profile string
	matrix  map[string][]string
}

func DefaultSystemDispatchPolicy() *SystemDispatchPolicy {
	return NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: SystemDispatchProfileFunctionFirst})
}

func FullAccessSystemDispatchPolicy() *SystemDispatchPolicy {
	return NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: SystemDispatchProfileFullAccess})
}

func NewSystemDispatchPolicyFromConfig(cfg config.AutonomyConfig) *SystemDispatchPolicy {
	profile := normalizeAutonomyProfile(cfg.Profile)
	matrix := profileSystemDispatchMatrix(profile)
	applySystemDispatchOverride(matrix, cronDispatchSource, cfg.SystemDispatch.Cron)
	applySystemDispatchOverride(matrix, webhookDispatchSource, cfg.SystemDispatch.Webhook)
	applySystemDispatchOverride(matrix, heartbeatDispatchSource, cfg.SystemDispatch.Heartbeat)
	applySystemDispatchOverride(matrix, workflowDispatchSource, cfg.SystemDispatch.Workflow)
	applySystemDispatchOverride(matrix, spawnDispatchSource, cfg.SystemDispatch.Spawn)
	applySystemDispatchOverride(matrix, solveDispatchSource, cfg.SystemDispatch.Solve)
	return &SystemDispatchPolicy{profile: profile, matrix: matrix}
}

func (p *SystemDispatchPolicy) Profile() string {
	if p == nil || p.profile == "" {
		return SystemDispatchProfileFunctionFirst
	}
	return p.profile
}

func (p *SystemDispatchPolicy) Entries(source string) []string {
	if p == nil {
		p = DefaultSystemDispatchPolicy()
	}
	entries := p.matrix[normalizeSystemDispatchSource(source)]
	return append([]string(nil), entries...)
}

// AllowsCategory reports whether a whole category is auto-approved for a
// source: the entry list carries "*", the category name itself, or every tool
// pattern of the category is individually authorized.
func (p *SystemDispatchPolicy) AllowsCategory(source, category string) bool {
	if p == nil {
		p = DefaultSystemDispatchPolicy()
	}
	source = normalizeSystemDispatchSource(source)
	category = strings.ToLower(strings.TrimSpace(category))
	tools := systemDispatchToolCategories[category]
	if source == "" || category == "" || len(tools) == 0 {
		return false
	}
	entries := p.matrix[source]
	for _, entry := range entries {
		if entry == "*" || entry == category {
			return true
		}
	}
	for _, tool := range tools {
		allowed := false
		for _, entry := range entries {
			if systemDispatchEntryAllows(entry, tool) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

func (p *SystemDispatchPolicy) Allows(source, toolName string) bool {
	if p == nil {
		p = DefaultSystemDispatchPolicy()
	}
	source = normalizeSystemDispatchSource(source)
	toolName = strings.TrimSpace(toolName)
	if source == "" || toolName == "" {
		return false
	}
	for _, entry := range p.matrix[source] {
		if systemDispatchEntryAllows(entry, toolName) {
			return true
		}
	}
	return false
}

func normalizeAutonomyProfile(profile string) string {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "", "function-first", "functional_first", "functional-first", "function_first":
		return SystemDispatchProfileFunctionFirst
	case "balanced", "balance":
		return SystemDispatchProfileBalanced
	case "strict":
		return SystemDispatchProfileStrict
	case "full", "full-access", "full_access", "all":
		return SystemDispatchProfileFullAccess
	default:
		return SystemDispatchProfileFunctionFirst
	}
}

func normalizeSystemDispatchSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case cronDispatchSource:
		return cronDispatchSource
	case webhookDispatchSource:
		return webhookDispatchSource
	case heartbeatDispatchSource:
		return heartbeatDispatchSource
	case workflowDispatchSource:
		return workflowDispatchSource
	case spawnDispatchSource:
		return spawnDispatchSource
	case solveDispatchSource:
		return solveDispatchSource
	default:
		return strings.ToLower(strings.TrimSpace(source))
	}
}

func profileSystemDispatchMatrix(profile string) map[string][]string {
	switch normalizeAutonomyProfile(profile) {
	case SystemDispatchProfileFullAccess:
		return copySystemDispatchMatrix(map[string][]string{
			cronDispatchSource:      {"*"},
			webhookDispatchSource:   {"*"},
			heartbeatDispatchSource: {"*"},
			workflowDispatchSource:  {"*"},
			spawnDispatchSource:     {"*"},
			solveDispatchSource:     {},
		})
	case SystemDispatchProfileBalanced:
		// cron 源不含 automation（GO-9）：cron job 里的 agent 再建 cron job 是
		// 自我复制回路（job 造 job，AddJob 无总量兜底），一律转审批；操作者可经
		// system_dispatch.cron override 显式放行。workflow 是用户显式编排，保留。
		return copySystemDispatchMatrix(map[string][]string{
			cronDispatchSource:      {"read", "browser", "files", "delivery"},
			webhookDispatchSource:   {"read", "browser", "files", "delivery"},
			heartbeatDispatchSource: {"read", "browser", "delivery"},
			workflowDispatchSource:  {"read", "browser", "files", "automation", "delivery"},
			spawnDispatchSource:     {"read", "files"},
			solveDispatchSource:     {},
		})
	case SystemDispatchProfileStrict:
		return copySystemDispatchMatrix(map[string][]string{
			cronDispatchSource:      {},
			webhookDispatchSource:   {},
			heartbeatDispatchSource: {},
			workflowDispatchSource:  {},
			spawnDispatchSource:     {},
			solveDispatchSource:     {},
		})
	default:
		// function_first：外部/无人值守触发源（cron/webhook/heartbeat）只自动放行
		// 沙箱执行（exec_sandboxed=code_exec），宿主直执行（exec_host=shell/code）对
		// 这些源转为需审批——避免外部不可信输入把宿主 shell 一并自动放行。内部编排源
		// （workflow/spawn）是已授权任务内的编排，保留宿主直执行（exec_host）。
		// cron 源不含 automation（GO-9）：cron 内自建 cron 是自我复制回路，转审批。
		return copySystemDispatchMatrix(map[string][]string{
			cronDispatchSource:      {"read", "browser", "exec_sandboxed", "files", "delivery", "media"},
			webhookDispatchSource:   {"read", "browser", "exec_sandboxed", "files", "delivery", "media"},
			heartbeatDispatchSource: {"read", "browser", "exec_sandboxed", "files", "delivery"},
			workflowDispatchSource:  {"read", "browser", "exec_sandboxed", "exec_host", "files", "automation", "delivery", "media", "heal"},
			spawnDispatchSource:     {"read", "exec_sandboxed", "exec_host", "files"},
			solveDispatchSource:     {},
		})
	}
}

func copySystemDispatchMatrix(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for source, entries := range in {
		out[source] = append([]string(nil), entries...)
	}
	return out
}

func applySystemDispatchOverride(matrix map[string][]string, source string, entries *[]string) {
	if entries == nil {
		return
	}
	matrix[source] = normalizeSystemDispatchEntries(*entries)
}

func normalizeSystemDispatchEntries(entries []string) []string {
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	return out
}

func systemDispatchEntryAllows(entry, toolName string) bool {
	// 大小写语义分层：类别名是固定小写枚举（read/publish…），大小写不敏感；
	// 精确工具名/glob 是用户/MCP 提供的连接器工具名，大小写敏感（如 createIssue）。
	// 此前把 entry 与 toolName 都小写比较 → 含大写字母的连接器工具授权后永远匹配不上（FS-2）。
	entry = strings.TrimSpace(entry)
	toolName = strings.TrimSpace(toolName)
	if entry == "" || toolName == "" {
		return false
	}
	if entry == "*" || entry == toolName {
		return true
	}
	// 类别名走小写查表；命中即在该类别内判定，不再 fall through 当 glob。
	if patterns, ok := systemDispatchToolCategories[strings.ToLower(entry)]; ok {
		for _, pattern := range patterns {
			if systemDispatchPatternMatches(pattern, toolName) {
				return true
			}
		}
		return false
	}
	// 非类别名：作精确/glob 工具名匹配（保留原始大小写）。
	return systemDispatchPatternMatches(entry, toolName)
}

func systemDispatchPatternMatches(pattern, toolName string) bool {
	if pattern == toolName {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	ok, err := path.Match(pattern, toolName)
	return err == nil && ok
}
