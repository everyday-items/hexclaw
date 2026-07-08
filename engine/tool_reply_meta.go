package engine

import (
	"context"
	"sync"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// BUG-1 plumb：把 skill 执行产出的结构化「reply-safe」元数据从工具边界带到最终回复。
//
// 为什么需要：ToolExecutor.Execute 的返回类型是 (string, error)，只回传 skill.Result.Content，
// skill.Result.Metadata 在工具边界被整个丢弃。K12 错题入库徽章（record chip）等需要把结构化
// 元数据带到 reply.Metadata → 前端消息 metadata.record。改返回签名会波及大量调用点，故用一个
// ctx 作用域的收集器：ToolExecutor 执行 skill 后把白名单键收进 sink，runner 跑完后 runner 路径
// 排空进 msg.Metadata，由 buildReplyMetadata 转发。
//
// AP-1：engine 不认识具体键的领域语义，只透传领域中立白名单（record）。领域字面量由场景包
// （scenarios/k12）产出。

type replyMetaSinkKeyType struct{}

var replyMetaSinkKey = replyMetaSinkKeyType{}

// replySafeToolMetaKeys 是允许从工具结果透传到回复元数据的键白名单。
// 只收结构化、面向前端渲染的领域中立键；杜绝内部 skill 元数据泄漏到回复。
var replySafeToolMetaKeys = map[string]bool{
	"record": true, // 记录本入库徽章（{collection,fields,status} JSON），前端 messageRecordChip 消费
}

type replyMetaSink struct {
	mu sync.Mutex
	m  map[string]string
}

// withToolReplyMetaSink 在 ctx 上安装「工具→回复」元数据收集器（幂等）。
// 在 runner 跑工具循环前调用，使工具执行期间 skill 产出的白名单元数据可被收集。
func withToolReplyMetaSink(ctx context.Context) context.Context {
	if ctx.Value(replyMetaSinkKey) != nil {
		return ctx // 已装：避免嵌套覆盖丢失已收集项
	}
	return context.WithValue(ctx, replyMetaSinkKey, &replyMetaSink{m: map[string]string{}})
}

func replyMetaSinkFrom(ctx context.Context) *replyMetaSink {
	s, _ := ctx.Value(replyMetaSinkKey).(*replyMetaSink)
	return s
}

// stampToolReplyMeta 由 ToolExecutor 在 skill 执行后调用，收集白名单内的 reply-safe 元数据。
// ctx 无 sink（旧路径/无工具循环）时安全 no-op。
func stampToolReplyMeta(ctx context.Context, meta map[string]string) {
	if len(meta) == 0 {
		return
	}
	s := replyMetaSinkFrom(ctx)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range meta {
		if v != "" && replySafeToolMetaKeys[k] {
			// 后写覆盖：同一轮工具循环内多次入库取最后一条（对齐「一次批改一道题」语义）。
			s.m[k] = v
		}
	}
}

// applyToolReplyMeta 把 sink 收集的 reply-safe 元数据落到 msg.Metadata，
// 供 buildReplyMetadata 转发进最终回复。在 runner 跑完后、组装回复前调用。
func applyToolReplyMeta(ctx context.Context, msg *adapter.Message) {
	s := replyMetaSinkFrom(ctx)
	if s == nil || msg == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) == 0 {
		return
	}
	ensureMessageMetadata(msg)
	for k, v := range s.m {
		msg.Metadata[k] = v
	}
}
