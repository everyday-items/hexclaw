package adapter

import "strings"

// IM 端没有桌面那样的结构化工具卡，本文件把工具调用渲染成**紧凑文本尾注**，
// 让 Telegram / 企业微信 / 飞书等 IM 用户也能看到「调用了什么工具、成没成、结果摘要」。
//
// 这是「工具调用展示」的 Go 侧共享实现（对应桌面前端 src/utils/tool-call.ts），
// 供所有 IM 适配器经统一出站点复用，避免各适配器各写一套或干脆没有。

// toolDisplayNames 工具名 → 中文显示名（镜像前端 i18n chat.toolName）。
// 未命中回退裸名。河蟹/小蟹为中文优先产品，IM 默认中文。
var toolDisplayNames = map[string]string{
	"code_exec":         "代码沙箱",
	"code":              "代码运行",
	"shell":             "终端",
	"file_ops":          "文件管理",
	"file_edit":         "文件编辑",
	"grep":              "内容搜索",
	"glob":              "文件查找",
	"search":            "网络搜索",
	"weather":           "天气查询",
	"translate":         "翻译",
	"summary":           "摘要提取",
	"browser":           "网页浏览",
	"create_skill":      "新建 Skill",
	"manage_skill":      "Skill 管理",
	"manage_mcp_server": "MCP 管理",
	"orchestrate":       "多 Agent 协作",
	"spawn_agent":       "子 Agent",
	"transfer_to_agent": "Agent 交接",
	"cron_task":         "定时任务",
}

// toolBaseName 剥离 MCP/命名空间前缀取末段（mcp__server__tool → tool）。
func toolBaseName(name string) string {
	if name == "" {
		return name
	}
	s := strings.TrimPrefix(name, "mcp__")
	s = strings.ReplaceAll(s, "__", "\x00")
	s = strings.ReplaceAll(s, ":", "\x00")
	s = strings.ReplaceAll(s, ".", "\x00")
	parts := strings.Split(s, "\x00")
	last := parts[len(parts)-1]
	if last == "" {
		return name
	}
	return last
}

// toolDisplayName 工具显示名 —— IM 永不泄露裸 id。
func toolDisplayName(name string) string {
	base := toolBaseName(name)
	if zh, ok := toolDisplayNames[base]; ok {
		return zh
	}
	return base
}

// toolResultSummary 工具结果一行摘要（rune 安全 + 去 markdown 强调标记 + 折叠空白）。
// 真实工具结果常是带 emoji 的 markdown 文本，这里压成单行短摘要。
func toolResultSummary(result string, maxRunes int) string {
	if strings.TrimSpace(result) == "" {
		return ""
	}
	// 去 markdown 强调标记
	cleaned := strings.NewReplacer("*", "", "`", "").Replace(result)
	// 折叠所有空白（含换行）为单空格
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	r := []rune(cleaned)
	if len(r) <= maxRunes {
		return cleaned
	}
	return string(r[:maxRunes-1]) + "…"
}

// toolStatusGlyph 状态字形：success→✓ / error→✗ / 其它（未知/运行）→·
func toolStatusGlyph(status string) string {
	switch status {
	case "error":
		return "✗"
	case "success":
		return "✓"
	default:
		return "·"
	}
}

// ToolCallDigest 把工具调用渲染成 IM 友好的紧凑尾注（带前导分隔）。
// 空调用返回空串（调用方据此决定是否追加）。
func ToolCallDigest(calls []ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n———")
	for _, c := range calls {
		b.WriteString("\n🔧 ")
		b.WriteString(toolDisplayName(c.Name))
		b.WriteString(" ")
		b.WriteString(toolStatusGlyph(c.Status))
		if sum := toolResultSummary(c.Result, 48); sum != "" {
			b.WriteString(" · ")
			b.WriteString(sum)
		}
	}
	return b.String()
}
