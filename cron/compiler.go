package cron

import (
	"context"
)

// JobSpecCompiler 把用户的自然语言任务需求编译成可执行 JobSpec（一次性 LLM 调用）。
//
// 实现要求：
//   - 创建期一次性消耗 LLM tokens；运行时不再调用 LLM
//   - 失败必须返回错误（拒绝入库），避免空 Spec 进入运行时
//   - 输出脚本须通过 validators.go 中的安全校验
type JobSpecCompiler interface {
	Compile(ctx context.Context, prompt string, hints CompileHints) (*JobSpec, error)
}

// ProgressStage 是 cron 编译过程中可观察的离散阶段。
// 前端 UI 用 stage 渲染进度条段落。
type ProgressStage string

const (
	StageAnalyzing  ProgressStage = "analyzing"   // 解析用户需求 / 选 provider
	StageCallingLLM ProgressStage = "calling_llm" // 调上游 LLM 生成 Python 脚本（最长一段）
	StageValidating ProgressStage = "validating"  // AST + 语法 + 输出契约校验
	StagePersisting ProgressStage = "persisting"  // 序列化 + INSERT cron_jobs
)

// CompileProgress 是一个进度事件。
type CompileProgress struct {
	Stage   ProgressStage `json:"stage"`
	Message string        `json:"message"`
}

// ProgressFunc 接收编译进度事件。回调里不要阻塞 — 后端 handler 会同步 flush 给客户端。
type ProgressFunc func(p CompileProgress)

// ProgressCompiler 是带进度反馈能力的 JobSpecCompiler 扩展接口。
//
// 后端 Scheduler 会做 interface 探测：实现了 ProgressCompiler 就走 stream 路径，
// 否则把整个编译当作单一阶段（仍能工作，只是用户看不到细节）。
type ProgressCompiler interface {
	JobSpecCompiler
	CompileWithProgress(ctx context.Context, prompt string, hints CompileHints, onProgress ProgressFunc) (*JobSpec, error)
}

// CompileHints 给 compiler 注入可选的上下文。
type CompileHints struct {
	// AvailableSkills 编译期 prompt 内告知 LLM 用户可用的 hexclaw skill 名（仅供 reference）。
	AvailableSkills []string
	// KBPath 用户知识库根路径，便于脚本写入。
	KBPath string
	// UserID 用户上下文，主要用于审计日志。
	UserID string
	// LocalAPIBase hexclaw 本机 HTTP API base，例如 "http://127.0.0.1:16060"。
	// 留空 → compiler 不在 system prompt 注入本地 API hint。
	LocalAPIBase string
}
