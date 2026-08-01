package usecase

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type dd036RecoveryEvidenceProviderSpy struct {
	recognizerCalls int
	providerCalls   int
}

func (s *dd036RecoveryEvidenceProviderSpy) Recognize(
	ctx context.Context,
	image []byte,
) ([]RecognizedQuestion, error) {
	s.recognizerCalls++
	_, err := k12.ExecuteRecognitionPhysicalCall(
		ctx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: image,
		},
		func(context.Context) (string, error) {
			s.providerCalls++
			return `{"questions":[{"question":"1+1="}]}`, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("unexpected successful recognition Provider call")
}

type dd036RecoveryEvidenceFixture struct {
	ctx        context.Context
	deps       Deps
	runDir     string
	first      *GradingOrchestrator
	run        *gradingRun
	job        GradingJobView
	parent     k12.ModelInvocation
	provider   *dd036RecoveryEvidenceProviderSpy
	currentNow *int64
}

func newDD036RecoveryEvidenceFixture(
	t *testing.T,
	sourceKey string,
	startedAt int64,
) *dd036RecoveryEvidenceFixture {
	t.Helper()

	ctx := context.Background()
	provider := &dd036RecoveryEvidenceProviderSpy{}
	deps := recoveryDeps(t, provider, nil, &photoAnnotatorFake{})
	currentNow := startedAt
	deps.Now = func() int64 { return currentNow }
	runDir := t.TempDir()
	first := newRecoverableOrchestrator(t, deps, runDir)
	snapshot := dd036SendBoundarySnapshot()
	job, created, err := first.StartPhotoGradingJob(
		ctx,
		StartPhotoGradingInput{
			Photo:         orchestratorPhotoRequest(),
			SourceKind:    "desktop",
			SourceKey:     sourceKey,
			ModelSnapshot: snapshot,
		},
	)
	if err != nil || !created {
		t.Fatalf("start recovery fixture created=%v err=%v", created, err)
	}
	run := first.lookup(job.Record.RecordID)
	if run == nil {
		t.Fatal("recovery fixture did not retain its grading runtime")
	}
	if job, err = first.advanceOK(
		ctx,
		run,
		job.Record.RecordID,
		"",
	); err != nil {
		t.Fatalf("advance queued: %v", err)
	}
	if job, err = first.advanceOK(
		ctx,
		run,
		job.Record.RecordID,
		"image:dd036-recovery-evidence",
	); err != nil {
		t.Fatalf("advance normalizing: %v", err)
	}
	if job.Record.Status != k12.GradingStageRecognizing {
		t.Fatalf(
			"recovery fixture stage=%s, want recognizing",
			job.Record.Status,
		)
	}
	policy := k12.ApprovedRecognizingRequestPolicy()
	parent, err := first.beginModelInvocationWithPolicy(
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
	if err != nil || parent.Status != k12.ModelInvocationSent {
		t.Fatalf(
			"prepare recovery parent status=%s err=%v",
			parent.Status,
			err,
		)
	}
	return &dd036RecoveryEvidenceFixture{
		ctx:        ctx,
		deps:       deps,
		runDir:     runDir,
		first:      first,
		run:        run,
		job:        job,
		parent:     parent,
		provider:   provider,
		currentNow: &currentNow,
	}
}

func (f *dd036RecoveryEvidenceFixture) prepareExactWholePageChild(
	t *testing.T,
) k12.ModelPhysicalInvocation {
	t.Helper()

	call := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: f.run.req.Image,
	}
	requestDigest, err := recognizingPhysicalInvocationDigest(f.parent, call)
	if err != nil {
		t.Fatalf("compute exact whole-page child digest: %v", err)
	}
	child, created, err := f.deps.Records.PrepareModelPhysicalInvocation(
		f.ctx,
		k12.ModelPhysicalInvocation{
			PhysicalInvocationID: stableRecognitionPhysicalInvocationID(
				f.parent.InvocationID,
				call.Unit,
			),
			ParentInvocationID:    f.parent.InvocationID,
			AgentName:             f.parent.AgentName,
			JobID:                 f.parent.JobID,
			Stage:                 f.parent.Stage,
			PhysicalUnit:          call.Unit,
			RequestDigest:         requestDigest,
			RouteSnapshot:         f.parent.RouteSnapshot,
			RequestPolicySnapshot: f.parent.RequestPolicySnapshot,
			Attempt:               1,
			CreatedAt:             f.deps.now(),
			UpdatedAt:             f.deps.now(),
		},
	)
	if err != nil || !created ||
		child.Status != k12.ModelInvocationPrepared {
		t.Fatalf(
			"prepare exact whole-page child created=%v child=%+v err=%v",
			created,
			child,
			err,
		)
	}
	return child
}

func (f *dd036RecoveryEvidenceFixture) recoverRunRecognize(
	t *testing.T,
) error {
	t.Helper()

	restarted := newRecoverableOrchestrator(t, f.deps, f.runDir)
	recoveredRun, err := restarted.ensureRun(f.ctx, f.job.Record.RecordID)
	if err != nil {
		t.Fatalf("recover grading runtime: %v", err)
	}
	_, runErr := restarted.runRecognize(
		f.ctx,
		recoveredRun,
		f.job.Record.RecordID,
	)
	return runErr
}

func (f *dd036RecoveryEvidenceFixture) assertDefiniteFailure(
	t *testing.T,
	wantChildStatus k12.ModelInvocationStatus,
	wantChildFailureKind string,
	wantChildren int,
	wantJobStatus string,
	wantRetryable bool,
	runErr error,
) {
	t.Helper()

	storedJob, jobErr := f.deps.GetGradingJob(
		f.ctx,
		f.parent.AgentName,
		f.job.Record.RecordID,
	)
	storedParent, parentErr := f.deps.Records.GetModelInvocation(
		f.ctx,
		f.parent.AgentName,
		f.parent.InvocationID,
	)
	children, childrenErr := f.deps.Records.ListModelPhysicalInvocations(
		f.ctx,
		f.parent.AgentName,
		f.job.Record.RecordID,
	)
	if jobErr != nil || parentErr != nil || childrenErr != nil {
		t.Fatalf(
			"reload recovery evidence job=%v parent=%v children=%v",
			jobErr,
			parentErr,
			childrenErr,
		)
	}
	if f.provider.providerCalls != 0 {
		t.Errorf(
			"conclusive recovery evidence sent %d Provider requests; want 0 (Recognizer entries=%d)",
			f.provider.providerCalls,
			f.provider.recognizerCalls,
		)
	}
	if storedParent.Status != k12.ModelInvocationFailed {
		t.Errorf(
			"conclusive recovery parent=%s failure_kind=%q, want failed and never outcome_unknown; run_err=%v",
			storedParent.Status,
			storedParent.FailureKind,
			runErr,
		)
	}
	if storedParent.FailureKind == "" ||
		storedParent.FailureKind == "provider_outcome_unknown" {
		t.Errorf(
			"conclusive recovery parent failure_kind=%q, want a definite non-unknown reason",
			storedParent.FailureKind,
		)
	}
	if storedJob.Record.Status != wantJobStatus ||
		storedJob.Fields.Retryable != wantRetryable {
		t.Errorf(
			"conclusive recovery Job=%s retryable=%v failure_kind=%q, want %s/%v and never outcome_unknown; run_err=%v",
			storedJob.Record.Status,
			storedJob.Fields.Retryable,
			storedJob.Fields.FailureKind,
			wantJobStatus,
			wantRetryable,
			runErr,
		)
	}
	if storedJob.Fields.FailedStage != k12.GradingStageRecognizing ||
		storedJob.Fields.AttemptCount != 1 {
		t.Errorf(
			"conclusive recovery failed_stage=%s attempt_count=%d, want recognizing/1",
			storedJob.Fields.FailedStage,
			storedJob.Fields.AttemptCount,
		)
	}
	if storedJob.Fields.FailureKind == "" ||
		storedJob.Fields.FailureKind == "provider_outcome_unknown" ||
		storedJob.Fields.FailureKind ==
			"invocation_reconciliation_required" {
		t.Errorf(
			"conclusive recovery Job failure_kind=%q, want a definite non-unknown reason",
			storedJob.Fields.FailureKind,
		)
	}
	if len(children) != wantChildren {
		t.Errorf(
			"physical child count=%d want=%d children=%+v",
			len(children),
			wantChildren,
			children,
		)
		return
	}
	if wantChildren == 1 &&
		(children[0].Status != wantChildStatus ||
			children[0].FailureKind != wantChildFailureKind) {
		t.Errorf(
			"physical child status=%s failure_kind=%q, want %s/%q",
			children[0].Status,
			children[0].FailureKind,
			wantChildStatus,
			wantChildFailureKind,
		)
	}
}

// REG-DD-036-P0: a sent stage parent is not sufficient evidence that a
// Provider request escaped. On restart, zero children, an exact prepared child
// whose durable deadline expired, and a definitively failed child are all
// conclusive no-resend states. Recovery must perform zero Provider calls and
// converge parent/Job from the child evidence instead of manufacturing
// outcome_unknown.
func TestDD036RunRecognizeRecoveryClassifiesConclusivePhysicalEvidence(
	t *testing.T,
) {
	t.Run("parent_sent_with_zero_children", func(t *testing.T) {
		fixture := newDD036RecoveryEvidenceFixture(
			t,
			"dd036-recovery-zero-children",
			time.Now().Unix(),
		)

		runErr := fixture.recoverRunRecognize(t)

		fixture.assertDefiniteFailure(
			t,
			"",
			"",
			0,
			k12.GradingStageFailedRetryable,
			true,
			runErr,
		)
	})

	t.Run("expired_exact_prepared_child", func(t *testing.T) {
		// Seed the durable work at a historical clock while its stage budget
		// is still live, then restart after that absolute deadline.
		fixture := newDD036RecoveryEvidenceFixture(
			t,
			"dd036-recovery-expired-prepared-child",
			time.Now().Add(-10*time.Minute).Unix(),
		)
		fixture.prepareExactWholePageChild(t)
		*fixture.currentNow = time.Now().Unix()

		runErr := fixture.recoverRunRecognize(t)

		fixture.assertDefiniteFailure(
			t,
			k12.ModelInvocationFailed,
			"provider_request_not_sent",
			1,
			k12.GradingStageFailedRetryable,
			true,
			runErr,
		)
	})

	t.Run("definitively_failed_child", func(t *testing.T) {
		fixture := newDD036RecoveryEvidenceFixture(
			t,
			"dd036-recovery-definite-failed-child",
			time.Now().Unix(),
		)
		child := fixture.prepareExactWholePageChild(t)
		child, claimed, err := fixture.deps.Records.
			ClaimModelPhysicalInvocationSent(
				fixture.ctx,
				fixture.parent.AgentName,
				child.PhysicalInvocationID,
			)
		if err != nil || !claimed ||
			child.Status != k12.ModelInvocationSent {
			t.Fatalf(
				"claim failed-child fixture claimed=%v child=%+v err=%v",
				claimed,
				child,
				err,
			)
		}
		const failureKind = "provider_response_http_400"
		child, err = fixture.deps.Records.MarkModelPhysicalInvocationFailed(
			fixture.ctx,
			fixture.parent.AgentName,
			child.PhysicalInvocationID,
			failureKind,
		)
		if err != nil || child.Status != k12.ModelInvocationFailed {
			t.Fatalf(
				"mark definite failed child child=%+v err=%v",
				child,
				err,
			)
		}

		runErr := fixture.recoverRunRecognize(t)

		fixture.assertDefiniteFailure(
			t,
			k12.ModelInvocationFailed,
			failureKind,
			1,
			k12.GradingStageFailedTerminal,
			false,
			runErr,
		)
	})
}
