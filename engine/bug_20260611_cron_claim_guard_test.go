package engine

// BUG-20260611 (review M6): the cron-creation hallucination guard
// (detectCronCreationClaim + hasCronTaskCall) was only wired into the
// stream-with-tools finalize path (finalizeRuntimeStreamResult). It was
// missing from:
//   - finalizeReply — the non-stream Process path, which is exactly what the
//     cron AgentRunner and sync API callers use
//   - the no-tools stream path — the weak-local-model scenario where a
//     hallucinated "task created" claim is most likely
//
// Contract: every finalize path must append the visible correction notice
// when the reply claims a scheduled task was created without a cron_task
// tool call in this round, and the hallucinated reply must never be cached.
//
// Review Low (cross-turn false positive): the guard only fires on turns where
// cron intent guidance was active, so a model truthfully RESTATING a
// prior-turn creation in an unrelated turn is not flagged.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
)

const hallucinatedCronClaim = "已为你创建定时任务：每天上午 10 点采集微博热搜，将准时执行。"

func newClaimProvider() *mockllm.LLMProvider {
	return mockllm.NewLLMProvider("test").WithResponseFn(func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{
			Content: hallucinatedCronClaim,
			Usage:   hexagon.Usage{TotalTokens: 10},
		}, nil
	})
}

// Non-stream Process path (finalizeReply) — the path cron-adjacent chat and
// the sync API actually use.
func TestBug20260611_CronClaimGuard_NonStreamProcess(t *testing.T) {
	eng := newEngineWithProvider(t, newClaimProvider())

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-claim-sync", Platform: adapter.PlatformAPI,
		UserID: "user-1", ChatID: "chat-1",
		Content: "每天上午10点采集微博热搜",
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if !strings.Contains(reply.Content, "系统校验") {
		t.Errorf("[BUG-20260611 M6] non-stream path missed the hallucinated creation claim, reply: %q", reply.Content)
	}
	// The flagged reply must not be cached.
	if entries := eng.LLMCache().Stats().Entries; entries != 0 {
		t.Errorf("[BUG-20260611 M6] hallucinated claim reply was cached (entries=%d)", entries)
	}
}

// sseStreamProvider overrides the mock's simplified Stream with a proper
// OpenAI-format SSE payload so the streamed content survives parsing.
type sseStreamProvider struct {
	*mockllm.LLMProvider
	content string
}

func (p *sseStreamProvider) Stream(_ context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	payload, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]string{"content": p.content}}},
	})
	sse := "data: " + string(payload) + "\n\ndata: [DONE]\n\n"
	return llm.NewStream(strings.NewReader(sse), llm.StreamOpenAIFormat), nil
}

// Streaming path (runtime stream → finalizeRuntimeStreamResult) regression
// lock: the guard existed here first and must keep working after the hoist
// into the shared helper.
func TestBug20260611_CronClaimGuard_StreamRuntime(t *testing.T) {
	eng := newEngineWithProvider(t, &sseStreamProvider{
		LLMProvider: mockllm.NewLLMProvider("test"),
		content:     hallucinatedCronClaim,
	})

	ch, err := eng.ProcessStream(context.Background(), &adapter.Message{
		ID: "msg-claim-stream", Platform: adapter.PlatformAPI,
		UserID: "user-1", ChatID: "chat-1",
		Content: "每天上午10点采集微博热搜",
	})
	if err != nil {
		t.Fatalf("ProcessStream failed: %v", err)
	}
	var full strings.Builder
	for chunk := range ch {
		full.WriteString(chunk.Content)
	}
	if !strings.Contains(full.String(), "系统校验") {
		t.Errorf("[BUG-20260611 M6] stream path missed the hallucinated creation claim, content: %q", full.String())
	}
}

// Unit lock for the shared helper used by every finalize path (including the
// currently dormant pipeStream / pipeStreamWithTools legacy stream paths).
func TestGuardCronCreationClaim_Helper(t *testing.T) {
	ctx := context.Background()
	guided := &adapter.Message{Metadata: map[string]string{"cron_guidance_active": "true"}}

	if notice, flagged := guardCronCreationClaim(ctx, guided, hallucinatedCronClaim, false, "s1", "m1", 0); !flagged || !strings.Contains(notice, "系统校验") {
		t.Errorf("claim without tool call under active guidance must be flagged, got (%q, %v)", notice, flagged)
	}
	if _, flagged := guardCronCreationClaim(ctx, guided, hallucinatedCronClaim, true, "s1", "m1", 1); flagged {
		t.Error("claim WITH a cron_task call must not be flagged")
	}
	if _, flagged := guardCronCreationClaim(ctx, guided, "好的，我可以帮你创建定时任务，请确认时间。", false, "s1", "m1", 0); flagged {
		t.Error("an offer to create must not be flagged")
	}
	// Guidance not active this turn (cross-turn restatement) → never flagged.
	unguided := &adapter.Message{Metadata: map[string]string{}}
	if _, flagged := guardCronCreationClaim(ctx, unguided, hallucinatedCronClaim, false, "s1", "m1", 0); flagged {
		t.Error("claim outside a guidance-active turn must not be flagged")
	}
	if _, flagged := guardCronCreationClaim(ctx, nil, hallucinatedCronClaim, false, "s1", "m1", 0); flagged {
		t.Error("nil message must not be flagged")
	}
}

// Cross-turn restatement: when the current turn carries no cron-creation
// intent (guidance not active), a reply that truthfully restates a
// prior-turn creation must NOT be flagged.
func TestBug20260611_CronClaimGuard_CrossTurnRestatementNotFlagged(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{
			Content: "已为你创建定时任务，它会每天 10 点执行。",
			Usage:   hexagon.Usage{TotalTokens: 10},
		}, nil
	})
	eng := newEngineWithProvider(t, provider)

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-restate", Platform: adapter.PlatformAPI,
		UserID: "user-1", ChatID: "chat-1",
		Content: "刚才那个建好了没有呀",
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if strings.Contains(reply.Content, "系统校验") {
		t.Errorf("[BUG-20260611 Low] truthful cross-turn restatement was flagged: %q", reply.Content)
	}
}
