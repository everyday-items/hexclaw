package engine

// BUG-20260611 (review H4/M5): the semantic-cache and skill-fast-path guards
// only covered cron dispatches. Heartbeat / webhook / spawn dispatches are the
// same bug class: their instruction text is static, so the first tick caches
// the result and every later tick becomes a ~ms fake cache hit instead of a
// real execution.
//
// Contract:
//   - any system-dispatched message (source ∈ cron/heartbeat/webhook/spawn)
//     must NOT read from the semantic cache (H4)
//   - must NOT match the skill keyword fast path (H4)
//   - must NOT have its result written into the semantic cache, so a user
//     typing identical text never receives a stale system-dispatch output (M5)
//   - the dispatch guard must be evaluated BEFORE cache.Get so discarded
//     lookups do not mutate hit counters (review Low)

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

// newCountingProvider returns a provider whose replies are unique per call
// ("fresh-1", "fresh-2", …) so cache hits are distinguishable from real runs.
func newCountingProvider() *mockllm.LLMProvider {
	var n int32
	return mockllm.NewLLMProvider("test").WithResponseFn(func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{
			Content: fmt.Sprintf("fresh-%d", atomic.AddInt32(&n, 1)),
			Usage:   hexagon.Usage{TotalTokens: 10},
		}, nil
	})
}

func TestBug20260611_HeartbeatDispatchBypassesSemanticCache(t *testing.T) {
	eng := newEngineWithProvider(t, newCountingProvider())
	ctx := context.Background()

	const instructions = "检查系统状态并汇报今天的待办重点"

	// 1. A normal user message warms the cache with fresh-1.
	userReply, err := eng.Process(ctx, &adapter.Message{
		ID: "msg-u1", Platform: adapter.PlatformAPI,
		UserID: "user-1", ChatID: "chat-1", Content: instructions,
	})
	if err != nil {
		t.Fatalf("Process(user) failed: %v", err)
	}
	if userReply.Content != "fresh-1" {
		t.Fatalf("precondition: want fresh-1, got %q", userReply.Content)
	}

	// 2. A heartbeat tick with the identical static instruction text must
	//    re-execute, not serve the cached result.
	hbReply, err := eng.Process(ctx, &adapter.Message{
		ID: "msg-hb1", Platform: adapter.PlatformAPI,
		UserID: "user-hb", ChatID: "chat-hb", Content: instructions,
		Metadata: map[string]string{"source": "heartbeat"},
	})
	if err != nil {
		t.Fatalf("Process(heartbeat) failed: %v", err)
	}
	if hbReply.Content == userReply.Content {
		t.Errorf("[BUG-20260611 H4] heartbeat tick was served from the semantic cache: %q", hbReply.Content)
	}
	if hbReply.Metadata["source"] == "cache" {
		t.Error("[BUG-20260611 H4] heartbeat reply metadata claims a cache hit")
	}

	// 3. The guard must run before cache.Get: a bypassed lookup must not
	//    bump hit counters (review Low: evaluation order of the guard).
	if hits := eng.LLMCache().Stats().Hits; hits != 0 {
		t.Errorf("[BUG-20260611 Low] dispatch guard evaluated after cache.Get: hit counter mutated (hits=%d)", hits)
	}

	// 4. The heartbeat result must not overwrite the cached user entry (M5):
	//    the same user text must still resolve to the original answer.
	userReply2, err := eng.Process(ctx, &adapter.Message{
		ID: "msg-u2", Platform: adapter.PlatformAPI,
		UserID: "user-1", ChatID: "chat-1", Content: instructions,
	})
	if err != nil {
		t.Fatalf("Process(user 2nd) failed: %v", err)
	}
	if userReply2.Content != userReply.Content {
		t.Errorf("[BUG-20260611 M5] heartbeat output leaked into the user-facing cache: want %q, got %q",
			userReply.Content, userReply2.Content)
	}
}

func TestBug20260611_SystemDispatchSkipsSkillFastPath(t *testing.T) {
	for _, source := range []string{"heartbeat", "webhook", "spawn"} {
		t.Run(source, func(t *testing.T) {
			skills := skill.NewRegistry()
			if err := skills.Register(&echoSkill{}); err != nil {
				t.Fatalf("register skill: %v", err)
			}
			eng := newEngineWithProviderAndSkills(t, newCountingProvider(), skills)

			reply, err := eng.Process(context.Background(), &adapter.Message{
				ID: "msg-" + source, Platform: adapter.PlatformAPI,
				UserID: "user-1", ChatID: "chat-1",
				Content:  "/echo system instruction text",
				Metadata: map[string]string{"source": source},
			})
			if err != nil {
				t.Fatalf("Process failed: %v", err)
			}
			if strings.HasPrefix(reply.Content, "echo: ") {
				t.Errorf("[BUG-20260611 H4] %s dispatch hijacked by skill keyword fast path: %q", source, reply.Content)
			}
		})
	}
}

func TestIsSystemDispatch(t *testing.T) {
	if isSystemDispatch(nil) {
		t.Error("nil message must not be a system dispatch")
	}
	if isSystemDispatch(&adapter.Message{Content: "hi"}) {
		t.Error("plain user message must not be a system dispatch")
	}
	if isSystemDispatch(&adapter.Message{Metadata: map[string]string{"source": "web"}}) {
		t.Error("source=web must not be a system dispatch")
	}
	for _, source := range []string{"cron", "heartbeat", "webhook", "spawn"} {
		if !isSystemDispatch(&adapter.Message{Metadata: map[string]string{"source": source}}) {
			t.Errorf("source=%s must be a system dispatch", source)
		}
	}
	// the cron helper stays narrow
	if isCronDispatch(&adapter.Message{Metadata: map[string]string{"source": "heartbeat"}}) {
		t.Error("isCronDispatch must stay cron-only")
	}
}

func TestBug20260611_CronDispatchResultNotCached(t *testing.T) {
	eng := newEngineWithProvider(t, newCountingProvider())
	ctx := context.Background()

	const prompt = "总结今天的科技新闻要点"
	cronReply, err := eng.Process(ctx, NewCronDispatchMessage("user-1", "chat-1", "job-1", prompt))
	if err != nil {
		t.Fatalf("Process(cron) failed: %v", err)
	}
	if cronReply.Content != "fresh-1" {
		t.Fatalf("precondition: want fresh-1 from cron run, got %q", cronReply.Content)
	}

	if entries := eng.LLMCache().Stats().Entries; entries != 0 {
		t.Errorf("[BUG-20260611 M5] cron dispatch result was written into the semantic cache (entries=%d)", entries)
	}

	// A user typing the identical text afterwards must get a real LLM run,
	// not the cron job's output.
	userReply, err := eng.Process(ctx, &adapter.Message{
		ID: "msg-user", Platform: adapter.PlatformAPI,
		UserID: "user-2", ChatID: "chat-2", Content: prompt,
	})
	if err != nil {
		t.Fatalf("Process(user) failed: %v", err)
	}
	if userReply.Content == cronReply.Content {
		t.Errorf("[BUG-20260611 M5] user received the cached cron output: %q", userReply.Content)
	}
}
