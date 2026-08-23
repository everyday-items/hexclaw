package api

import (
	"context"
	"database/sql"
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
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	"github.com/hexagon-codes/hexclaw/webhook"
)

type bug20260801003WebhookJobOwnerHarness struct {
	server    *Server
	db        *sql.DB
	scheduler *cron.Scheduler
}

func newBUG20260801003WebhookJobOwnerHarness(t *testing.T) *bug20260801003WebhookJobOwnerHarness {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "webhook-owner.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	scheduler := cron.NewScheduler(
		db,
		nil,
		cron.NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()),
	)
	if err := scheduler.Init(ctx); err != nil {
		t.Fatalf("init scheduler: %v", err)
	}
	mgr := webhook.NewManager(db)
	if err := mgr.Init(ctx); err != nil {
		t.Fatalf("init webhook manager: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetWebhookManager(mgr)
	srv.SetCronScheduler(scheduler)
	return &bug20260801003WebhookJobOwnerHarness{server: srv, db: db, scheduler: scheduler}
}

func (h *bug20260801003WebhookJobOwnerHarness) addJob(t *testing.T, id, owner string) {
	t.Helper()
	err := h.scheduler.AddJob(context.Background(), &cron.Job{
		ID:       id,
		Name:     id,
		Type:     cron.JobTypeCron,
		Schedule: "@daily",
		UserID:   owner,
		Spec: &cron.JobSpec{
			Runtime: cron.RuntimeStarlark,
			Script:  `emit("ok")`,
		},
	})
	if err != nil {
		t.Fatalf("add job %s: %v", id, err)
	}
}

func (h *bug20260801003WebhookJobOwnerHarness) register(
	t *testing.T,
	authenticatedOwner, name, jobID, bodyOwner string,
) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(
		`{"name":%q,"type":"generic","job_id":%q,"user_id":%q}`,
		name,
		jobID,
		bodyOwner,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/webhooks?user_id=forged-query-owner",
		strings.NewReader(body),
	)
	req = req.WithContext(skill.WithAuthenticatedUser(req.Context(), authenticatedOwner))
	rec := httptest.NewRecorder()
	h.server.handleRegisterWebhook(rec, req)
	return rec
}

func (h *bug20260801003WebhookJobOwnerHarness) assertWebhookAbsent(t *testing.T, name string) {
	t.Helper()
	if _, ok := h.server.webhookMgr.Get(name); ok {
		t.Fatalf("webhook %q exists in memory", name)
	}
	reloaded := webhook.NewManager(h.db)
	if err := reloaded.Init(context.Background()); err != nil {
		t.Fatalf("reload webhook manager: %v", err)
	}
	if _, ok := reloaded.Get(name); ok {
		t.Fatalf("webhook %q was persisted", name)
	}
}

func TestBUG20260801003WebhookCreateHidesCrossOwnerAndMissingJobs(t *testing.T) {
	h := newBUG20260801003WebhookJobOwnerHarness(t)
	h.addJob(t, "victim-job", "owner-b")

	tests := []struct {
		name  string
		jobID string
	}{
		{name: "cross-owner", jobID: "victim-job"},
		{name: "missing", jobID: "missing-job"},
	}
	responses := make(map[string]string, len(tests))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hookName := "owner-gate-" + tc.name
			rec := h.register(t, "owner-a", hookName, tc.jobID, "owner-b")
			if rec.Code != http.StatusNotFound {
				t.Errorf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
			}
			responses[tc.name] = strings.TrimSpace(rec.Body.String())
			h.assertWebhookAbsent(t, hookName)
		})
	}
	if responses["cross-owner"] != responses["missing"] {
		t.Fatalf("cross-owner response leaks job existence: cross=%s missing=%s",
			responses["cross-owner"], responses["missing"])
	}
}

func TestBUG20260801003WebhookCreateAcceptsSameOwnerJob(t *testing.T) {
	h := newBUG20260801003WebhookJobOwnerHarness(t)
	h.addJob(t, "owned-job", "owner-a")

	rec := h.register(t, "owner-a", "owned-hook", "owned-job", "forged-body-owner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	created, ok := h.server.webhookMgr.Get("owned-hook")
	if !ok {
		t.Fatal("same-owner webhook was not created")
	}
	if created.UserID != "owner-a" || created.JobID != "owned-job" {
		t.Fatalf("created webhook owner/job=%q/%q", created.UserID, created.JobID)
	}

	reloaded := webhook.NewManager(h.db)
	if err := reloaded.Init(context.Background()); err != nil {
		t.Fatalf("reload webhook manager: %v", err)
	}
	persisted, ok := reloaded.Get("owned-hook")
	if !ok || persisted.UserID != "owner-a" || persisted.JobID != "owned-job" {
		t.Fatalf("persisted webhook=%+v exists=%v", persisted, ok)
	}
}

func TestBUG20260801003WebhookCreateRequiresSchedulerForBoundJob(t *testing.T) {
	h := newBUG20260801003WebhookJobOwnerHarness(t)
	h.addJob(t, "owned-job", "owner-a")
	h.server.SetCronScheduler(nil)

	rec := h.register(t, "owner-a", "scheduler-missing-hook", "owned-job", "owner-a")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	h.assertWebhookAbsent(t, "scheduler-missing-hook")
}
