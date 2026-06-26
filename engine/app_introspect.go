package engine

import "context"

// AppInventory 是注入"系统名片"的基数 + 版本——便宜地让模型知道"有什么、能查什么"，
// 而不是把实际条目灌进每轮 prompt（基数 + 指针，非 payload）。
type AppInventory struct {
	Version string
	// Counts: agents / cron / connections / webhooks / workflows / mcp 各自的数量。
	Counts map[string]int
}

// AppIntrospector 让 Agent 感知并按需读取本应用已配置的资源
// （连接/通道、定时任务、webhook、工作流、MCP/数据连接器、配置）。
//
// 实现方在 main.go 注入，闭包持有各 Manager。**安全不变量（硬约束）**：
// Query 的任何返回**绝不**包含 secret / token / api_key / 密码 / 凭据 / PII——
// 与 connectionCapabilities 脱敏列表同一红线（泄露给模型 = 模型可能复述 = P0 事故）。
type AppIntrospector interface {
	// Inventory 返回基数 + 版本，用于静态系统名片（每轮便宜注入）。userID 用于按用户隔离。
	Inventory(ctx context.Context, userID string) AppInventory
	// Query 按 domain（connections/mcp/webhooks/cron/agents/config）返回**脱敏**的人读清单，
	// 供 app_query 工具按需调用。action 目前支持 "list"/"get"；id 用于 get 单条。
	Query(ctx context.Context, domain, action, id, userID string) (string, error)
}

// SetAppIntrospector 注入自感知能力（可选；nil 时系统名片不含基数/版本，app_query 工具也不应注册）。
func (e *ReActEngine) SetAppIntrospector(introspector AppIntrospector) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.appIntrospector = introspector
}
