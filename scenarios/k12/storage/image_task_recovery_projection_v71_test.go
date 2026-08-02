package k12storage_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

func TestImageTaskRecoveryProjectionIsAgentSessionScopedAndStableAfterReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "image-task-recovery.db")
	open := func() (*sql.DB, *k12storage.Store) {
		db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		registry := scenario.NewRegistry()
		if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		return db, k12storage.NewStore(db, registry.Records)
	}

	db, store := open()
	for _, agent := range []string{"mingming", "lele"} {
		if _, err := db.Exec(`INSERT INTO agents(name) VALUES(?)`, agent); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(dispatchID, invocationID, agent, session, message string, generation int) {
		t.Helper()
		dispatch := testImageTaskDispatch()
		dispatch.DispatchID = dispatchID
		dispatch.ClassificationInvocationID = invocationID
		dispatch.AgentName = agent
		dispatch.LearnerID = agent
		dispatch.SourceSessionID = session
		dispatch.SourceRef = message
		dispatch.SourceAssetRefs = []string{"asset://" + agent + "/" + dispatchID + ".png"}
		dispatch.SourceDigest = "sha256:" + dispatchID
		dispatch.IdempotencyKey = agent + ":" + message
		dispatch.RequestDigest = "sha256:request:" + dispatchID
		dispatch.AttemptGeneration = generation
		invocation := k12.ImageTaskInvocation{
			InvocationID: invocationID, AgentName: agent, DispatchID: dispatchID,
			Operation:     k12.ImageTaskOperationClassification,
			OperationKey:  "dispatch:" + dispatchID + ":classification",
			RequestDigest: "sha256:invocation:" + dispatchID,
			RouteSnapshot: testImageRoute(), Status: k12.ImageTaskInvocationPrepared,
			Attempt: generation, CreatedAt: 100, UpdatedAt: 100,
		}
		if _, _, err := store.PrepareImageTaskDispatch(context.Background(), dispatch, invocation); err != nil {
			t.Fatal(err)
		}
	}
	seed("dispatch-a", "invocation-a", "mingming", "session-a", "message-a", 2)
	seed("dispatch-other-session", "invocation-b", "mingming", "session-b", "message-b", 1)
	seed("dispatch-other-agent", "invocation-c", "lele", "session-a", "message-c", 1)

	assertProjection := func(got []k12.ImageTaskDispatch) {
		t.Helper()
		if len(got) != 1 {
			t.Fatalf("agent/session projection length=%d, want 1: %+v", len(got), got)
		}
		if got[0].DispatchID != "dispatch-a" || got[0].SourceRef != "message-a" ||
			got[0].SourceSessionID != "session-a" || got[0].AttemptGeneration != 2 {
			t.Fatalf("stable dispatch/source identity drift: %+v", got[0])
		}
	}

	got, err := store.ListImageTaskDispatchesForSession(
		context.Background(), "mingming", "session-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	assertProjection(got)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedDB, reopenedStore := open()
	defer reopenedDB.Close()
	got, err = reopenedStore.ListImageTaskDispatchesForSession(
		context.Background(), "mingming", "session-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	assertProjection(got)
	var dispatches, invocations int
	if err := reopenedDB.QueryRow(`SELECT COUNT(*) FROM k12_image_task_dispatches`).Scan(&dispatches); err != nil {
		t.Fatal(err)
	}
	if err := reopenedDB.QueryRow(`SELECT COUNT(*) FROM k12_image_task_invocations`).Scan(&invocations); err != nil {
		t.Fatal(err)
	}
	if dispatches != 3 || invocations != 3 {
		t.Fatalf("read projection caused side effects: dispatches=%d invocations=%d", dispatches, invocations)
	}
}
