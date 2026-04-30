package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/events"
)

// LLMCallContext 装载一次 failover-aware LLM 调用所需的全部依赖。
//
// 字段会被 CompleteWithFailover / StreamWithFailover 在 fail-over 过程中就地修改
// （切 Provider 时 Provider/ProviderName 会被更新成 fallback 的）。调用方在拿到
// 返回值后可以读取最终生效的 ProviderName 用于打点 / 计费。
type LLMCallContext struct {
	Provider     hexagon.Provider
	ProviderName string
	ModelName    string

	// Fallback 在 FailoverAction.SwitchProvider/SwitchCredential 为 true 时被调用，
	// 用于挑出下一个候选 Provider；exclude 列出已尝试过的 Provider 名，避免循环。
	// 没有可用候选时返回非 nil error，wrapper 会上抛最后一次原始错误。
	Fallback func(exclude ...string) (hexagon.Provider, string, error)

	// Compress 在 FailContextTooLong 时被调用，原地修改 req（截断 / 摘要）。
	// 返回 true 表示压缩成功值得重试；false 表示无法继续压缩，wrapper 立即终止。
	Compress func(req *llm.CompletionRequest) bool

	// Logger 用于打 failover 转移日志。nil 时静默。
	Logger func(msg string, fields ...any)

	// Middlewares v0.4.0 H8：把 Provider 包装一层 ProviderMiddleware（rate limit /
	// observe / prompt rewrite 等）；flag model.gateway.v1 OFF 时被忽略。
	// 列表会以洋葱模型应用：第一个最外层、最后一个最里层。
	Middlewares []ProviderMiddleware
}

// applyGateway 把 LLMCallContext.Middlewares 通过 Chain 包装到 fc.Provider 上。
// flag model.gateway.v1 OFF 时 Chain 直接返回 inner，所以仅在 flag ON 时实际起作用。
func (fc *LLMCallContext) applyGateway(ctx context.Context) hexagon.Provider {
	if len(fc.Middlewares) == 0 {
		return fc.Provider
	}
	return Chain(ctx, fc.Provider, fc.Middlewares...)
}

// maxFailoverIterations 是兜底硬上限，防止 Compress / Fallback 配合错误把循环推进死。
const maxFailoverIterations = 6

// errNilCallContext 是非用户可见错误，仅用于参数校验。
var errNilCallContext = errors.New("CompleteWithFailover: nil provider context")

// CompleteWithFailover 把 ClassifyError + HandleFailover 包装到 provider.Complete
// 入口处，按 reason → action 映射做精确 failover：
//   - FailRateLimit:       退避 BackoffSeconds 后同 Provider 重试，最多 MaxRetries 次
//   - FailQuotaExceeded:   切 Fallback Provider（视作换凭证）
//   - FailContextTooLong:  调用 Compress 压缩请求后同 Provider 重试一次
//   - FailProviderDown:    切 Fallback Provider；exhausted 即终止
//   - FailInvalidKey/ModelNotFound: 立即返回（Retry=false）
//   - FailUnknown:         同 Provider 重试 1 次
//
// fc 字段会被原地更新（Provider / ProviderName 反映最终生效的）。返回的错误已经
// 用 fmt.Errorf 包了 user-facing 描述，调用方可用 errors.Is/Unwrap 拿原始 err。
func CompleteWithFailover(
	ctx context.Context,
	fc *LLMCallContext,
	req llm.CompletionRequest,
) (*llm.CompletionResponse, error) {
	if fc == nil || fc.Provider == nil {
		return nil, errNilCallContext
	}

	tried := make([]string, 0, 4)
	if fc.ProviderName != "" {
		tried = append(tried, fc.ProviderName)
	}

	var lastErr error
	var sameProviderRetries int
	startedAt := time.Now()

	for i := 0; i < maxFailoverIterations; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		resp, err := fc.applyGateway(ctx).Complete(ctx, req)
		if err == nil {
			// v0.4.0 H6：成功投递事件
			_ = events.Emit(ctx, events.New("llm.call.completed", events.SeverityInfo).
				WithSource("engine.llm_call").
				With("provider", fc.ProviderName).
				With("model", fc.ModelName).
				With("retries", i).
				With("duration_ms", time.Since(startedAt).Milliseconds()))
			return resp, nil
		}
		lastErr = err

		reason := ClassifyError(err, 0, "")
		action := HandleFailover(reason)
		// v0.4.0 H6：每次 failover 投递事件
		_ = events.Emit(ctx, events.New("llm.call.failover", events.SeverityWarn).
			WithSource("engine.llm_call").
			With("provider", fc.ProviderName).
			With("reason", reason.String()).
			With("iter", i))
		if fc.Logger != nil {
			fc.Logger("LLM call failover",
				"provider", fc.ProviderName,
				"reason", reason.String(),
				"user_facing", action.UserFacing,
				"iter", i,
			)
		}

		if !action.Retry {
			return nil, fmt.Errorf("%s: %w", action.UserFacing, err)
		}

		switch {
		case action.SwitchProvider, action.SwitchCredential:
			if fc.Fallback == nil {
				return nil, fmt.Errorf("%s (no fallback): %w", action.UserFacing, err)
			}
			next, name, fbErr := fc.Fallback(tried...)
			if fbErr != nil {
				return nil, fmt.Errorf("%s (fallback exhausted: %v): %w", action.UserFacing, fbErr, err)
			}
			fc.Provider = next
			fc.ProviderName = name
			tried = append(tried, name)
			sameProviderRetries = 0

		case action.CompressContext:
			if fc.Compress == nil || !fc.Compress(&req) {
				return nil, fmt.Errorf("%s (compress unavailable): %w", action.UserFacing, err)
			}
			sameProviderRetries++

		default:
			// 同 Provider 退避重试（rate-limit / unknown）
			sameProviderRetries++
			if sameProviderRetries > action.MaxRetries {
				return nil, fmt.Errorf("%s (retries exhausted): %w", action.UserFacing, err)
			}
			if action.BackoffSeconds > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(action.BackoffSeconds) * time.Second):
				}
			}
		}
	}

	return nil, fmt.Errorf("failover exhausted iterations: %w", lastErr)
}

// StreamWithFailover 与 Complete 版本同语义，但只能在"未消费第一个 chunk 之前"
// 做 fail-over —— 一旦 provider.Stream 成功返回 *llm.Stream，调用方已经持有半开
// 流，里面的错误不能由本 wrapper 兜底，需要调用方自行处理。
func StreamWithFailover(
	ctx context.Context,
	fc *LLMCallContext,
	req llm.CompletionRequest,
) (*llm.Stream, error) {
	if fc == nil || fc.Provider == nil {
		return nil, errNilCallContext
	}

	tried := make([]string, 0, 4)
	if fc.ProviderName != "" {
		tried = append(tried, fc.ProviderName)
	}

	var lastErr error
	var sameProviderRetries int

	for i := 0; i < maxFailoverIterations; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		stream, err := fc.applyGateway(ctx).Stream(ctx, req)
		if err == nil {
			return stream, nil
		}
		lastErr = err

		reason := ClassifyError(err, 0, "")
		action := HandleFailover(reason)
		if fc.Logger != nil {
			fc.Logger("LLM stream failover",
				"provider", fc.ProviderName,
				"reason", reason.String(),
				"user_facing", action.UserFacing,
				"iter", i,
			)
		}

		if !action.Retry {
			return nil, fmt.Errorf("%s: %w", action.UserFacing, err)
		}

		switch {
		case action.SwitchProvider, action.SwitchCredential:
			if fc.Fallback == nil {
				return nil, fmt.Errorf("%s (no fallback): %w", action.UserFacing, err)
			}
			next, name, fbErr := fc.Fallback(tried...)
			if fbErr != nil {
				return nil, fmt.Errorf("%s (fallback exhausted: %v): %w", action.UserFacing, fbErr, err)
			}
			fc.Provider = next
			fc.ProviderName = name
			tried = append(tried, name)
			sameProviderRetries = 0

		case action.CompressContext:
			if fc.Compress == nil || !fc.Compress(&req) {
				return nil, fmt.Errorf("%s (compress unavailable): %w", action.UserFacing, err)
			}
			sameProviderRetries++

		default:
			sameProviderRetries++
			if sameProviderRetries > action.MaxRetries {
				return nil, fmt.Errorf("%s (retries exhausted): %w", action.UserFacing, err)
			}
			if action.BackoffSeconds > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(action.BackoffSeconds) * time.Second):
				}
			}
		}
	}

	return nil, fmt.Errorf("failover exhausted iterations: %w", lastErr)
}
