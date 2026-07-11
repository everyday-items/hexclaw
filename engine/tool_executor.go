package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexagon/observe/events"
	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/featureflag"
	"github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/skill"
)

// ToolExecutor executes tool calls with hook chain support.
//
// Resolution order: Skill registry → MCP Manager.
// Hook chain: beforeHooks → execute → afterHooks.
type ToolExecutor struct {
	skills      *skill.DefaultRegistry
	mcpMgr      *mcp.Manager
	beforeHooks []BeforeToolHook
	afterHooks  []AfterToolHook
}

// NewToolExecutor creates a new executor.
func NewToolExecutor(skills *skill.DefaultRegistry, mcpMgr *mcp.Manager) *ToolExecutor {
	return &ToolExecutor{skills: skills, mcpMgr: mcpMgr}
}

// AddHook adds a hook that implements BeforeToolHook and/or AfterToolHook.
func (e *ToolExecutor) AddHook(h any) {
	if bh, ok := h.(BeforeToolHook); ok {
		e.beforeHooks = append(e.beforeHooks, bh)
	}
	if ah, ok := h.(AfterToolHook); ok {
		e.afterHooks = append(e.afterHooks, ah)
	}
}

// Execute runs a tool call through the hook chain.
//
//  1. Find the tool (Skill first, then MCP)
//  2. Run beforeHooks (any can abort by returning error)
//  3. Execute the tool
//  4. Run afterHooks (can modify result, e.g. truncation)
func (e *ToolExecutor) Execute(ctx context.Context, toolName string, args map[string]any) (string, error) {
	call := &ToolCallInfo{
		Name:      toolName,
		Arguments: args,
	}

	// 1. Try Skill registry — direct name first, then derived slug for skills
	//    whose original name isn't a valid LLM identifier (e.g. Chinese names).
	if e.skills != nil {
		s, ok := e.skills.Get(toolName)
		if !ok {
			s, ok = e.resolveSkillBySlug(toolName)
		}
		if ok {
			call.Source = "skill"
			skillName := s.Name()
			// v0.4.0 G2：flag skill.pipeline.v1 ON 时 Skill 走 7 阶段 pipeline，
			// 触发 Loading / Verification / Persistence / Improvement 钩子；
			// flag OFF 退化到直接 Execute（与 v0.3 行为一致）。
			if featureflag.Enabled(ctx, skill.FlagSkillPipelineV1) {
				return e.executeWithHooks(ctx, call, func(ctx context.Context) (string, error) {
					return e.runSkillViaPipeline(ctx, skillName, args)
				})
			}
			return e.executeWithHooks(ctx, call, func(ctx context.Context) (string, error) {
				result, err := s.Execute(ctx, args)
				if err != nil {
					return "", err
				}
				// BUG-1：skill 结构化 reply-safe 元数据（如 record chip）经 ctx sink 带到 reply。
				stampToolReplyMeta(ctx, result.Metadata)
				return result.Content, nil
			})
		}
	}

	// 2. Prevent MCP tools from shadowing builtin skill names
	if e.skills != nil && e.isBuiltinSkillName(toolName) {
		return "", fmt.Errorf("tool %q is a reserved builtin skill name", toolName)
	}

	// 3. Try MCP Manager
	if e.mcpMgr != nil {
		call.Source = "mcp"
		return e.executeWithHooks(ctx, call, func(ctx context.Context) (string, error) {
			result, owner, err := e.mcpMgr.CallToolWithOwner(ctx, toolName, args)
			call.ServerName = owner
			return result, err
		})
	}

	return "", fmt.Errorf("tool '%s' not found in skills or MCP servers", toolName)
}

// resolveSkillBySlug finds the skill whose derived LLM tool slug matches name,
// so a non-ASCII-named skill exposed under a slug still routes on tool calls.
func (e *ToolExecutor) resolveSkillBySlug(name string) (skill.Skill, bool) {
	if e.skills == nil {
		return nil, false
	}
	for _, s := range e.skills.All() {
		if llmToolNameSlug(s.Name()) == name {
			return s, true
		}
	}
	return nil, false
}

// HasTool checks whether a tool exists in any source.
func (e *ToolExecutor) HasTool(toolName string) bool {
	if e.skills != nil {
		if _, ok := e.skills.Get(toolName); ok {
			return true
		}
		if _, ok := e.resolveSkillBySlug(toolName); ok {
			return true
		}
	}
	if e.mcpMgr != nil {
		for _, info := range e.mcpMgr.ListToolInfos() {
			if info.Name == toolName {
				return true
			}
		}
	}
	return false
}

// isBuiltinSkillName checks whether the given tool name is registered as a builtin skill.
func (e *ToolExecutor) isBuiltinSkillName(name string) bool {
	_, ok := e.skills.Get(name)
	return ok
}

func (e *ToolExecutor) executeWithHooks(ctx context.Context, call *ToolCallInfo, exec func(context.Context) (string, error)) (string, error) {
	v2 := featureflag.Enabled(ctx, FlagToolLifecycleV2)

	beforeHooks := e.beforeHooks
	afterHooks := e.afterHooks
	if v2 {
		beforeHooks = sortBeforeHooks(beforeHooks)
		afterHooks = sortAfterHooks(afterHooks)
	}

	// Before hooks
	for _, h := range beforeHooks {
		if err := h.BeforeToolCall(ctx, call); err != nil {
			return "", fmt.Errorf("tool call blocked by hook: %w", err)
		}
	}

	// Execute
	startedAt := time.Now()
	content, err := exec(ctx)
	result := &ToolCallResult{Content: content, Error: err}
	if v2 {
		result.StartedAt = startedAt
		result.Duration = time.Since(startedAt)
	}

	// After hooks (run even on error for audit)
	for _, h := range afterHooks {
		runAfterHook(ctx, h, call, result, v2)
	}
	labelToolResultEgress(ctx, call.Name)

	// v0.4.0 H6：投递结构化事件；是否落盘/外发由注入的 events.Sink 决定。
	severity := events.SeverityInfo
	if result.Error != nil {
		severity = events.SeverityError
	}
	_ = events.Emit(ctx, events.New("tool.call.completed", severity).
		WithSource("engine.tool_executor").
		With("tool", call.Name).
		With("source", call.Source).
		With("ok", result.Error == nil).
		With("duration_ms", result.Duration.Milliseconds()).
		With("content_len", len(result.Content)))

	return result.Content, result.Error
}

// runAfterHook 在 lifecycle.v2 启用时用 panic recover 隔离单个 hook 失败，避免
// Audit / metrics 类 hook 抛出 panic 把整个调用链炸掉。flag 关闭时退化成直接调用，
// 与 v0.3 行为完全一致。
func runAfterHook(ctx context.Context, h AfterToolHook, call *ToolCallInfo, result *ToolCallResult, v2 bool) {
	if !v2 {
		h.AfterToolCall(ctx, call, result)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			trace.L(ctx).Error("after-tool hook panicked",
				"tool", call.Name,
				"hook", fmt.Sprintf("%T", h),
				"panic", r,
			)
		}
	}()
	h.AfterToolCall(ctx, call, result)
}

// runSkillViaPipeline 通过 7 阶段 skill.RunPipeline 执行命名 Skill。
//
// 与 RunSkillByPipeline（按 query 召回）不同：这里调用方已经知道精确 toolName，
// 用 toolName 同时作为 Query —— 名称完全匹配在 SelectByContext 中得分 +100，
// Discovery 阶段几乎必然命中。
//
// 防御：若 Discovery 召回的 skill 名称与请求的 toolName 不一致（例如被 when/not_when
// 过滤后命中了同义 skill），返回 error 让调用方决定是否退化；不静默路由到错误工具。
//
// flag OFF 时由调用方退化到 s.Execute；本函数假设 flag 已 ON。
func (e *ToolExecutor) runSkillViaPipeline(ctx context.Context, toolName string, args map[string]any) (string, error) {
	if e.skills == nil {
		return "", fmt.Errorf("runSkillViaPipeline: nil skill registry")
	}
	res, err := skill.RunPipeline(ctx, e.skills, skill.PipelineOptions{
		Query: toolName,
		Args:  args,
		TopK:  1,
	})
	if err != nil {
		return "", err
	}
	if res == nil || res.Result == nil {
		return "", fmt.Errorf("runSkillViaPipeline: %q produced no result", toolName)
	}
	if res.Skill != nil && res.Skill.Name() != toolName {
		return "", fmt.Errorf("runSkillViaPipeline: pipeline routed %q → %q (refusing to execute mismatched skill)", toolName, res.Skill.Name())
	}
	// BUG-1：pipeline 路径同样透传结构化 reply-safe 元数据（record chip 等）。
	stampToolReplyMeta(ctx, res.Result.Metadata)
	return res.Result.Content, nil
}

// InitLifecycle 调用所有实现 LifecycleTool 的 Skill 的 Init —— 仅在 lifecycle.v2
// flag 启用时生效；flag 关闭时返回 nil（no-op）。任一 Init 返回 error 立即终止并上抛。
//
// 用法（cmd/hexclaw/main.go 启动阶段）：
//
//	if err := toolExec.InitLifecycle(ctx); err != nil {
//	    return fmt.Errorf("tool lifecycle init failed: %w", err)
//	}
func (e *ToolExecutor) InitLifecycle(ctx context.Context) error {
	if !featureflag.Enabled(ctx, FlagToolLifecycleV2) {
		return nil
	}
	if e.skills == nil {
		return nil
	}
	for _, s := range e.skills.All() {
		lt, ok := s.(LifecycleTool)
		if !ok {
			continue
		}
		if err := lt.Init(ctx); err != nil {
			return fmt.Errorf("init lifecycle tool %q: %w", s.Name(), err)
		}
	}
	return nil
}

// ShutdownLifecycle 调用所有实现 LifecycleTool 的 Skill 的 Shutdown —— 仅在
// lifecycle.v2 flag 启用时生效。Shutdown error 仅记日志，继续清理后续 tool（避免
// 一个工具关闭失败把整个 graceful shutdown 卡住）。
func (e *ToolExecutor) ShutdownLifecycle(ctx context.Context) {
	if !featureflag.Enabled(ctx, FlagToolLifecycleV2) {
		return
	}
	if e.skills == nil {
		return
	}
	for _, s := range e.skills.All() {
		lt, ok := s.(LifecycleTool)
		if !ok {
			continue
		}
		if err := lt.Shutdown(ctx); err != nil {
			trace.L(ctx).Warn("lifecycle tool shutdown failed",
				"tool", s.Name(),
				"err", err,
			)
		}
	}
}
