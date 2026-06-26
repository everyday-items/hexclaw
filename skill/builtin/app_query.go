package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// AppQueryDomains 是 app_query 支持的 domain 全集 —— 工具 schema Enum、入参校验、各处错误文案的
// **单一事实源**（F2：此前 schema Enum / "domain is required" / "unknown domain" 三处各写一份 → 漂移，
// 错误串漏列 logs/workflows）。新增 domain 只改这里 + queryDomain 的 switch case。
// 顺序即给模型看的呈现顺序：能直接作答/操作的资源在前，诊断面（config/logs）在后。
var AppQueryDomains = []string{"connections", "mcp", "cron", "webhooks", "workflows", "agents", "config", "logs"}

// domainEnumAny 把 AppQueryDomains 适配成 schema.Enum 需要的 []any。
func domainEnumAny() []any {
	out := make([]any, len(AppQueryDomains))
	for i, d := range AppQueryDomains {
		out[i] = d
	}
	return out
}

// AppQuerier 是 app_query 工具依赖的窄能力：按 domain 返回**脱敏**的人读清单。
// 由 main.go 的自感知实现满足（与 engine.AppIntrospector 结构化兼容，同一对象）。
// 安全不变量（硬约束）：返回**绝不**含 secret / token / 密码 / 凭据 / PII。
type AppQuerier interface {
	Query(ctx context.Context, domain, action, id, userID string) (string, error)
}

// AppQuerySkill 让 Agent 按需读取本应用已配置资源
// （连接/通道、MCP 数据连接器、cron、webhook、工作流、config、agents）。
//
// 这是"自省读"的**收口**工具：一个工具 + domain 枚举，避免 N 个 list_* 工具撑爆工具列表、
// 也让模型从枚举里发现所有可读域。详情按需拉，不灌进每轮静态 prompt。
type AppQuerySkill struct {
	querier AppQuerier
}

func NewAppQuerySkill(querier AppQuerier) *AppQuerySkill {
	return &AppQuerySkill{querier: querier}
}

func (s *AppQuerySkill) Name() string { return "app_query" }

func (s *AppQuerySkill) Description() string {
	return "Read the user's configured HexClaw resources (connections/MCP data-connectors/cron/webhooks/workflows/config/agents)"
}

// Match 一律走 LLM tool-calling，无关键词快路径。
func (s *AppQuerySkill) Match(string) bool { return false }

func (s *AppQuerySkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("app_query",
		"Read this app's already-configured resources so you can answer the user and ground your actions on real "+
			"state instead of guessing. Read-only and redacted — never returns secrets/passwords/tokens. Domains: "+
			"connections (configured IM/email channels you can send to), mcp (MCP servers including data connectors "+
			"like MySQL/Postgres and the tools they expose), cron (scheduled tasks), webhooks, workflows, agents, "+
			"config (app settings, redacted), logs (recent app logs for diagnosis). For collection domains "+
			"(connections/mcp/cron/webhooks/workflows/agents) use action=get with id (resource id or name) to fetch "+
			"a single item; config/logs are singletons (action=list).",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"domain": {
					Type:        "string",
					Description: "Which resource to read",
					Enum:        domainEnumAny(),
				},
				"action": {
					Type:        "string",
					Description: "list (all, default) or get (single by id, for collection domains)",
					Enum:        []any{"list", "get"},
				},
				"id": {
					Type:        "string",
					Description: "Resource id/name (required for action=get)",
				},
			},
			Required: []string{"domain"},
		})
}

func (s *AppQuerySkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	if s.querier == nil {
		return nil, fmt.Errorf("app introspection is not enabled")
	}
	domain := cronArgString(args, "domain")
	if domain == "" {
		return nil, fmt.Errorf("domain is required (%s)", strings.Join(AppQueryDomains, "/"))
	}
	action := cronArgString(args, "action")
	if action == "" {
		action = "list"
	}
	id := cronArgString(args, "id")
	// 兑现 schema 契约（AP-058）：action=get 必须带 id —— 否则 get 退化成 list 是 over-promise。
	if action == "get" && id == "" {
		return nil, fmt.Errorf("action=get requires id (the resource id or name); use action=list to list all")
	}
	// 认证用户由引擎在入口戳到 ctx（model-supplied user_id 不可信）。缺省兜底归一由
	// querier 实现统一处理（resolveUserID），此处不再各自兜底，避免与基数侧口径漂移（P10 #6）。
	userID := skill.AuthenticatedUserID(ctx)
	slog.Info("[app_query] execute", "domain", domain, "action", action, "user", userID)

	out, err := s.querier.Query(ctx, domain, action, id, userID)
	if err != nil {
		return nil, err
	}
	// Content 是给模型读的脱敏 prose；Data 给程序化消费者一个免解析的轻量信封（P10 nit）。
	return &skill.Result{
		Content: out,
		Data:    map[string]any{"domain": domain, "action": action, "id": id},
	}, nil
}
