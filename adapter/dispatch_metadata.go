package adapter

// ReservedDispatchMetadataKeys 是只能由受信内部派发器（cron/webhook/workflow/
// spawn）在 Go 侧构造消息时盖章的保留 metadata 键：它们决定系统派发身份
// （source）、任务级授权范围（*_job_id/*_id → task_ref → grant）、递归深度、
// 运行树与工具继承。若允许外部聊天/工作流调用方经请求 metadata 传入，即可伪造
// source=cron + cron_job_id 盗用他人任务的授权（GO-3/BUG-20260703）。
//
// 定义在最底层的 adapter 包，供 engine 与各入站适配器（web WS 等）共用，避免
// adapter → engine 反向依赖成环。
var ReservedDispatchMetadataKeys = []string{
	"source",
	"cron_job_id",
	"webhook_id",
	"workflow_id",
	"workflow_node_id",
	"spawn_depth",
	"subagent_run_id",
	"tool_allow",
	"tool_deny",
	"dispatch_role",
	"cron_claim_unverified",
	"cron_context",
	// 路由身份：只应由引擎内部路由（react.go 路由块 / applyPinnedAgent）在 Strip 之后
	// 写入。客户端提供即伪造——带 provider hint 令路由块跳过时，伪造的 routed_agent 会
	// 被 skill.WithRoutedAgent 采纳，绕过 agent 存在性校验跨孩子写记录（R1 High）。
	"routed_agent",
	"route_source",
}

// StripReservedDispatchMetadata 从不可信客户端 metadata 中剥除保留派发键。
// 每个 API/适配器信任边界在把客户端 metadata 交给引擎前必须调用它；受信内部
// 派发器在 Go 侧直接构造 msg.Metadata，不经此函数、身份不受影响。
func StripReservedDispatchMetadata(m map[string]string) {
	if m == nil {
		return
	}
	for _, k := range ReservedDispatchMetadataKeys {
		delete(m, k)
	}
}
