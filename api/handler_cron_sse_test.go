package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	_ "modernc.org/sqlite"
)

// stubLLMResp 构造一个能通过 cron Compiler 全部校验链的 LLM 返回。
const stubScriptResp = `{"runtime":"python3","script":"import json\nprint(json.dumps({\"status\":\"success\"}))\n","deps":[],"timeout_s":60}`

type stubProvider struct{ content string }

func (s *stubProvider) Name() string                             { return "stub" }
func (s *stubProvider) Models() []llm.ModelInfo                  { return nil }
func (s *stubProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }
func (s *stubProvider) Stream(_ context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	return nil, nil
}
func (s *stubProvider) Complete(_ context.Context, _ llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return &llm.CompletionResponse{Content: s.content}, nil
}

// 构造一个含 SSE 路径所需依赖的 Server。
func newSSETestServer(t *testing.T) (*Server, *cron.Scheduler) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	provider := &stubProvider{content: stubScriptResp}
	compiler := cron.NewLLMCompilerStatic(provider, "")
	scriptExec := cron.NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir())
	scheduler := cron.NewScheduler(db, compiler, scriptExec)
	if err := scheduler.Init(t.Context()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetCronScheduler(scheduler)
	return srv, scheduler
}

// TestHandleAddCronJobSSE_EmitsAllStages 验证 SSE 完整流：
//
//	progress(analyzing) → progress(calling_llm) → progress(validating)
//	→ progress(persisting) → done
func TestHandleAddCronJobSSE_EmitsAllStages(t *testing.T) {
	srv, _ := newSSETestServer(t)

	body := `{"name":"t","schedule":"@daily","prompt":"x","user_id":"u1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron/jobs", strings.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleAddCronJob(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type: %q", ct)
	}

	body2 := w.Body.String()
	wantStages := []string{
		`"stage":"analyzing"`,
		`"stage":"calling_llm"`,
		`"stage":"validating"`,
		`"stage":"persisting"`,
	}
	for _, want := range wantStages {
		if !strings.Contains(body2, want) {
			t.Errorf("缺 progress 阶段：%s\n---\n%s", want, body2)
		}
	}
	if !strings.Contains(body2, "event: done") {
		t.Errorf("缺 done 事件\n%s", body2)
	}
	if !strings.Contains(body2, `"spec_preview"`) {
		t.Errorf("done 缺 spec_preview\n%s", body2)
	}
}

// TestHandleAddCronJobSSE_EmitsErrorOnCompileFailure
func TestHandleAddCronJobSSE_EmitsErrorOnCompileFailure(t *testing.T) {
	srv, _ := newSSETestServer(t)
	// 替换 compiler 让它失败：直接换个 scheduler 不太方便，这里通过另起一份 stub
	badProvider := &stubProvider{content: `not json`}
	badCompiler := cron.NewLLMCompilerStatic(badProvider, "")
	db, _ := sql.Open("sqlite", ":memory:")
	defer db.Close()
	scriptExec := cron.NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir())
	scheduler := cron.NewScheduler(db, badCompiler, scriptExec)
	_ = scheduler.Init(t.Context())
	srv.SetCronScheduler(scheduler)

	body := `{"name":"t","schedule":"@daily","prompt":"x","user_id":"u1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron/jobs", strings.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleAddCronJob(w, req)
	body2 := w.Body.String()
	if !strings.Contains(body2, "event: error") {
		t.Errorf("应发 error 事件\n%s", body2)
	}
	if !strings.Contains(body2, "event: done") {
		// done 不应出现
	} else {
		t.Errorf("不应有 done 事件\n%s", body2)
	}
}

// TestHandleAddCronJob_JSONFallback Accept != event-stream 走旧 JSON 路径
func TestHandleAddCronJob_JSONFallback(t *testing.T) {
	srv, _ := newSSETestServer(t)
	body := `{"name":"t","schedule":"@daily","prompt":"x","user_id":"u1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleAddCronJob(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type: %q", ct)
	}
	if strings.Contains(w.Body.String(), "event:") {
		t.Errorf("JSON 路径不应含 SSE 事件\n%s", w.Body.String())
	}
}
