package engine

// GO-3（BUG-20260703）：系统派发身份/授权范围/工具继承全靠 msg.Metadata 里的
// source / cron_job_id / webhook_id / workflow_id / spawn_depth / subagent_run_id /
// tool_allow / tool_deny 推导，而 HTTP 聊天入口（api/server.go）把 req.Metadata
// 原样拷进 msg.Metadata。外部聊天调用方即可伪造 source=cron + cron_job_id=X，
// 骗过 isSystemDispatch → task_ref="cron:X" → 命中该 cron job 的任务级 grant，
// 一步盗用其授权（提权）。对比 solve 用不可伪造 typed-ctx grant。
//
// 契约：这些保留键只能由受信内部派发器（cron/webhook/workflow/spawn）在 Go 侧
// 构造消息时盖章，必须在信任边界从不可信客户端 metadata 中剥除。

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestBug20260703_StripReservedDispatchMetadata(t *testing.T) {
	m := map[string]string{
		// 保留键（应被剥除）：伪造派发身份 + 盗 grant + 越权
		"source":          "cron",
		"cron_job_id":     "victim-job",
		"webhook_id":      "victim-wh",
		"workflow_id":     "victim-wf",
		"spawn_depth":     "0",
		"subagent_run_id": "forged-run",
		"tool_allow":      "shell,code",
		"tool_deny":       "",
		"dispatch_role":   "x",
		// 合法客户端可控键（应保留）
		"provider":     "openrouter",
		"model":        "qwen3.6",
		"role":         "translator",
		"pinned_agent": "translator",
		"thinking":     "on",
		"skills":       "a,b",
	}
	StripReservedDispatchMetadata(m)

	for _, k := range []string{"source", "cron_job_id", "webhook_id", "workflow_id", "spawn_depth", "subagent_run_id", "tool_allow", "tool_deny", "dispatch_role"} {
		if _, ok := m[k]; ok {
			t.Errorf("[GO-3] 保留派发键 %q 未从不可信 metadata 中剥除", k)
		}
	}
	for _, k := range []string{"provider", "model", "role", "pinned_agent", "thinking", "skills"} {
		if _, ok := m[k]; !ok {
			t.Errorf("合法客户端键 %q 被误删", k)
		}
	}
}

// 伪造 source 的聊天消息经剥除后不得被判为系统派发（否则 task_ref 盗 grant）。
func TestBug20260703_ForgedChatMessageNotSystemDispatch(t *testing.T) {
	forged := map[string]string{"source": "cron", "cron_job_id": "victim-job"}
	StripReservedDispatchMetadata(forged)

	msg := &adapter.Message{Platform: adapter.PlatformAPI, Metadata: forged}
	if isSystemDispatch(msg) {
		t.Fatal("[GO-3] 伪造 source=cron 的聊天消息剥除后仍被判为系统派发")
	}
	if ref := systemDispatchTaskRefFromMessage(msg); ref != "" {
		t.Fatalf("[GO-3] 伪造消息仍能推出 task_ref=%q（可命中受害任务 grant）", ref)
	}
}

// 受信派发器（Go 侧构造）的保留键不受影响：剥除只作用于信任边界的客户端输入，
// 不作用于内部构造的消息（那条路径根本不调 Strip）。此测试锁定 cron 派发消息
// 仍带完整身份。
func TestBug20260703_TrustedCronDispatchKeepsIdentity(t *testing.T) {
	msg := NewCronDispatchMessage("u", "c", "job-9", "summarize")
	// 受信构造路径不经 Strip；直接断言身份完好。
	if !isSystemDispatch(msg) {
		t.Fatal("受信 cron 派发应被判为系统派发")
	}
	if ref := systemDispatchTaskRefFromMessage(msg); ref != "cron:job-9" {
		t.Fatalf("受信 cron 派发 task_ref 应为 cron:job-9，实际 %q", ref)
	}
}
