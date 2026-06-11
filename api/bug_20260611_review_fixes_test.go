// Review-fix regression tests (2026-06-11 batch):
//   - C1: stripBetween infinite loop on " 0x" payloads (handler hang at 100% CPU)
//   - M1: transport-stage compile errors ("LLM 编译失败: invalid x-api-key")
//     hijacked by the generic {"invalid" → "参数不合法"} pattern
//   - M2: agent frequency-guard rejection mapped to HTTP 500 + stage-less SSE
//   - M4: action=update deletes the original job and fails to recreate it
//     when the new draft is rejected (e.g. by the agent 1h guard)
//   - L8: humanizeError fallback clipped by bytes, splitting UTF-8 runes
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hexagon-codes/hexclaw/cron"
)

// C1: sanitizeErrorMessage must terminate when the message contains " 0x" —
// the prefix begins with the suffix " ", so the old loop never shrank the
// input and spun forever.
func TestBug20260611_SanitizeStripBetweenTerminates(t *testing.T) {
	done := make(chan string, 1)
	go func() { done <- sanitizeErrorMessage("upstream crashed at 0xc000123456 while flushing") }()
	select {
	case got := <-done:
		if strings.Contains(got, "0xc000123456") {
			t.Errorf("memory address not stripped: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sanitizeErrorMessage did not terminate (stripBetween infinite loop)")
	}
}

// M1: transport-stage compile failures (invalid api key, 401, network) must
// not be flattened into the parameter-blame text.
func TestBug20260611_TransportCompileErrorNotParamBlame(t *testing.T) {
	for _, in := range []string{
		"LLM 编译失败: invalid x-api-key",
		"编译失败: LLM 编译失败: openai api error: 401 Unauthorized",
	} {
		got := humanizeError(errors.New(in))
		if got == "参数不合法" {
			t.Fatalf("transport compile error flattened to parameter-blame text: %q ← %q", got, in)
		}
		if !strings.Contains(got, "模型") {
			t.Errorf("message should point at the upstream model call, got %q ← %q", got, in)
		}
	}
}

// L8: the fallback clip must cut on rune boundaries, never mid-UTF-8.
func TestBug20260611_HumanizeClipIsRuneSafe(t *testing.T) {
	long := strings.Repeat("错", 300) // 900 bytes, no known pattern matches
	out := humanizeError(errors.New(long))
	if !utf8.ValidString(out) {
		t.Fatalf("clip split a UTF-8 rune: %q", out)
	}
	if len([]rune(out)) > 220 {
		t.Errorf("long error not truncated: %d runes", len([]rune(out)))
	}
}

// M2: the agent-mode frequency guard is a user-correctable rejection — it
// must map to HTTP 400 (not 500) with a non-internal error code.
func TestBug20260611_AgentFrequencyGuardReturns400(t *testing.T) {
	srv, _ := newSSETestServer(t)

	body, _ := json.Marshal(CronJobRequest{
		Action: "create", UserID: "u1",
		Draft: &CronJobDraft{Name: "高频总结", Schedule: "@every 5m", Prompt: "每5分钟总结一次日志"},
	})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/cronjob", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleCronjobUnified(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("agent frequency guard should map to 400, got %d body=%s", w.Code, w.Body.String())
	}
	var e APIError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Code == CodeInternalError || e.Code == "" {
		t.Errorf("guard rejection should carry a user-correctable code, got %q", e.Code)
	}
}

// M2 (SSE side): the error event must carry a pipeline stage.
func TestBug20260611_AgentFrequencyGuardSSEHasStage(t *testing.T) {
	srv, _ := newSSETestServer(t)

	body := `{"name":"高频总结","schedule":"@every 5m","prompt":"每5分钟总结一次日志","user_id":"u1"}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/cron/jobs", strings.NewReader(body))
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	srv.handleAddCronJob(w, r)

	out := w.Body.String()
	if !strings.Contains(out, "event: error") {
		t.Fatalf("guard rejection should emit an SSE error event:\n%s", out)
	}
	if strings.Contains(out, `"stage":""`) || !strings.Contains(out, `"stage":"validating"`) {
		t.Errorf("SSE error event should carry stage \"validating\":\n%s", out)
	}
}

// M4: action=update is delete-then-create; if the create side is rejected
// (here by the agent 1h guard), the original job must survive.
func TestBug20260611_UpdatePreservesJobOnCreateFailure(t *testing.T) {
	srv, scheduler := newSSETestServer(t)
	ctx := context.Background()

	orig := &cron.Job{
		Name: "采集", Type: cron.JobTypeCron, Schedule: "@every 5m", UserID: "u1",
		Status: cron.StatusActive, SourcePrompt: "每5分钟采集热搜",
		Spec: &cron.JobSpec{Runtime: "python3", Script: "print(1)", TimeoutSec: 60},
	}
	if err := scheduler.AddJob(ctx, orig); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	body, _ := json.Marshal(CronJobRequest{
		Action: "update", UserID: "u1", JobID: orig.ID,
		Draft: &CronJobDraft{Name: "高频总结", Schedule: "@every 5m", Prompt: "每5分钟总结一次日志"},
	})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/cronjob", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleCronjobUnified(w, r)

	if w.Code == http.StatusOK {
		t.Fatalf("update with a guard-rejected draft should fail, got 200 body=%s", w.Body.String())
	}
	got, ok := scheduler.GetJob(ctx, orig.ID)
	if !ok {
		t.Fatal("original job must survive a failed update (delete-then-create lost it)")
	}
	if got.Spec == nil || got.Spec.Script != "print(1)" {
		t.Errorf("restored job should keep the original spec: %+v", got.Spec)
	}
}
