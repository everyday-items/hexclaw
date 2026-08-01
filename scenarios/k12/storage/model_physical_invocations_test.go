package k12storage_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

func preparePhysicalInvocationParent(
	t *testing.T,
	store *k12storage.Store,
	jobID string,
) k12.ModelInvocation {
	t.Helper()
	policy := k12.ApprovedRecognizingRequestPolicy()
	parent, created, err := store.PrepareModelInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID:  "parent-" + jobID,
			AgentName:     "mingming",
			JobID:         jobID,
			Stage:         k12.GradingStageRecognizing,
			RequestDigest: "sha256:parent-" + jobID,
			RouteSnapshot: k12.GradingModelSnapshot{
				Provider:                 "hexclaw-gpt",
				Model:                    k12.RecognizingPolicyModel,
				Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
				RecognizingRequestPolicy: policy,
			},
			RequestPolicySnapshot: policy,
			Attempt:               1,
			CreatedAt:             100,
		},
	)
	if err != nil || !created {
		t.Fatalf("prepare parent: created=%v err=%v", created, err)
	}
	parent, err = store.MarkModelInvocationSent(
		context.Background(),
		parent.AgentName,
		parent.InvocationID,
		"",
	)
	if err != nil {
		t.Fatalf("mark parent sent: %v", err)
	}
	return parent
}

func newPhysicalInvocation(
	parent k12.ModelInvocation,
	id string,
	unit k12.RecognitionPhysicalUnit,
) k12.ModelPhysicalInvocation {
	return k12.ModelPhysicalInvocation{
		PhysicalInvocationID:  id,
		ParentInvocationID:    parent.InvocationID,
		AgentName:             parent.AgentName,
		JobID:                 parent.JobID,
		Stage:                 parent.Stage,
		PhysicalUnit:          unit,
		RequestDigest:         "sha256:" + string(unit),
		RouteSnapshot:         parent.RouteSnapshot,
		RequestPolicySnapshot: parent.RequestPolicySnapshot,
		Attempt:               1,
		CreatedAt:             200,
	}
}

func openPhysicalLedgerFileStore(
	t *testing.T,
	path string,
) (*k12storage.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open physical ledger db: %v", err)
	}
	db.SetMaxOpenConns(1)
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		_ = db.Close()
		t.Fatalf("assemble K12 registry: %v", err)
	}
	return k12storage.NewStore(db, registry.Records), db
}

// REG-DD-036: one actual recognition POST is one immutable child receipt. The
// (parent,physical_unit) identity accepts only an exact replay.
func TestModelPhysicalInvocationPrepareExactReplayAndMutationConflict(t *testing.T) {
	store, _ := setup(t)
	job := newGradingJobRecord(t, "mingming", "physical-exact-replay")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	want := newPhysicalInvocation(
		parent,
		"physical-whole-page",
		k12.RecognitionPhysicalUnitWholePage,
	)

	first, created, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		want,
	)
	if err != nil || !created {
		t.Fatalf("first prepare: created=%v invocation=%+v err=%v", created, first, err)
	}
	if first.Status != k12.ModelInvocationPrepared || first.Attempt != 1 {
		t.Fatalf("prepared physical invocation has wrong state: %+v", first)
	}
	replay, created, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		want,
	)
	if err != nil || created || replay != first {
		t.Fatalf("exact replay: created=%v replay=%+v err=%v", created, replay, err)
	}

	mutations := map[string]func(*k12.ModelPhysicalInvocation){
		"physical id": func(v *k12.ModelPhysicalInvocation) {
			v.PhysicalInvocationID = "physical-rewritten"
		},
		"job": func(v *k12.ModelPhysicalInvocation) {
			v.JobID = "other-job"
		},
		"stage": func(v *k12.ModelPhysicalInvocation) {
			v.Stage = k12.GradingStageAssessing
		},
		"request digest": func(v *k12.ModelPhysicalInvocation) {
			v.RequestDigest = "sha256:rewritten"
		},
		"route": func(v *k12.ModelPhysicalInvocation) {
			v.RouteSnapshot = k12.GradingModelSnapshot{
				Provider: "other",
				Model:    "other",
				Route:    "other/other",
			}
		},
		"request policy": func(v *k12.ModelPhysicalInvocation) {
			v.RequestPolicySnapshot = k12.ModelRequestPolicySnapshot{}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := want
			mutate(&changed)
			if _, _, err := store.PrepareModelPhysicalInvocation(
				context.Background(),
				changed,
			); !errors.Is(err, k12storage.ErrModelPhysicalInvocationConflict) {
				t.Fatalf("mutation must fail with immutable identity conflict, got %v", err)
			}
		})
	}
}

func TestModelPhysicalInvocationRejectsInvalidUnitAttemptAndParentScope(t *testing.T) {
	store, _ := setup(t)
	job := newGradingJobRecord(t, "mingming", "physical-validation")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	base := newPhysicalInvocation(
		parent,
		"physical-validation",
		k12.RecognitionPhysicalUnitWholePage,
	)

	invalidUnit := base
	invalidUnit.PhysicalUnit = "prompt_inferred"
	if _, _, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		invalidUnit,
	); err == nil {
		t.Fatal("unknown physical unit must be rejected")
	}
	invalidAttempt := base
	invalidAttempt.Attempt = 2
	if _, _, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		invalidAttempt,
	); err == nil {
		t.Fatal("physical invocation attempt must be exactly one")
	}
	wrongOwner := base
	wrongOwner.AgentName = "lele"
	if _, _, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		wrongOwner,
	); !errors.Is(err, k12storage.ErrModelPhysicalInvocationConflict) {
		t.Fatalf("parent-child owner mismatch must fail closed, got %v", err)
	}
}

func TestModelPhysicalInvocationPrepareRequiresSentParent(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()

	for _, target := range []k12.ModelInvocationStatus{
		k12.ModelInvocationPrepared,
		k12.ModelInvocationSucceeded,
		k12.ModelInvocationFailed,
		k12.ModelInvocationOutcomeUnknown,
		k12.ModelInvocationReconciled,
	} {
		t.Run(string(target), func(t *testing.T) {
			job := newGradingJobRecord(
				t,
				"mingming",
				"physical-parent-"+string(target),
			)
			if _, err := store.Put(ctx, job); err != nil {
				t.Fatal(err)
			}
			policy := k12.ApprovedRecognizingRequestPolicy()
			parent, _, err := store.PrepareModelInvocation(
				ctx,
				k12.ModelInvocation{
					InvocationID:  "parent-" + job.RecordID,
					AgentName:     "mingming",
					JobID:         job.RecordID,
					Stage:         k12.GradingStageRecognizing,
					RequestDigest: "sha256:parent-" + job.RecordID,
					RouteSnapshot: k12.GradingModelSnapshot{
						Provider:                 "hexclaw-gpt",
						Model:                    k12.RecognizingPolicyModel,
						Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
						RecognizingRequestPolicy: policy,
					},
					RequestPolicySnapshot: policy,
					Attempt:               1,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if target != k12.ModelInvocationPrepared {
				parent, err = store.MarkModelInvocationSent(
					ctx,
					parent.AgentName,
					parent.InvocationID,
					"",
				)
				if err != nil {
					t.Fatal(err)
				}
			}
			switch target {
			case k12.ModelInvocationSucceeded:
				_, err = store.MarkModelInvocationSucceeded(
					ctx,
					parent.AgentName,
					parent.InvocationID,
					"sha256:parent-result",
					"",
				)
			case k12.ModelInvocationFailed:
				_, err = store.MarkModelInvocationFailed(
					ctx,
					parent.AgentName,
					parent.InvocationID,
					"provider_failed",
				)
			case k12.ModelInvocationOutcomeUnknown:
				_, err = store.MarkModelInvocationOutcomeUnknown(
					ctx,
					parent.AgentName,
					parent.InvocationID,
					"transport_ambiguous",
				)
			case k12.ModelInvocationReconciled:
				if _, err = store.MarkModelInvocationOutcomeUnknown(
					ctx,
					parent.AgentName,
					parent.InvocationID,
					"transport_ambiguous",
				); err == nil {
					_, err = store.ReconcileModelInvocationNotExecuted(
						ctx,
						parent.AgentName,
						parent.InvocationID,
					)
				}
			}
			if err != nil {
				t.Fatalf("move parent to %s: %v", target, err)
			}
			if _, _, err := store.PrepareModelPhysicalInvocation(
				ctx,
				newPhysicalInvocation(
					parent,
					"physical-parent-"+string(target),
					k12.RecognitionPhysicalUnitWholePage,
				),
			); !errors.Is(err, records.ErrIllegalTransition) {
				t.Fatalf(
					"parent status %s must reject new physical child, got %v",
					target,
					err,
				)
			}
		})
	}
}

func TestModelPhysicalInvocationConcurrentClaimSendsExactlyOnce(t *testing.T) {
	store, _ := setup(t)
	job := newGradingJobRecord(t, "mingming", "physical-claim")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	prepared, _, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		newPhysicalInvocation(
			parent,
			"physical-claim",
			k12.RecognitionPhysicalUnitWholePage,
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 100
	var claimed atomic.Int32
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			invocation, won, claimErr := store.ClaimModelPhysicalInvocationSent(
				context.Background(),
				"mingming",
				prepared.PhysicalInvocationID,
			)
			if claimErr != nil {
				errs <- claimErr
				return
			}
			if invocation.Status != k12.ModelInvocationSent {
				errs <- fmt.Errorf("claimed invocation status=%s", invocation.Status)
				return
			}
			if won {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if claimed.Load() != 1 {
		t.Fatalf("physical send claims=%d, want exactly 1", claimed.Load())
	}
}

func TestModelPhysicalInvocationClaimRequiresParentStillSent(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	job := newGradingJobRecord(t, "mingming", "physical-parent-terminal-before-claim")
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	child, _, err := store.PrepareModelPhysicalInvocation(
		ctx,
		newPhysicalInvocation(
			parent,
			"physical-parent-terminal-before-claim",
			k12.RecognitionPhysicalUnitWholePage,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkModelInvocationFailed(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		"parent_closed_before_child_send",
	); err != nil {
		t.Fatalf("close parent before child claim: %v", err)
	}

	got, claimed, err := store.ClaimModelPhysicalInvocationSent(
		ctx,
		child.AgentName,
		child.PhysicalInvocationID,
	)
	if !errors.Is(err, records.ErrIllegalTransition) || claimed {
		t.Fatalf(
			"terminal parent reauthorized child POST: claimed=%v invocation=%+v err=%v",
			claimed,
			got,
			err,
		)
	}
	stored, getErr := store.GetModelPhysicalInvocation(
		ctx,
		child.AgentName,
		child.PhysicalInvocationID,
	)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.Status != k12.ModelInvocationPrepared {
		t.Fatalf("rejected child claim changed status to %s", stored.Status)
	}
}

func TestModelPhysicalInvocationTransitionsUseCASAndExactTerminalReplay(t *testing.T) {
	store, _ := setup(t)
	job := newGradingJobRecord(t, "mingming", "physical-cas")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	prepared, _, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		newPhysicalInvocation(
			parent,
			"physical-cas",
			k12.RecognitionPhysicalUnitWholePage,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
		`{"result":"premature"}`,
		"upstream-1",
	); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("prepared -> succeeded must be rejected, got %v", err)
	}

	sent, err := store.MarkModelPhysicalInvocationSent(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
	)
	if err != nil || sent.Status != k12.ModelInvocationSent {
		t.Fatalf("mark sent: invocation=%+v err=%v", sent, err)
	}
	replayedSent, err := store.MarkModelPhysicalInvocationSent(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
	)
	if err != nil || replayedSent != sent {
		t.Fatalf("sent replay: invocation=%+v err=%v", replayedSent, err)
	}

	succeeded, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
		`{"result":"ok"}`,
		"upstream-1",
	)
	if err != nil || succeeded.Status != k12.ModelInvocationSucceeded {
		t.Fatalf("mark succeeded: invocation=%+v err=%v", succeeded, err)
	}
	replayedSuccess, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
		`{"result":"ok"}`,
		"upstream-1",
	)
	if err != nil || replayedSuccess != succeeded {
		t.Fatalf("success replay: invocation=%+v err=%v", replayedSuccess, err)
	}
	if _, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
		`{"result":"changed"}`,
		"upstream-1",
	); !errors.Is(err, k12storage.ErrModelPhysicalInvocationConflict) {
		t.Fatalf("changed terminal result must fail immutable replay, got %v", err)
	}
	if _, err := store.MarkModelPhysicalInvocationFailed(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
		"provider_error",
	); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("succeeded -> failed must be rejected, got %v", err)
	}
}

func TestModelPhysicalInvocationFailedAndUnknownTransitionsAreDurable(t *testing.T) {
	store, _ := setup(t)

	tests := []struct {
		name string
		mark func(string) (k12.ModelPhysicalInvocation, error)
		want k12.ModelInvocationStatus
	}{
		{
			name: "failed",
			mark: func(id string) (k12.ModelPhysicalInvocation, error) {
				return store.MarkModelPhysicalInvocationFailed(
					context.Background(),
					"mingming",
					id,
					"provider_rejected",
				)
			},
			want: k12.ModelInvocationFailed,
		},
		{
			name: "outcome_unknown",
			mark: func(id string) (k12.ModelPhysicalInvocation, error) {
				return store.MarkModelPhysicalInvocationOutcomeUnknown(
					context.Background(),
					"mingming",
					id,
					"transport_ambiguous",
				)
			},
			want: k12.ModelInvocationOutcomeUnknown,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := newGradingJobRecord(
				t,
				"mingming",
				"physical-terminal-"+test.name,
			)
			if _, err := store.Put(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			parent := preparePhysicalInvocationParent(
				t,
				store,
				job.RecordID,
			)
			prepared, _, err := store.PrepareModelPhysicalInvocation(
				context.Background(),
				newPhysicalInvocation(
					parent,
					"physical-"+test.name,
					k12.RecognitionPhysicalUnitWholePage,
				),
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.MarkModelPhysicalInvocationSent(
				context.Background(),
				"mingming",
				prepared.PhysicalInvocationID,
			); err != nil {
				t.Fatal(err)
			}
			terminal, err := test.mark(prepared.PhysicalInvocationID)
			if err != nil || terminal.Status != test.want || terminal.FailureKind == "" {
				t.Fatalf("terminal invocation=%+v err=%v", terminal, err)
			}
			got, err := store.GetModelPhysicalInvocation(
				context.Background(),
				"mingming",
				prepared.PhysicalInvocationID,
			)
			if err != nil || got != terminal {
				t.Fatalf("durable terminal invocation=%+v err=%v", got, err)
			}
		})
	}
}

func TestModelPhysicalInvocationGetAndListAreOwnerAndJobScoped(t *testing.T) {
	store, _ := setup(t)
	job := newGradingJobRecord(t, "mingming", "physical-list")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	whole, _, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		newPhysicalInvocation(
			parent,
			"physical-list-whole",
			k12.RecognitionPhysicalUnitWholePage,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimModelPhysicalInvocationSent(
		context.Background(),
		parent.AgentName,
		whole.PhysicalInvocationID,
	); err != nil || !claimed {
		t.Fatalf("claim whole: claimed=%v err=%v", claimed, err)
	}
	const wholeContent = "not-json"
	if _, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		context.Background(),
		parent.AgentName,
		whole.PhysicalInvocationID,
		wholeContent,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.AuthorizeRecognitionFallback(
		context.Background(),
		parent.AgentName,
		parent.InvocationID,
		whole.PhysicalInvocationID,
		wholeContent,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		newPhysicalInvocation(
			parent,
			"physical-list-segment",
			k12.RecognitionPhysicalUnitSegment1,
		),
	); err != nil {
		t.Fatal(err)
	}

	if _, err := store.GetModelPhysicalInvocation(
		context.Background(),
		"lele",
		"physical-list-whole",
	); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("cross-owner get must be not found, got %v", err)
	}
	list, err := store.ListModelPhysicalInvocations(
		context.Background(),
		"mingming",
		job.RecordID,
	)
	if err != nil || len(list) != 2 {
		t.Fatalf("list physical invocations=%+v err=%v", list, err)
	}
	for _, invocation := range list {
		if invocation.AgentName != "mingming" ||
			invocation.JobID != job.RecordID ||
			invocation.ParentInvocationID != parent.InvocationID {
			t.Fatalf("list leaked a different scope: %+v", invocation)
		}
	}
}

func TestModelPhysicalInvocationSurvivesSQLiteCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "physical-ledger.db")
	store, db := openPhysicalLedgerFileStore(t, path)
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("migrate file db: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO agents(name) VALUES(?)`,
		"mingming",
	); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", "physical-close-reopen")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	want := newPhysicalInvocation(
		parent,
		"physical-close-reopen",
		k12.RecognitionPhysicalUnitWholePage,
	)
	prepared, _, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		want,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimModelPhysicalInvocationSent(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
	); err != nil || !claimed {
		t.Fatalf("claim before close: claimed=%v err=%v", claimed, err)
	}
	if _, err := store.MarkModelPhysicalInvocationOutcomeUnknown(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
		"transport_ambiguous",
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close physical ledger db: %v", err)
	}

	restarted, reopenedDB := openPhysicalLedgerFileStore(t, path)
	defer reopenedDB.Close()
	stored, err := restarted.GetModelPhysicalInvocation(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
	)
	if err != nil ||
		stored.Status != k12.ModelInvocationOutcomeUnknown ||
		stored.FailureKind != "transport_ambiguous" {
		t.Fatalf("after SQLite reopen invocation=%+v err=%v", stored, err)
	}
	replay, created, err := restarted.PrepareModelPhysicalInvocation(
		context.Background(),
		want,
	)
	if err != nil || created ||
		replay.PhysicalInvocationID != prepared.PhysicalInvocationID ||
		replay.Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf(
			"reopen replay created=%v invocation=%+v err=%v",
			created,
			replay,
			err,
		)
	}
	if _, claimed, err := restarted.ClaimModelPhysicalInvocationSent(
		context.Background(),
		"mingming",
		prepared.PhysicalInvocationID,
	); err == nil || claimed {
		t.Fatalf("outcome_unknown child was reauthorized: claimed=%v err=%v", claimed, err)
	}
}

func TestModelPhysicalInvocationSchemaAllowsArchitecturalReconciledState(t *testing.T) {
	store, db := setup(t)
	job := newGradingJobRecord(t, "mingming", "physical-reconciled-schema")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	parent := preparePhysicalInvocationParent(t, store, job.RecordID)
	prepared, _, err := store.PrepareModelPhysicalInvocation(
		context.Background(),
		newPhysicalInvocation(
			parent,
			"physical-reconciled-schema",
			k12.RecognitionPhysicalUnitWholePage,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(
		context.Background(),
		`UPDATE k12_model_physical_invocations SET status='reconciled'
         WHERE physical_invocation_id=?`,
		prepared.PhysicalInvocationID,
	); err != nil {
		t.Fatalf("physical ledger schema rejected architectural reconciled state: %v", err)
	}
}
