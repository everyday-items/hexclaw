package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// SessionSearcher 是 session_search 依赖的窄接口（*storage.Store 结构上满足）。
type SessionSearcher interface {
	SearchMessages(ctx context.Context, userID, query string, limit, offset int) ([]*storage.SearchResult, int, error)
}

// SessionSearchSkill 是记忆「检索双通道」的**会话深召回**通道（方案 §6bis.B / §7.2）：
// 当策展事实（常驻 + 三维打分注入）不足以回答时，Agent 主动用本工具按关键词翻**原始历史会话**，
// 取回当时的对话原文。与策展通道互补：策展=蒸馏后的稳定事实，深召回=逐字原始上下文（FTS 兜底）。
type SessionSearchSkill struct {
	store SessionSearcher
}

func NewSessionSearchSkill(store SessionSearcher) *SessionSearchSkill {
	return &SessionSearchSkill{store: store}
}

func (s *SessionSearchSkill) Name() string { return "session_search" }

func (s *SessionSearchSkill) Description() string {
	return "Search the user's past conversations by keyword (deep recall over raw history)."
}

// Match 始终走 LLM tool-calling，无关键字快路径。
func (s *SessionSearchSkill) Match(string) bool { return false }

func (s *SessionSearchSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("session_search",
		"Search the user's PAST conversations by keyword when the curated long-term memory in "+
			"context is not enough to answer (e.g. \"上次我们聊的那个部署方案\", \"之前那张架构图\"). "+
			"Returns matching message snippets with their session and time. Use sparingly — prefer the "+
			"memory already in context; reach for this only for older details not surfaced there.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"query": {
					Type:        "string",
					Description: "Keywords to search past messages for",
				},
				"limit": {
					Type:        "integer",
					Description: "Max results (default 5, max 20)",
				},
			},
			Required: []string{"query"},
		})
}

func (s *SessionSearchSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	if s.store == nil {
		return nil, fmt.Errorf("session storage is not enabled")
	}
	query := strings.TrimSpace(memArgString(args, "query"))
	if query == "" {
		return nil, fmt.Errorf("session_search requires query")
	}
	limit := memArgInt(args, "limit", 5)
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	// 鉴权用户由引擎在入口戳在 ctx（BUG-20260611 M7）；LLM 不得越权搜他人会话。
	userID := skill.AuthenticatedUserID(ctx)

	results, total, err := s.store.SearchMessages(ctx, userID, query, limit, 0)
	if err != nil {
		return nil, fmt.Errorf("搜索历史会话失败: %w", err)
	}
	if len(results) == 0 {
		return &skill.Result{Content: fmt.Sprintf("没有找到与「%s」相关的历史会话。", query)}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "找到 %d 条与「%s」相关的历史消息（显示前 %d 条）：\n", total, query, len(results))
	for i, r := range results {
		if r == nil || r.Message == nil {
			continue
		}
		title := strings.TrimSpace(r.SessionTitle)
		if title == "" {
			title = "(未命名会话)"
		}
		snippet := stringx.TruncateWithSuffix(strings.ReplaceAll(r.Message.Content, "\n", " "), 160, "…")
		fmt.Fprintf(&b, "%d. [%s · %s · %s] %s\n",
			i+1, title, roleLabel(r.Message.Role), r.Message.CreatedAt.Format("2006-01-02"), snippet)
	}
	return &skill.Result{
		Content:  strings.TrimRight(b.String(), "\n"),
		Metadata: map[string]string{"total": fmt.Sprintf("%d", total)},
	}, nil
}

// roleLabel 把存储 role 映射为可读中文（用户/助手/工具）。
func roleLabel(role string) string {
	switch role {
	case "user":
		return "用户"
	case "assistant":
		return "助手"
	case "tool":
		return "工具"
	default:
		return role
	}
}

// memArgInt 从 args 取整数参数（容错 float64 / 字符串）。
func memArgInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return def
	}
}
