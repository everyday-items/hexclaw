package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/skill"
)

type bug20260801003CronOwnerHarness struct {
	server    *Server
	scheduler *cron.Scheduler
	db        *sql.DB
}

type bug20260801003CronJobSnapshot struct {
	Exists       bool
	Owner        string
	Name         string
	Schedule     string
	Status       string
	SourcePrompt string
	TotalJobs    int
	RunCount     int
}

func newBUG20260801003CronOwnerHarness(t *testing.T) *bug20260801003CronOwnerHarness {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cron-owner.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	provider := &stubProvider{content: stubScriptResp}
	scheduler := cron.NewScheduler(
		db,
		cron.NewLLMCompilerStatic(provider, ""),
		cron.NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()),
	)
	if err := scheduler.Init(context.Background()); err != nil {
		t.Fatalf("init scheduler: %v", err)
	}
	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetCronScheduler(scheduler)
	return &bug20260801003CronOwnerHarness{server: srv, scheduler: scheduler, db: db}
}

func (h *bug20260801003CronOwnerHarness) addJob(
	t *testing.T,
	id, owner string,
	status cron.JobStatus,
) {
	t.Helper()
	if err := h.scheduler.AddJob(context.Background(), &cron.Job{
		ID:           id,
		Name:         id,
		Type:         cron.JobTypeCron,
		Schedule:     "@daily",
		UserID:       owner,
		Status:       status,
		SourcePrompt: "original prompt",
		Spec: &cron.JobSpec{
			Runtime: cron.RuntimeStarlark,
			Script:  `emit("ok")`,
		},
	}); err != nil {
		t.Fatalf("add job %s: %v", id, err)
	}
}

func (h *bug20260801003CronOwnerHarness) invoke(
	t *testing.T,
	reqBody CronJobRequest,
) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cronjob?user_id=forged-query-owner",
		bytes.NewReader(body),
	)
	req = req.WithContext(skill.WithAuthenticatedUser(req.Context(), "owner-a"))
	rec := httptest.NewRecorder()
	h.server.handleCronjobUnified(rec, req)
	return rec
}

func (h *bug20260801003CronOwnerHarness) snapshot(t *testing.T, jobID string) bug20260801003CronJobSnapshot {
	t.Helper()
	var snapshot bug20260801003CronJobSnapshot
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM cron_jobs`).Scan(&snapshot.TotalJobs); err != nil {
		t.Fatalf("count cron jobs: %v", err)
	}
	err := h.db.QueryRow(
		`SELECT user_id, name, schedule, status, source_prompt FROM cron_jobs WHERE id = ?`,
		jobID,
	).Scan(&snapshot.Owner, &snapshot.Name, &snapshot.Schedule, &snapshot.Status, &snapshot.SourcePrompt)
	if err == nil {
		snapshot.Exists = true
	} else if err != sql.ErrNoRows {
		t.Fatalf("read cron job snapshot: %v", err)
	}
	if err := h.db.QueryRow(
		`SELECT COUNT(*) FROM cron_job_runs WHERE job_id = ?`,
		jobID,
	).Scan(&snapshot.RunCount); err != nil {
		t.Fatalf("count cron runs: %v", err)
	}
	return snapshot
}

func TestBUG20260801003CronUnifiedUsesAuthenticatedOwnerForEveryAction(t *testing.T) {
	actions := []string{"create", "list", "run", "update", "pause", "resume", "remove"}
	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			switch action {
			case "create":
				testBUG20260801003CronCreateUsesAuthenticatedOwner(t)
			case "list":
				testBUG20260801003CronListUsesAuthenticatedOwner(t)
			default:
				testBUG20260801003CronTargetActionHidesCrossOwner(t, action)
			}
		})
	}
}

func testBUG20260801003CronCreateUsesAuthenticatedOwner(t *testing.T) {
	h := newBUG20260801003CronOwnerHarness(t)
	rec := h.invoke(t, CronJobRequest{
		Action:         "create",
		UserID:         "owner-b",
		IdempotencyKey: "owner-create-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Draft: &CronJobDraft{
			Name: "authenticated owner job", Schedule: "@daily", Prompt: "summarize notes",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response CronJobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if response.Job == nil || response.Job.UserID != "owner-a" {
		t.Fatalf("created job=%+v, want authenticated owner", response.Job)
	}
	ownerBJobs, err := h.scheduler.ListJobs(context.Background(), "owner-b")
	if err != nil {
		t.Fatalf("list forged owner jobs: %v", err)
	}
	if len(ownerBJobs) != 0 {
		t.Fatalf("forged owner received %d created job(s)", len(ownerBJobs))
	}
}

func testBUG20260801003CronListUsesAuthenticatedOwner(t *testing.T) {
	h := newBUG20260801003CronOwnerHarness(t)
	h.addJob(t, "owner-a-job", "owner-a", cron.StatusActive)
	h.addJob(t, "owner-b-job", "owner-b", cron.StatusActive)
	rec := h.invoke(t, CronJobRequest{Action: "list", UserID: "owner-b", IncludePaused: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response CronJobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(response.Jobs) != 1 || response.Jobs[0].ID != "owner-a-job" {
		t.Fatalf("authenticated owner list leaked or omitted jobs: %+v", response.Jobs)
	}
}

func testBUG20260801003CronTargetActionHidesCrossOwner(t *testing.T, action string) {
	invoke := func(t *testing.T, target string) (int, string, bug20260801003CronJobSnapshot, bug20260801003CronJobSnapshot) {
		t.Helper()
		h := newBUG20260801003CronOwnerHarness(t)
		status := cron.StatusActive
		if action == "resume" || action == "run" {
			status = cron.StatusPaused
		}
		h.addJob(t, "owner-b-job", "owner-b", status)
		before := h.snapshot(t, "owner-b-job")
		req := CronJobRequest{
			Action:         action,
			UserID:         "owner-b",
			JobID:          target,
			IdempotencyKey: fmt.Sprintf("owner-%s-%s-%s", action, target, strings.ReplaceAll(t.Name(), "/", "-")),
		}
		if action == "update" {
			req.Draft = &CronJobDraft{Name: "forged update", Schedule: "@daily", Prompt: "replace victim"}
		}
		rec := h.invoke(t, req)
		after := h.snapshot(t, "owner-b-job")
		return rec.Code, strings.TrimSpace(rec.Body.String()), before, after
	}

	crossStatus, crossBody, before, after := invoke(t, "owner-b-job")
	missingStatus, missingBody, _, _ := invoke(t, "missing-job")
	if crossStatus != http.StatusNotFound || missingStatus != http.StatusNotFound {
		t.Errorf("%s cross/missing status=%d/%d, want 404/404; cross=%s missing=%s",
			action, crossStatus, missingStatus, crossBody, missingBody)
	}
	if crossBody != missingBody {
		t.Errorf("%s leaks target existence: cross=%s missing=%s", action, crossBody, missingBody)
	}
	if before != after {
		t.Errorf("%s mutated or executed cross-owner job: before=%+v after=%+v", action, before, after)
	}
}

func decodeBUG20260801003CronAPIError(
	t *testing.T,
	rec *httptest.ResponseRecorder,
) APIError {
	t.Helper()
	var response APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode cron API error: %v; body=%s", err, rec.Body.String())
	}
	return response
}

func TestBUG20260801003CronRunReturnsOpaqueNotFoundCode(t *testing.T) {
	invoke := func(t *testing.T, withCrossOwnerTarget bool) (int, APIError) {
		t.Helper()
		h := newBUG20260801003CronOwnerHarness(t)
		if withCrossOwnerTarget {
			h.addJob(t, "run-target", "owner-b", cron.StatusActive)
		}
		rec := h.invoke(t, CronJobRequest{
			Action:         "run",
			JobID:          "run-target",
			UserID:         "owner-b",
			IdempotencyKey: "run-not-found-" + strings.ReplaceAll(t.Name(), "/", "-"),
		})
		return rec.Code, decodeBUG20260801003CronAPIError(t, rec)
	}

	crossStatus, cross := invoke(t, true)
	missingStatus, missing := invoke(t, false)
	if crossStatus != http.StatusNotFound || missingStatus != http.StatusNotFound {
		t.Fatalf("run cross/missing status=%d/%d, want 404/404", crossStatus, missingStatus)
	}
	if cross.Code != CodeCronJobNotFound || missing.Code != CodeCronJobNotFound {
		t.Fatalf("run cross/missing code=%q/%q, want %q",
			cross.Code, missing.Code, CodeCronJobNotFound)
	}
	if cross.Message != missing.Message || cross.LegacyEr != missing.LegacyEr {
		t.Fatalf("run response leaks target existence: cross=%+v missing=%+v", cross, missing)
	}
}

func TestBUG20260801003CronRunPausedAfterRestartReturnsConflictCode(t *testing.T) {
	h := newBUG20260801003CronOwnerHarness(t)
	h.addJob(t, "paused-run", "owner-a", cron.StatusPaused)

	restarted := cron.NewScheduler(
		h.db,
		nil,
		cron.NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()),
	)
	if err := restarted.Init(context.Background()); err != nil {
		t.Fatalf("restart scheduler: %v", err)
	}
	h.server.SetCronScheduler(restarted)
	rec := h.invoke(t, CronJobRequest{
		Action:         "run",
		JobID:          "paused-run",
		UserID:         "forged-body-owner",
		IdempotencyKey: "run-paused-" + strings.ReplaceAll(t.Name(), "/", "-"),
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("paused run status=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	response := decodeBUG20260801003CronAPIError(t, rec)
	if response.Code != CodeCronJobPaused {
		t.Fatalf("paused run code=%q, want %q; body=%s",
			response.Code, CodeCronJobPaused, rec.Body.String())
	}
}

func TestBUG20260801003CronRunWithoutExecutorReturnsServiceUnavailableCode(t *testing.T) {
	h := newBUG20260801003CronOwnerHarness(t)
	h.addJob(t, "executor-missing-run", "owner-a", cron.StatusActive)

	restarted := cron.NewScheduler(h.db, nil, nil)
	if err := restarted.Init(context.Background()); err != nil {
		t.Fatalf("restart scheduler without executor: %v", err)
	}
	h.server.SetCronScheduler(restarted)
	rec := h.invoke(t, CronJobRequest{
		Action:         "run",
		JobID:          "executor-missing-run",
		UserID:         "forged-body-owner",
		IdempotencyKey: "run-executor-missing-" + strings.ReplaceAll(t.Name(), "/", "-"),
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("executor-missing run status=%d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	response := decodeBUG20260801003CronAPIError(t, rec)
	if response.Code != CodeCronExecutorUnavailable {
		t.Fatalf("executor-missing run code=%q, want %q; body=%s",
			response.Code, CodeCronExecutorUnavailable, rec.Body.String())
	}
}

func TestBUG20260801003CronLifecycleActionsReturnOpaqueNotFoundCode(t *testing.T) {
	for _, action := range []string{"pause", "resume", "remove"} {
		t.Run(action, func(t *testing.T) {
			invoke := func(t *testing.T, withCrossOwnerTarget bool) (int, APIError) {
				t.Helper()
				h := newBUG20260801003CronOwnerHarness(t)
				if withCrossOwnerTarget {
					status := cron.StatusActive
					if action == "resume" {
						status = cron.StatusPaused
					}
					h.addJob(t, "lifecycle-target", "owner-b", status)
				}
				rec := h.invoke(t, CronJobRequest{
					Action:         action,
					JobID:          "lifecycle-target",
					UserID:         "owner-b",
					IdempotencyKey: fmt.Sprintf(
						"%s-not-found-%s", action, strings.ReplaceAll(t.Name(), "/", "-"),
					),
				})
				return rec.Code, decodeBUG20260801003CronAPIError(t, rec)
			}

			crossStatus, cross := invoke(t, true)
			missingStatus, missing := invoke(t, false)
			if crossStatus != http.StatusNotFound || missingStatus != http.StatusNotFound {
				t.Fatalf("%s cross/missing status=%d/%d, want 404/404",
					action, crossStatus, missingStatus)
			}
			if cross.Code != CodeCronJobNotFound || missing.Code != CodeCronJobNotFound {
				t.Fatalf("%s cross/missing code=%q/%q, want %q",
					action, cross.Code, missing.Code, CodeCronJobNotFound)
			}
			if cross.Message != missing.Message || cross.LegacyEr != missing.LegacyEr {
				t.Fatalf("%s response leaks target existence: cross=%+v missing=%+v",
					action, cross, missing)
			}
		})
	}
}
