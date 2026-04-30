// cases_real.go 实现 v0.4.0 H9 真业务 EvalCase（替换 V04Suite 中部分 mock case）。
//
// 与 cases (mock) 的区别：本文件每个 case.Run 真调 hexclaw 内部生产代码路径
// （engine.HeuristicCompress / engine.ClassifyError / skill.RunPipeline /
// engine.DefaultBaselinePolicy / events.Emitter），断言真实行为是否符合契约。
//
// 5 条真业务 case + 7 条 mock case 配套使用：
//   - mock case 验证协议形态稳定（CI 快、零依赖）
//   - real case 验证生产代码 v0.4.0 关键不变量没退化
//
// 通过 V04SuiteFull() 取得 5+7=12 case 的完整套件；V04Suite() 仍返回 10 条
// mock 套件以兼容已有调用方。
package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/events"
	"github.com/hexagon-codes/hexclaw/featureflag"
	"github.com/hexagon-codes/hexclaw/skill"
)

// V04SuiteFull 返回 v0.4.0 完整套件（mock + real 共 12 条）。
func V04SuiteFull() *Suite {
	mock := V04Suite()
	cases := append([]EvalCase{}, mock.Cases...)
	cases = append(cases,
		caseRealHeuristicCompress(),
		caseRealClassifyRateLimit(),
		caseRealSkillPipelineFlagOff(),
		caseRealPolicyDenyShell(),
		caseRealEventsEmitterRoundtrip(),
	)
	return &Suite{Cases: cases}
}

// caseRealHeuristicCompress 真调 engine.HeuristicCompress：注入 6 条带重复 user
// 的 history → 断言去重 + tool pair 截断生效（消息数 < 原始）。
func caseRealHeuristicCompress() EvalCase {
	return EvalCase{
		ID:          "real-heuristic-compress",
		Description: "真调 HeuristicCompress 验证去重 + tool pair 截断",
		Tags:        []string{"real", "compression", "K12"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			start := time.Now()
			hist := []llm.Message{
				{Role: "system", Content: "you are an assistant"},
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "hi"},
				{Role: "user", Content: "Hello"},  // 大小写重复
				{Role: "user", Content: " hello "}, // 前后空格重复
				{Role: "assistant", ToolCalls: []llm.ToolCallRef{{ID: "1"}}},
				{Role: "tool", Content: "r1"},
				{Role: "assistant", ToolCalls: []llm.ToolCallRef{{ID: "2"}}},
				{Role: "tool", Content: "r2"},
				{Role: "assistant", ToolCalls: []llm.ToolCallRef{{ID: "3"}}},
				{Role: "tool", Content: "r3"},
			}
			compressed := engine.HeuristicCompress(hist, engine.HeuristicCompressOptions{KeepRecentToolPairs: 1})

			// 统计 user 数应为 1（去重后）
			userCount := 0
			for _, m := range compressed {
				if strings.EqualFold(string(m.Role), "user") {
					userCount++
				}
			}
			out := Output{
				Content:    fmt.Sprintf("compressed %d → %d msgs, user=%d", len(hist), len(compressed), userCount),
				DurationMs: time.Since(start).Milliseconds(),
			}
			if len(compressed) >= len(hist) {
				return out, fmt.Errorf("expected message count to drop; got %d ≥ %d", len(compressed), len(hist))
			}
			if userCount != 1 {
				return out, fmt.Errorf("expected user dedup to 1; got %d", userCount)
			}
			return out, nil
		},
		Assertions: []Assertion{AssertNoError(), AssertContent("user=1")},
	}
}

// caseRealClassifyRateLimit 真调 engine.ClassifyError：用 "rate limit exceeded"
// 触发 FailRateLimit → HandleFailover 应返回 Retry=true。
func caseRealClassifyRateLimit() EvalCase {
	return EvalCase{
		ID:          "real-classify-rate-limit",
		Description: "真调 ClassifyError + HandleFailover 验证 429 路径",
		Tags:        []string{"real", "llm", "failover"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			err := errors.New("rate limit exceeded: please retry after 1s")
			reason := engine.ClassifyError(err, 429, "")
			action := engine.HandleFailover(reason)
			out := Output{
				Content: fmt.Sprintf("reason=%s retry=%v backoff=%ds",
					reason.String(), action.Retry, action.BackoffSeconds),
			}
			if reason != engine.FailRateLimit {
				return out, fmt.Errorf("expected FailRateLimit; got %s", reason.String())
			}
			if !action.Retry {
				return out, fmt.Errorf("expected Retry=true on rate limit")
			}
			return out, nil
		},
		Assertions: []Assertion{AssertNoError(), AssertContent("reason=rate_limit"), AssertContent("retry=true")},
	}
}

// caseRealSkillPipelineFlagOff 真调 skill.RunPipeline：flag OFF 时应返回
// ErrPipelineDisabled，让调用方退化到老路径。
func caseRealSkillPipelineFlagOff() EvalCase {
	return EvalCase{
		ID:          "real-skill-pipeline-flag-off",
		Description: "真调 RunPipeline，flag OFF 应返回 ErrPipelineDisabled",
		Tags:        []string{"real", "skill", "pipeline"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			reg := skill.NewRegistry()
			// 不注入 flag → 默认 OFF
			ctx := context.Background()
			_, err := skill.RunPipeline(ctx, reg, skill.PipelineOptions{Query: "x"})
			out := Output{Content: fmt.Sprintf("err=%v", err)}
			if !errors.Is(err, skill.ErrPipelineDisabled) {
				return out, fmt.Errorf("expected ErrPipelineDisabled; got %v", err)
			}
			return out, nil
		},
		Assertions: []Assertion{AssertNoError(), AssertContent("disabled")},
	}
}

// caseRealPolicyDenyShell 真调 DefaultBaselinePolicy.Evaluate 对 shell 工具
// 应返回 require_approval 或 deny（K12 安全底线，永不 allow）。
func caseRealPolicyDenyShell() EvalCase {
	return EvalCase{
		ID:          "real-policy-deny-shell",
		Description: "真调 DefaultBaselinePolicy 对 shell 工具至少应 require_approval",
		Tags:        []string{"real", "permission", "policy", "K12"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			policy := engine.DefaultBaselinePolicy()
			decision := policy.Evaluate(&engine.ToolCallInfo{Name: "shell", Source: "skill"})
			out := Output{Content: fmt.Sprintf("action=%s rule=%s", decision.Action, decision.MatchedRule)}
			if decision.Action == engine.ActionAllow {
				return out, fmt.Errorf("shell 永不能直接 allow；got %s", decision.Action)
			}
			return out, nil
		},
		Assertions: []Assertion{AssertNoError()},
	}
}

// caseRealEventsEmitterRoundtrip 真调 events.NewEmitter + Emit + MemorySink
// 验证事件投递端到端可用（不经过 ctx flag gate，直接 emitter.Emit）。
func caseRealEventsEmitterRoundtrip() EvalCase {
	return EvalCase{
		ID:          "real-events-emitter-roundtrip",
		Description: "真调 events.Emitter + MemorySink 验证投递契约",
		Tags:        []string{"real", "events", "observability"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			sink := events.NewMemorySink()
			emitter := events.NewEmitter(sink, "eval.real")
			ctx := featureflag.WithContext(context.Background(),
				featureflag.NewStatic(featureflag.Registered(), map[string]bool{events.FlagEventsTransportV1: true}))
			ctx = events.WithEmitter(ctx, emitter)

			ev := events.New("test.real.event", events.SeverityInfo).With("k", "v")
			if err := events.Emit(ctx, ev); err != nil {
				return Output{}, err
			}

			got := sink.Events()
			out := Output{
				Content: fmt.Sprintf("sink got %d event(s)", len(got)),
				Events:  collectEventTypes(got),
			}
			if len(got) != 1 || got[0].Type != "test.real.event" {
				return out, fmt.Errorf("expected 1 event type=test.real.event; got %d %v", len(got), out.Events)
			}
			return out, nil
		},
		Assertions: []Assertion{AssertNoError(), AssertEventEmitted("test.real.event")},
	}
}

// collectEventTypes 从 events.Event 列表抽出 Type 字段供 Output.Events 使用。
func collectEventTypes(evts []events.Event) []string {
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		out = append(out, e.Type)
	}
	return out
}
