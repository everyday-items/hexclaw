package engine

// 本地模型预热（BUG-20260710 · qwen3.5 走应用慢 vs 直连快 根因修复之二）。
//
// 真机取证（qwen3.5:9b 纯 CPU）：应用一次请求 prompt≈7943 token（SOUL+操作手册+工具
// schema），prefill 23 tok/s → 冷路径 344s；Ollama KV 前缀缓存命中后热路径 46s。
// 修复组合：①ai-core ollama 客户端随请求下发 keep_alive=30m（模型+KV 缓存驻留）
// ②本文件：启动后台预热——用与真实聊天同构的请求把巨型前缀先行 prefill 进 KV 缓存，
// 用户首条消息直接走热路径。预热消息不建会话、不写库、memory=off，纯 provider 直调。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hexagon-codes/ai-core/llm"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/util/logger"
)

const defaultLocalWarmupTimeout = 10 * time.Minute

// WarmupHandle controls one background warmup and exposes deterministic
// shutdown semantics to the engine and callers.
type WarmupHandle struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	err error
}

func (h *WarmupHandle) Cancel() {
	if h != nil && h.cancel != nil {
		h.cancel()
	}
}

func (h *WarmupHandle) Wait(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-h.done:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *WarmupHandle) finish(err error) {
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	close(h.done)
}

// WarmupLocalDefaultModel 若默认路由是本地模型（ollama/local），发送一次与真实聊天
// 同构（system prompt + 基础工具集）的 1-token 请求，把大前缀 prefill 进 KV 缓存。
// 返回 warmed=false 表示默认路由非本地、按设计跳过（云端无预热需求）。
func (e *ReActEngine) WarmupLocalDefaultModel(ctx context.Context) (warmed bool, err error) {
	ctx = egress.WithRequest(ctx, egress.PurposeGeneralChat, "warmup", egress.ClassGeneral)
	// 与日常提问同构的普通中文短句：工具渐进召回（CollectFiltered 按 query）对无关键词
	// 文本返回基础工具集——与多数真实首问相同，前缀才能命中。
	msg := &adapter.Message{
		ID:        "warmup",
		Content:   "你好",
		Timestamp: time.Now(),
		Metadata:  map[string]string{"memory": "off", "source": "warmup"},
	}
	selection, serr := e.resolveLLMSelection(ctx, msg)
	if serr != nil {
		return false, serr
	}
	if !isLocalProvider(selection.providerName) {
		return false, nil
	}

	// 关键：KV 前缀缓存按**逐字节**匹配。system prompt 里有按 metadata 变化的装饰
	//（引擎身份行「当前搭载 X 的 Y」、locale 输出指令），真实桌面请求恒带
	// provider/model/user_locale —— 预热必须镜像同一组值，否则前缀在工具区之前
	// 就分叉、预热白做（真机取证：B4 预热后首问仍 363s，对齐元数据后 ~46s）。
	// user_locale 取产品默认 zh-CN；非中文桌面的首问仍冷（P2：待后端拿到系统语言配置）。
	msg.Metadata["provider"] = selection.providerName
	msg.Metadata["model"] = selection.modelName
	msg.Metadata["user_locale"] = "zh-CN"

	// 工具收集近似复刻真实聊天路径（Process/Stream 的收集块）：偏差只影响预热命中率，
	// 不影响正确性——system prompt（占前缀大头）由 buildCompletionRequest 精确同构。
	var tools []llm.ToolDefinition
	toolsCfg := e.cfg.LLM.Tools
	if e.toolCollector != nil && resolveToolsEnabledForMessage(toolsCfg, true, msg.Metadata) {
		tools = e.toolCollector.CollectFiltered(msg.Content, skill.Activation{
			Mode: string(ResolveMode(msg.Metadata["agent_mode"], msg.Content)),
		})
		tools = e.ensureSystemDispatchToolFloor(tools, msg)
		if cap := effectiveMaxTools(toolsCfg.MaxTools, msg); cap > 0 && len(tools) > cap {
			tools = tools[:cap]
		}
	}

	req := e.buildCompletionRequest(ctx, msg, nil, "")
	if len(tools) > 0 {
		req.Tools = tools
	}
	applyPerTurnRequestPolicy(&req, selection.modelName, msg, nil)
	// BUG-20260712：预热必须与真实请求用同一 num_ctx，否则预热按自动分档把 runner 载到 16384
	// （巨型 KV 常驻），真实请求即便注入 num_ctx=4096 也只会复用那个 16384 runner（Ollama 不会
	// 为更小 ctx 降载），内存照样被撑爆。此处显式盖同一档，让 runner 一开始就载在 4096。
	e.applyLocalNumCtxCap(&req, true) // 到此 selection 必为本地（上方已 return 云端）
	req.MaxTokens = 1                 // 只要 prefill 进缓存，产出 1 个 token 即停

	start := time.Now()
	logger.Info("[warmup] 本地默认模型预热开始（把 system prompt+工具模板 prefill 进 KV 缓存）",
		"provider", selection.providerName, "model", selection.modelName,
		"tools", len(req.Tools), "num_ctx", reqNumCtxField(req), "prompt_bytes", promptBytesField(req))
	// 必须走流式：非流式 Complete 的响应头超时 120s < 纯 CPU 大 prompt prefill（实测 344s），
	// 预热自己就会超时（首版踩坑取证 sidecar2.log）。流式响应头即刻返回，prefill 期间连接保活。
	stream, cerr := selection.provider.Stream(ctx, req)
	if cerr != nil {
		return true, cerr
	}
	for range stream.Chunks() {
		// 排空至流结束（num_predict=1，产出即止）
	}
	select {
	case serr, ok := <-stream.Errors():
		if ok && serr != nil {
			return true, serr
		}
	default:
	}
	logger.Info("[warmup] 本地默认模型预热完成（system prompt+工具模板已入 KV 缓存）",
		"provider", selection.providerName, "model", selection.modelName,
		"num_ctx", reqNumCtxField(req), "prompt_bytes", promptBytesField(req),
		"elapsed", time.Since(start).Round(time.Second).String())
	return true, nil
}

// StartLocalWarmup 后台预热入口（serve 启动尾调用）：不阻塞启动，失败仅告警。
// 可选 timeout 主要用于测试/受限环境；省略时使用十分钟上限（覆盖纯 CPU 大前缀实测耗时）。
func (e *ReActEngine) StartLocalWarmup(ctx context.Context, timeout ...time.Duration) *WarmupHandle {
	if ctx == nil {
		ctx = context.Background()
	}
	limit := defaultLocalWarmupTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		limit = timeout[0]
	}
	runCtx, cancel := context.WithTimeout(ctx, limit)
	handle := &WarmupHandle{cancel: cancel, done: make(chan struct{})}

	e.mu.Lock()
	previous := e.warmup
	e.warmup = handle
	e.mu.Unlock()
	if previous != nil {
		previous.Cancel()
	}

	e.bgWg.Add(1)
	go func() {
		defer e.bgWg.Done()
		defer cancel()
		var runErr error
		// BUG-20260710-H2：纯性能优化路径的 panic 不允许带崩进程（同 tool_executor recover 范式）。
		defer func() {
			if r := recover(); r != nil {
				runErr = fmt.Errorf("warmup panic: %v", r)
				logger.Warn("[warmup] 预热 goroutine panic（已吞，不影响功能）", "panic", r)
			}
			if runErr == nil && runCtx.Err() != nil {
				runErr = runCtx.Err()
			}
			handle.finish(runErr)
			e.mu.Lock()
			if e.warmup == handle {
				e.warmup = nil
			}
			e.mu.Unlock()
		}()
		warmed, err := e.WarmupLocalDefaultModel(runCtx)
		runErr = err
		switch {
		case err != nil:
			logger.Warn("[warmup] 本地模型预热失败（不影响功能，首条消息将走冷路径）", "error", err)
		case !warmed:
			logger.Info("[warmup] 默认路由非本地模型，跳过预热")
		}
	}()
	return handle
}
