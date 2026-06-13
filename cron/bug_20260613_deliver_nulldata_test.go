package cron

// BUG-20260613 (review F3): a script that succeeds with no payload outputs
// {"status":"success","data":null}. deliverableContent saw Data==nil and fell
// back to Stdout — delivering the raw contract JSON line to the user instead of
// nothing. A no-payload success must produce no deliverable content (the
// scheduler then skips the empty notification), while a genuine agent reply in
// Stdout must still be delivered.

import "testing"

func TestBug20260613_NullDataScriptDeliversNothing(t *testing.T) {
	// Script success with explicit null data → raw contract JSON in Stdout.
	got := deliverableContent(&RunResult{
		Data:   nil,
		Stdout: `{"status": "success", "data": null}`,
	})
	if got != "" {
		t.Errorf("a no-payload script success must deliver nothing, got %q", got)
	}
}

func TestBug20260613_AgentTextStillDelivered(t *testing.T) {
	// Agent reply: Data nil, Stdout is human text (not contract JSON) → delivered.
	got := deliverableContent(&RunResult{Stdout: "今日要点：A、B、C"})
	if got != "今日要点：A、B、C" {
		t.Errorf("agent text must still be delivered, got %q", got)
	}
}

func TestBug20260613_ScriptWithRealDataStillDelivered(t *testing.T) {
	got := deliverableContent(&RunResult{
		Data:   "热搜：A、B、C",
		Stdout: `{"status":"success","data":"热搜：A、B、C"}`,
	})
	if got != "热搜：A、B、C" {
		t.Errorf("script with real data must deliver the payload, got %q", got)
	}
}

// A success line with extra free-text printed before it is NOT pure contract
// JSON, so it must still be delivered (conservative: only suppress the bare
// contract line).
func TestBug20260613_NonContractStdoutNotSuppressed(t *testing.T) {
	got := deliverableContent(&RunResult{Stdout: "采集到 3 条结果"})
	if got != "采集到 3 条结果" {
		t.Errorf("non-contract stdout must be delivered, got %q", got)
	}
}
