package usecase

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type dd036PreparedCrashRecognizer struct {
	mu sync.Mutex

	store       *k12storage.Store
	jobID       string
	wantChildID string

	recognizeCalls int
	providerSends  int
	sawCASClaim    bool
}

func (r *dd036PreparedCrashRecognizer) bindRecoveredStore(
	store *k12storage.Store,
	jobID string,
	wantChildID string,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = store
	r.jobID = jobID
	r.wantChildID = wantChildID
}

func (r *dd036PreparedCrashRecognizer) Recognize(
	ctx context.Context,
	image []byte,
) ([]RecognizedQuestion, error) {
	r.mu.Lock()
	r.recognizeCalls++
	store := r.store
	jobID := r.jobID
	wantChildID := r.wantChildID
	r.mu.Unlock()

	result, err := k12.ExecuteRecognitionPhysicalCall(
		ctx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: image,
		},
		func(context.Context) (string, error) {
			r.mu.Lock()
			r.providerSends++
			r.mu.Unlock()

			children, listErr := store.ListModelPhysicalInvocations(
				context.Background(),
				"mingming",
				jobID,
			)
			if listErr != nil {
				return "", listErr
			}
			if len(children) != 1 ||
				children[0].PhysicalInvocationID != wantChildID ||
				children[0].Status != k12.ModelInvocationSent {
				return "", fmt.Errorf(
					"provider boundary escaped without the recovered child CAS claim: %+v",
					children,
				)
			}
			r.mu.Lock()
			r.sawCASClaim = true
			r.mu.Unlock()
			return `{"questions":[{"question":"1+1="}]}`, nil
		},
	)
	if err != nil {
		return nil, err
	}
	if result.InvocationID != wantChildID {
		return nil, fmt.Errorf(
			"recovery used physical child %q, want %q",
			result.InvocationID,
			wantChildID,
		)
	}
	return []RecognizedQuestion{{
		Question:    "1+1=",
		Subject:     "数学",
		AnswerState: AnswerStateBlank,
	}}, nil
}

func (r *dd036PreparedCrashRecognizer) stats() (
	recognizeCalls int,
	providerSends int,
	sawCASClaim bool,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recognizeCalls, r.providerSends, r.sawCASClaim
}

func openDD036PreparedCrashStore(
	t *testing.T,
	path string,
) (*sql.DB, *k12storage.Store, scenario.ConstraintProvider) {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open file SQLite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		_ = db.Close()
		t.Fatalf("migrate file SQLite: %v", err)
	}
	registry := scenario.NewRegistry()
	constraint := k12.NewCurriculumStub()
	if err := registry.Assemble(k12.Pack(constraint)); err != nil {
		_ = db.Close()
		t.Fatalf("assemble K12 registry: %v", err)
	}
	return db, k12storage.NewStore(db, registry.Records), constraint
}

func dd036PreparedCrashDeps(
	store *k12storage.Store,
	constraint scenario.ConstraintProvider,
	recognizer Recognizer,
) Deps {
	return Deps{
		Recognizer:            recognizer,
		Records:               store,
		Constraint:            constraint,
		GradingBudgetSnapshot: orchestratorTestBudget(),
		Now:                   func() int64 { return time.Now().Unix() },
	}
}

func dd036PreparedCrashSnapshot() k12.GradingModelSnapshot {
	return k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision",
		TimeoutMS:                120_000,
		RecognizingRequestPolicy: k12.ApprovedRecognizingRequestPolicy(),
	}
}

func dd036PreparedCrashSnapshotResolver(
	k12.GradingModelSnapshot,
) (k12.GradingModelSnapshot, error) {
	return dd036PreparedCrashSnapshot(), nil
}

// REG-DD-036-P0: a crash after the immutable physical child is prepared but
// before its CAS send claim is the one safe recovery window. Restart must reuse
// the exact sent parent and prepared child, claim that child once, and issue
// exactly one provider request. It must never create a second paid-attempt
// identity merely because process memory was lost.
func TestDD036PreparedPhysicalChildCrashRecoveryResumesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "prepared-crash.db")
	runDir := filepath.Join(root, "grading-runs")
	recognizer := &dd036PreparedCrashRecognizer{}

	db1, store1, constraint1 := openDD036PreparedCrashStore(t, dbPath)
	db1Open := true
	defer func() {
		if db1Open {
			_ = db1.Close()
		}
	}()
	if _, err := db1.Exec(
		`INSERT INTO agents(name, metadata) VALUES(?, ?)`,
		"mingming",
		`{"k12.grade_term":"五年级上"}`,
	); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	first := NewGradingOrchestrator(
		dd036PreparedCrashDeps(store1, constraint1, recognizer),
		dd036PreparedCrashSnapshotResolver,
		WithGradingRunDir(runDir),
	)
	job, created, err := first.StartPhotoGradingJob(
		ctx,
		StartPhotoGradingInput{
			Photo: PhotoGradeRequest{
				AgentName:     "mingming",
				Grade:         "五年级上",
				SourceSession: "dd036-prepared-crash",
				Image:         []byte("dd036 prepared crash image"),
			},
			SourceKind: "desktop",
			SourceKey:  "dd036-prepared-crash",
		},
	)
	if err != nil || !created {
		t.Fatalf("start grading job created=%v err=%v", created, err)
	}
	jobID := job.Record.RecordID
	run := first.lookup(jobID)
	if run == nil {
		t.Fatal("first process did not retain the grading runtime")
	}
	if job, err = first.advanceOK(ctx, run, jobID, ""); err != nil {
		t.Fatalf("advance queued: %v", err)
	}
	if job, err = first.advanceOK(ctx, run, jobID, "image:prepared-crash"); err != nil {
		t.Fatalf("advance normalizing: %v", err)
	}
	if job.Record.Status != k12.GradingStageRecognizing {
		t.Fatalf("fixture stage=%s, want recognizing", job.Record.Status)
	}

	policy := k12.ApprovedRecognizingRequestPolicy()
	parentBefore, err := first.beginModelInvocationWithPolicy(
		ctx,
		job,
		k12.GradingStageRecognizing,
		recognizingInvocationDigest(
			run.req.Image,
			job.Fields.ModelSnapshot,
			policy,
		),
		policy,
	)
	if err != nil {
		t.Fatalf("prepare sent parent: %v", err)
	}
	call := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: run.req.Image,
	}
	childDigest, err := recognizingPhysicalInvocationDigest(parentBefore, call)
	if err != nil {
		t.Fatalf("compute physical child digest: %v", err)
	}
	childBefore, childCreated, err := store1.PrepareModelPhysicalInvocation(
		ctx,
		k12.ModelPhysicalInvocation{
			PhysicalInvocationID: stableRecognitionPhysicalInvocationID(
				parentBefore.InvocationID,
				call.Unit,
			),
			ParentInvocationID:    parentBefore.InvocationID,
			AgentName:             parentBefore.AgentName,
			JobID:                 parentBefore.JobID,
			Stage:                 parentBefore.Stage,
			PhysicalUnit:          call.Unit,
			RequestDigest:         childDigest,
			RouteSnapshot:         parentBefore.RouteSnapshot,
			RequestPolicySnapshot: parentBefore.RequestPolicySnapshot,
			Attempt:               1,
			CreatedAt:             time.Now().Unix(),
		},
	)
	if err != nil || !childCreated {
		t.Fatalf(
			"prepare physical child created=%v child=%+v err=%v",
			childCreated,
			childBefore,
			err,
		)
	}
	if parentBefore.Status != k12.ModelInvocationSent ||
		parentBefore.Attempt != 1 ||
		childBefore.Status != k12.ModelInvocationPrepared ||
		childBefore.Attempt != 1 {
		t.Fatalf(
			"crash fixture parent=%+v child=%+v",
			parentBefore,
			childBefore,
		)
	}
	if recognizeCalls, providerSends, _ := recognizer.stats(); recognizeCalls != 0 || providerSends != 0 {
		t.Fatalf(
			"fixture crossed provider boundary before crash: recognize=%d sends=%d",
			recognizeCalls,
			providerSends,
		)
	}

	shutdown1Ctx, cancelShutdown1 := context.WithTimeout(ctx, 5*time.Second)
	if err := first.Shutdown(shutdown1Ctx); err != nil {
		cancelShutdown1()
		t.Fatalf("shutdown first orchestrator: %v", err)
	}
	cancelShutdown1()
	if err := db1.Close(); err != nil {
		t.Fatalf("close first SQLite process: %v", err)
	}
	db1Open = false

	db2, store2, constraint2 := openDD036PreparedCrashStore(t, dbPath)
	defer func() { _ = db2.Close() }()
	recognizer.bindRecoveredStore(
		store2,
		jobID,
		childBefore.PhysicalInvocationID,
	)
	second := NewGradingOrchestrator(
		dd036PreparedCrashDeps(store2, constraint2, recognizer),
		dd036PreparedCrashSnapshotResolver,
		WithGradingRunDir(runDir),
	)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		defer cancel()
		if err := second.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown recovered orchestrator: %v", err)
		}
	}()

	recovered, err := second.RecoverGradingJobs(ctx, []string{"mingming"})
	if err != nil {
		t.Fatalf("recover grading jobs: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered jobs=%d, want 1", recovered)
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, 5*time.Second)
	if err := second.WaitForIdle(waitCtx); err != nil {
		cancelWait()
		t.Fatalf("wait recovered worker: %v", err)
	}
	cancelWait()

	jobAfter, err := second.deps.GetGradingJob(
		ctx,
		"mingming",
		jobID,
	)
	if err != nil {
		t.Fatalf("read recovered job: %v", err)
	}
	parentsAfter, err := store2.ListModelInvocations(
		ctx,
		"mingming",
		jobID,
	)
	if err != nil {
		t.Fatalf("list recovered parents: %v", err)
	}
	childrenAfter, err := store2.ListModelPhysicalInvocations(
		ctx,
		"mingming",
		jobID,
	)
	if err != nil {
		t.Fatalf("list recovered children: %v", err)
	}
	if len(parentsAfter) != 1 || len(childrenAfter) != 1 {
		t.Fatalf(
			"restart created a second model identity: parents=%+v children=%+v",
			parentsAfter,
			childrenAfter,
		)
	}
	parentAfter := parentsAfter[0]
	childAfter := childrenAfter[0]
	if parentAfter.InvocationID != parentBefore.InvocationID ||
		parentAfter.RequestDigest != parentBefore.RequestDigest ||
		parentAfter.Attempt != parentBefore.Attempt ||
		childAfter.PhysicalInvocationID != childBefore.PhysicalInvocationID ||
		childAfter.ParentInvocationID != parentBefore.InvocationID ||
		childAfter.RequestDigest != childBefore.RequestDigest ||
		childAfter.Attempt != 1 {
		t.Fatalf(
			"restart changed immutable parent/child identity: before=%+v/%+v after=%+v/%+v",
			parentBefore,
			childBefore,
			parentAfter,
			childAfter,
		)
	}
	recognizeCalls, providerSends, sawCASClaim := recognizer.stats()
	if recognizeCalls != 1 ||
		providerSends != 1 ||
		!sawCASClaim ||
		parentAfter.Status != k12.ModelInvocationSucceeded ||
		childAfter.Status != k12.ModelInvocationSucceeded ||
		childAfter.ResultDigest == "" ||
		jobAfter.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf(
			"prepared-crash recovery did not resume exactly once: job=%s parent=%s child=%s child_result=%q recognize=%d sends=%d saw_cas=%v",
			jobAfter.Record.Status,
			parentAfter.Status,
			childAfter.Status,
			childAfter.ResultDigest,
			recognizeCalls,
			providerSends,
			sawCASClaim,
		)
	}
}
