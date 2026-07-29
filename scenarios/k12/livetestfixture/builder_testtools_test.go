//go:build testtools

package livetestfixture

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type fixtureHarness struct {
	db      *sql.DB
	agents  *router.SQLiteStore
	records *k12storage.Store
	calls   *BoundaryCounter
}

func newFixtureHarness(t *testing.T) *fixtureHarness {
	t.Helper()
	db, err := sql.Open(
		"sqlite",
		filepath.Join(t.TempDir(), "fixture.db")+"?_pragma=foreign_keys(1)",
	)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	agents := router.NewSQLiteStore(db)
	if err := agents.Init(context.Background()); err != nil {
		t.Fatalf("init agents: %v", err)
	}
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		t.Fatalf("assemble K12 records: %v", err)
	}
	return &fixtureHarness{
		db:      db,
		agents:  agents,
		records: k12storage.NewStore(db, registry.Records),
		calls:   &BoundaryCounter{},
	}
}

func (h *fixtureHarness) builder(now time.Time) *Builder {
	return &Builder{
		Agents:  h.agents,
		Records: h.records,
		Calls:   h.calls,
		Now:     func() time.Time { return now },
	}
}

func fixtureOptions(runID string, lease time.Duration) CreateOptions {
	return CreateOptions{
		RunID:     runID,
		LearnerID: "live-child",
		Lease:     lease,
		AgentConfig: router.AgentConfig{
			DisplayName: "LIVE K12 fixture",
			Provider:    "hexclaw-gpt",
			Model:       "gpt-5.6-sol",
			Metadata: map[string]string{
				"k12.child_name": "LIVE child",
				"k12.grade_term": "grade5_2",
			},
		},
	}
}

func findAgent(
	t *testing.T,
	store router.Store,
	name string,
) (router.AgentConfig, bool) {
	t.Helper()
	agents, _, err := store.LoadAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if agent.Name == name {
			return agent, true
		}
	}
	return router.AgentConfig{}, false
}

func TestBuilderCreatesExactDurableStatesWithoutExternalCallsAndCleansIdempotently(
	t *testing.T,
) {
	h := newFixtureHarness(t)
	builder := h.builder(time.Unix(1_800_000_000, 0))
	manifest, err := builder.Create(
		context.Background(),
		fixtureOptions("strict-run-1", 10*time.Minute),
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if manifest.Ownership == "" || manifest.AgentName == "" ||
		manifest.RetryableDispatchID == "" ||
		manifest.OutcomeUnknownDispatchID == "" ||
		manifest.RetryableDispatchID == manifest.OutcomeUnknownDispatchID {
		t.Fatalf("invalid opaque manifest: %+v", manifest.Redacted())
	}

	retryable, err := h.records.GetImageTaskDispatch(
		context.Background(), manifest.AgentName, manifest.RetryableDispatchID,
	)
	if err != nil {
		t.Fatalf("retryable dispatch: %v", err)
	}
	if retryable.Status != k12.ImageTaskStatusFailed || !retryable.RetrySafe {
		t.Fatalf("retryable state drift: %+v", retryable)
	}
	unknown, err := h.records.GetImageTaskDispatch(
		context.Background(), manifest.AgentName, manifest.OutcomeUnknownDispatchID,
	)
	if err != nil {
		t.Fatalf("outcome-unknown dispatch: %v", err)
	}
	if unknown.Status != k12.ImageTaskStatusFailed || unknown.RetrySafe ||
		unknown.FailureKind != FixtureFailureOutcomeUnknown {
		t.Fatalf("outcome-unknown state drift: %+v", unknown)
	}
	if got := h.calls.Snapshot(); got != (BoundarySnapshot{}) {
		t.Fatalf("fixture creation reached an external boundary: %+v", got)
	}
	agent, ok := findAgent(t, h.agents, manifest.AgentName)
	if !ok || agent.Metadata[metadataOwnership] != manifest.Ownership {
		t.Fatalf("fixture ownership missing: agent=%+v found=%v", agent, ok)
	}
	if got := agent.Metadata["scenario"]; got != "k12-tutor" {
		t.Fatalf("fixture K12 scenario identity = %q, want k12-tutor", got)
	}

	cleaned, err := builder.Cleanup(context.Background(), manifest)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if cleaned.Cleaned != 2 || cleaned.Remaining != 0 {
		t.Fatalf("cleanup receipt: %+v", cleaned)
	}
	if _, err := h.records.GetImageTaskDispatch(
		context.Background(), manifest.AgentName, manifest.RetryableDispatchID,
	); !errors.Is(err, k12storage.ErrImageTaskNotFound) {
		t.Fatalf("retryable fixture survived cleanup: %v", err)
	}
	if _, err := h.records.GetImageTaskDispatch(
		context.Background(), manifest.AgentName, manifest.OutcomeUnknownDispatchID,
	); !errors.Is(err, k12storage.ErrImageTaskNotFound) {
		t.Fatalf("outcome-unknown fixture survived cleanup: %v", err)
	}
	repeated, err := builder.Cleanup(context.Background(), manifest)
	if err != nil || repeated.Cleaned != 0 || repeated.Remaining != 0 {
		t.Fatalf("idempotent cleanup: receipt=%+v err=%v", repeated, err)
	}
}

func TestBuilderRunCleansAfterGateFailureAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate func(context.Context, Manifest) error
	}{
		{
			name: "gate failure",
			gate: func(context.Context, Manifest) error {
				return errors.New("strict gate failed")
			},
		},
		{
			name: "canceled context",
			gate: func(ctx context.Context, _ Manifest) error {
				cancelable, cancel := context.WithCancel(ctx)
				cancel()
				return cancelable.Err()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFixtureHarness(t)
			builder := h.builder(time.Unix(1_800_000_100, 0))
			manifest, receipt, err := builder.Run(
				context.Background(),
				fixtureOptions("strict-run-"+tc.name, 10*time.Minute),
				tc.gate,
			)
			if err == nil {
				t.Fatal("Run must retain the gate failure")
			}
			if receipt.Cleaned != 2 || receipt.Remaining != 0 {
				t.Fatalf("Run cleanup receipt: %+v", receipt)
			}
			if _, ok := findAgent(t, h.agents, manifest.AgentName); ok {
				t.Fatalf("fixture agent %q survived failed gate", manifest.AgentName)
			}
			if got := h.calls.Snapshot(); got != (BoundarySnapshot{}) {
				t.Fatalf("failed gate reached an external boundary: %+v", got)
			}
		})
	}
}

func TestBuilderScavengesOnlyExpiredFixtureOwnership(t *testing.T) {
	h := newFixtureHarness(t)
	oldBuilder := h.builder(time.Unix(1_800_001_000, 0))
	oldManifest, err := oldBuilder.Create(
		context.Background(),
		fixtureOptions("expired-run", time.Second),
	)
	if err != nil {
		t.Fatalf("create expired fixture: %v", err)
	}
	unrelated := router.AgentConfig{
		Name: "ordinary-agent", DisplayName: "ordinary",
		Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
		Metadata: map[string]string{"owner": "user"},
	}
	if err := h.agents.SaveAgent(context.Background(), &unrelated); err != nil {
		t.Fatalf("save unrelated agent: %v", err)
	}

	newBuilder := h.builder(time.Unix(1_800_001_002, 0))
	newManifest, err := newBuilder.Create(
		context.Background(),
		fixtureOptions("new-run", 10*time.Minute),
	)
	if err != nil {
		t.Fatalf("Create after orphan: %v", err)
	}
	t.Cleanup(func() {
		_, _ = newBuilder.Cleanup(context.Background(), newManifest)
		_ = h.agents.DeleteAgent(context.Background(), unrelated.Name)
	})
	if _, ok := findAgent(t, h.agents, oldManifest.AgentName); ok {
		t.Fatalf("expired ownership %q was not scavenged", oldManifest.Ownership)
	}
	if _, ok := findAgent(t, h.agents, unrelated.Name); !ok {
		t.Fatal("scavenger deleted an unrelated agent")
	}
	if _, ok := findAgent(t, h.agents, newManifest.AgentName); !ok {
		t.Fatal("scavenger deleted the current fixture")
	}
}
