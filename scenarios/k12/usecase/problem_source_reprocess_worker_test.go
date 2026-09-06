package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type sourceReprocessQueueSpy struct {
	mu sync.Mutex

	job                     k12storage.ProblemSourceReprocessJob
	claimable               bool
	reconciliationJob       k12storage.ProblemSourceReprocessJob
	reconciliationClaimable bool

	heartbeats                      int
	claimCalls                      int
	succeeded                       int
	needsConfirmation               int
	retryable                       int
	released                        int
	outcomeUnknown                  int
	outcomeUnknownFailure           k12storage.ProblemSourceReprocessFailure
	reconciliationClaims            int
	reconciliationHeartbeats        int
	reconciliationSucceeded         int
	reconciliationNeedsConfirmation int
	reconciliationReleased          int
	reconciliationFailure           k12storage.ProblemSourceReprocessFailure
	reconciliationHeartbeatErr      error
	heartbeatObserved               chan struct{}
	heartbeatOnce                   sync.Once
	heartbeatErr                    error
	heartbeatWaitForCancel          bool
	succeededObserved               chan struct{}
	succeededOnce                   sync.Once
}

func (q *sourceReprocessQueueSpy) ClaimProblemSourceReprocessJob(
	_ context.Context,
	workerID string,
	now time.Time,
	leaseDuration time.Duration,
) (k12storage.ProblemSourceReprocessJob, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claimCalls++
	if !q.claimable {
		return k12storage.ProblemSourceReprocessJob{}, false, nil
	}
	q.claimable = false
	q.job.Status = k12storage.ProblemSourceReprocessRunning
	q.job.LeaseOwner = workerID
	q.job.LeaseEpoch++
	q.job.LeaseExpiresAtMilli = now.Add(leaseDuration).UnixMilli()
	q.job.AttemptCount++
	return q.job, true, nil
}

func (q *sourceReprocessQueueSpy) ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
	_ context.Context,
	reconcilerID string,
	now time.Time,
	leaseDuration time.Duration,
) (k12storage.ProblemSourceReprocessJob, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.reconciliationClaimable {
		return k12storage.ProblemSourceReprocessJob{}, false, nil
	}
	q.reconciliationClaimable = false
	q.reconciliationClaims++
	q.reconciliationJob.Status = k12storage.ProblemSourceReprocessOutcomeUnknown
	q.reconciliationJob.ReconciliationOwner = reconcilerID
	q.reconciliationJob.ReconciliationEpoch++
	q.reconciliationJob.ReconciliationExpiresAtMilli = now.Add(leaseDuration).UnixMilli()
	q.reconciliationJob.ReconciliationAttemptCount++
	return q.reconciliationJob, true, nil
}

func (q *sourceReprocessQueueSpy) HeartbeatProblemSourceReprocessJob(
	ctx context.Context,
	lease k12storage.ProblemSourceReprocessLease,
	now time.Time,
	leaseDuration time.Duration,
) (k12storage.ProblemSourceReprocessJob, error) {
	q.mu.Lock()
	if lease != q.job.Lease() {
		q.mu.Unlock()
		return k12storage.ProblemSourceReprocessJob{}, k12storage.ErrProblemSourceReprocessFenced
	}
	if q.heartbeatWaitForCancel {
		q.heartbeatOnce.Do(func() { close(q.heartbeatObserved) })
		q.mu.Unlock()
		<-ctx.Done()
		return k12storage.ProblemSourceReprocessJob{}, ctx.Err()
	}
	if q.heartbeatErr != nil {
		q.heartbeatOnce.Do(func() { close(q.heartbeatObserved) })
		q.mu.Unlock()
		return k12storage.ProblemSourceReprocessJob{}, q.heartbeatErr
	}
	q.heartbeats++
	q.job.LeaseExpiresAtMilli = now.Add(leaseDuration).UnixMilli()
	q.heartbeatOnce.Do(func() { close(q.heartbeatObserved) })
	q.mu.Unlock()
	return q.job, nil
}

func (q *sourceReprocessQueueSpy) HeartbeatProblemSourceReprocessOutcomeUnknownReconciliation(
	_ context.Context,
	lease k12storage.ProblemSourceReprocessReconciliationLease,
	now time.Time,
	leaseDuration time.Duration,
) (k12storage.ProblemSourceReprocessJob, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if lease != q.reconciliationJob.ReconciliationLease() {
		return k12storage.ProblemSourceReprocessJob{}, k12storage.ErrProblemSourceReprocessReconciliationFenced
	}
	if q.reconciliationHeartbeatErr != nil {
		return k12storage.ProblemSourceReprocessJob{}, q.reconciliationHeartbeatErr
	}
	q.reconciliationHeartbeats++
	q.reconciliationJob.ReconciliationExpiresAtMilli = now.Add(leaseDuration).UnixMilli()
	return q.reconciliationJob, nil
}

func (q *sourceReprocessQueueSpy) CompleteProblemSourceReprocessSucceeded(
	_ context.Context,
	lease k12storage.ProblemSourceReprocessLease,
	_ time.Time,
) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if lease != q.job.Lease() {
		return k12storage.ErrProblemSourceReprocessFenced
	}
	q.succeeded++
	if q.succeededObserved != nil {
		q.succeededOnce.Do(func() { close(q.succeededObserved) })
	}
	return nil
}

func (q *sourceReprocessQueueSpy) ReleaseProblemSourceReprocessJob(
	_ context.Context,
	lease k12storage.ProblemSourceReprocessLease,
	_ time.Time,
) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if lease != q.job.Lease() {
		return k12storage.ErrProblemSourceReprocessFenced
	}
	q.released++
	q.job.Status = k12storage.ProblemSourceReprocessQueued
	q.job.LeaseOwner = ""
	q.job.LeaseExpiresAtMilli = 0
	if q.job.AttemptCount > 0 {
		q.job.AttemptCount--
	}
	return nil
}

func (q *sourceReprocessQueueSpy) ReleaseProblemSourceReprocessOutcomeUnknownReconciliation(
	_ context.Context,
	lease k12storage.ProblemSourceReprocessReconciliationLease,
	_ time.Time,
) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if lease != q.reconciliationJob.ReconciliationLease() {
		return k12storage.ErrProblemSourceReprocessReconciliationFenced
	}
	q.reconciliationReleased++
	q.reconciliationJob.ReconciliationOwner = ""
	q.reconciliationJob.ReconciliationExpiresAtMilli = 0
	if q.reconciliationJob.ReconciliationAttemptCount > 0 {
		q.reconciliationJob.ReconciliationAttemptCount--
	}
	return nil
}

func (q *sourceReprocessQueueSpy) CompleteProblemSourceReprocessNeedsConfirmation(
	_ context.Context,
	lease k12storage.ProblemSourceReprocessLease,
	_ k12storage.ProblemSourceReprocessFailure,
	_ time.Time,
) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if lease != q.job.Lease() {
		return k12storage.ErrProblemSourceReprocessFenced
	}
	q.needsConfirmation++
	return nil
}

func (q *sourceReprocessQueueSpy) FailProblemSourceReprocessRetryable(
	_ context.Context,
	lease k12storage.ProblemSourceReprocessLease,
	failure k12storage.ProblemSourceReprocessFailure,
	now time.Time,
) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if lease != q.job.Lease() {
		return k12storage.ErrProblemSourceReprocessFenced
	}
	if !failure.RetryAt.After(now) {
		return errors.New("retry deadline is not after failure time")
	}
	q.retryable++
	return nil
}

func (q *sourceReprocessQueueSpy) MarkProblemSourceReprocessOutcomeUnknown(
	_ context.Context,
	lease k12storage.ProblemSourceReprocessLease,
	failure k12storage.ProblemSourceReprocessFailure,
	_ time.Time,
) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if lease != q.job.Lease() {
		return k12storage.ErrProblemSourceReprocessFenced
	}
	q.outcomeUnknown++
	q.outcomeUnknownFailure = failure
	return nil
}

func (q *sourceReprocessQueueSpy) ResolveProblemSourceReprocessOutcomeUnknown(
	_ context.Context,
	lease k12storage.ProblemSourceReprocessReconciliationLease,
	resolution k12storage.ProblemSourceReprocessOutcomeUnknownResolution,
	failure k12storage.ProblemSourceReprocessFailure,
	_ time.Time,
) (k12storage.ProblemSourceReprocessJob, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if lease != q.reconciliationJob.ReconciliationLease() {
		return k12storage.ProblemSourceReprocessJob{}, k12storage.ErrProblemSourceReprocessReconciliationFenced
	}
	q.reconciliationFailure = failure
	switch resolution {
	case k12storage.ProblemSourceReprocessOutcomeUnknownResolutionSucceeded:
		q.reconciliationSucceeded++
		q.reconciliationJob.Status = k12storage.ProblemSourceReprocessSucceeded
	case k12storage.ProblemSourceReprocessOutcomeUnknownResolutionNeedsConfirmation:
		q.reconciliationNeedsConfirmation++
		q.reconciliationJob.Status = k12storage.ProblemSourceReprocessNeedsConfirmation
	default:
		return k12storage.ProblemSourceReprocessJob{}, fmt.Errorf("unexpected resolution %q", resolution)
	}
	return q.reconciliationJob, nil
}

func (q *sourceReprocessQueueSpy) counts() (heartbeat, succeeded, needsConfirmation, retryable, outcomeUnknown int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.heartbeats, q.succeeded, q.needsConfirmation, q.retryable, q.outcomeUnknown
}

func (q *sourceReprocessQueueSpy) releaseCounts() (ordinary, reconciliation int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.released, q.reconciliationReleased
}

func (q *sourceReprocessQueueSpy) makeClaimable() {
	q.mu.Lock()
	q.claimable = true
	q.mu.Unlock()
}

func (q *sourceReprocessQueueSpy) observeNextSuccess() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.succeededObserved = make(chan struct{})
	q.succeededOnce = sync.Once{}
	return q.succeededObserved
}

type sourceReprocessProcessorFunc func(context.Context, k12storage.ProblemSourceReprocessJob) error

func (fn sourceReprocessProcessorFunc) ProcessProblemSourceReprocess(
	ctx context.Context,
	job k12storage.ProblemSourceReprocessJob,
) error {
	return fn(ctx, job)
}

type sourceReprocessBlockingSolver struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *sourceReprocessBlockingSolver) Solve(
	ctx context.Context,
	_, _, _ string,
) (SolveResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return SolveResult{
			Solution: "2",
			Evidence: SolveEvidence{
				Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec,
			},
		}, nil
	case <-ctx.Done():
		return SolveResult{}, ctx.Err()
	}
}

func (s *sourceReprocessBlockingSolver) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// sourceReprocessPhysicalRecognizer exercises the same physical-call context
// contract as the production vision adapter. Each logical recognition emits
// one durable whole-page provider call before returning its typed batch.
type sourceReprocessPhysicalRecognizer struct {
	mu         sync.Mutex
	batches    [][]RecognizedQuestion
	sendErrors []error
	calls      int
	sends      int
}

func (r *sourceReprocessPhysicalRecognizer) Recognize(
	ctx context.Context,
	image []byte,
) ([]RecognizedQuestion, error) {
	r.mu.Lock()
	batchIndex := r.calls
	r.calls++
	r.mu.Unlock()
	if batchIndex >= len(r.batches) {
		return nil, fmt.Errorf("unexpected recognition call %d", batchIndex+1)
	}
	_, err := k12.ExecuteRecognitionPhysicalCall(
		ctx,
		k12.RecognitionPhysicalCall{
			Unit: k12.RecognitionPhysicalUnitWholePage, Image: image,
		},
		func(context.Context) (string, error) {
			r.mu.Lock()
			r.sends++
			send := r.sends
			var sendErr error
			if batchIndex < len(r.sendErrors) {
				sendErr = r.sendErrors[batchIndex]
			}
			r.mu.Unlock()
			return fmt.Sprintf(`{"source_reprocess_test_send":%d}`, send), sendErr
		},
	)
	if err != nil {
		return nil, err
	}
	return cloneRecognizedQuestions(r.batches[batchIndex]), nil
}

func (r *sourceReprocessPhysicalRecognizer) counts() (calls, sends int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.sends
}

func TestProblemSourceReprocessWorkerHeartbeatsBlockedWorkAndCompletesWithCurrentFence(t *testing.T) {
	heartbeatObserved := make(chan struct{})
	queue := &sourceReprocessQueueSpy{
		job: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-1", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "correct_text", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-1"},
		},
		claimable: true, heartbeatObserved: heartbeatObserved,
	}
	processorCalls := 0
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(ctx context.Context, _ k12storage.ProblemSourceReprocessJob) error {
			processorCalls++
			select {
			case <-heartbeatObserved:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}),
		WorkerID: "worker-1", LeaseDuration: 100 * time.Millisecond,
		HeartbeatInterval: time.Millisecond,
	}

	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("run one durable source reprocess: processed=%v err=%v", processed, err)
	}
	heartbeats, succeeded, needsConfirmation, retryable, outcomeUnknown := queue.counts()
	if processorCalls != 1 || heartbeats < 1 || succeeded != 1 ||
		needsConfirmation != 0 || retryable != 0 || outcomeUnknown != 0 {
		t.Fatalf(
			"worker effects calls=%d heartbeat=%d succeeded=%d needs_confirmation=%d retryable=%d unknown=%d",
			processorCalls, heartbeats, succeeded, needsConfirmation, retryable, outcomeUnknown,
		)
	}
}

func TestProblemSourceReprocessWorkerProcessorCompletionDoesNotMisclassifyCancelledHeartbeatAsFence(t *testing.T) {
	heartbeatObserved := make(chan struct{})
	queue := &sourceReprocessQueueSpy{
		job: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-heartbeat-race", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "correct_text", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-1"},
		},
		claimable: true, heartbeatObserved: heartbeatObserved,
		heartbeatWaitForCancel: true,
	}
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(context.Context, k12storage.ProblemSourceReprocessJob) error {
			<-heartbeatObserved
			return nil
		}),
		WorkerID: "worker-heartbeat-race", LeaseDuration: 100 * time.Millisecond,
		HeartbeatInterval: time.Millisecond,
	}
	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("processor/heartbeat completion race: processed=%v err=%v", processed, err)
	}
	_, succeeded, _, _, _ := queue.counts()
	if succeeded != 1 {
		t.Fatalf("processor result was lost after local heartbeat cancellation: succeeded=%d", succeeded)
	}
}

func TestProblemSourceReprocessWorkerParksAmbiguousProviderOutcomeWithoutRetry(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	queue := &sourceReprocessQueueSpy{
		job: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-unknown", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "resume", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-1"},
		},
		claimable: true, heartbeatObserved: make(chan struct{}),
	}
	processorCalls := 0
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(context.Context, k12storage.ProblemSourceReprocessJob) error {
			processorCalls++
			return errors.Join(
				context.DeadlineExceeded,
				ErrGradingPhysicalCallOutcomeUnknown,
				ErrModelInvocationRequiresReconciliation,
			)
		}),
		WorkerID: "worker-unknown", LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
		Now:               func() time.Time { return now },
	}

	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("park ambiguous source reprocess: processed=%v err=%v", processed, err)
	}
	processed, err = worker.RunOnce(context.Background())
	if err != nil || processed {
		t.Fatalf("outcome_unknown must not be claimable again: processed=%v err=%v", processed, err)
	}
	_, succeeded, needsConfirmation, retryable, outcomeUnknown := queue.counts()
	if processorCalls != 1 || succeeded != 0 || needsConfirmation != 0 || retryable != 0 || outcomeUnknown != 1 {
		t.Fatalf(
			"ambiguous effects calls=%d succeeded=%d needs_confirmation=%d retryable=%d unknown=%d",
			processorCalls, succeeded, needsConfirmation, retryable, outcomeUnknown,
		)
	}
	queue.mu.Lock()
	reconcileAt := queue.outcomeUnknownFailure.RetryAt
	queue.mu.Unlock()
	if want := now.Add(30 * time.Second); !reconcileAt.Equal(want) {
		t.Fatalf("ambiguous outcome reconciliation time=%v, want grace deadline %v", reconcileAt, want)
	}
}

func TestProblemSourceReprocessWorkerReconcilesOutcomeUnknownBeforeOrdinaryClaim(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	queue := &sourceReprocessQueueSpy{
		reconciliationJob: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-reconcile-success", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "retake", InputRevision: 2, AttemptCount: 3,
			AffectedProblemIDs: []string{"problem-1"},
		},
		reconciliationClaimable: true,
		job: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-ordinary", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-2", Action: "correct_text", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-2"},
		},
		claimable: true,
	}
	var processedJob k12storage.ProblemSourceReprocessJob
	var reconciliationOnly bool
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(
			ctx context.Context,
			job k12storage.ProblemSourceReprocessJob,
		) error {
			processedJob = job
			reconciliationOnly = problemSourceReconciliationOnly(ctx)
			return nil
		}),
		WorkerID: "worker-reconcile-success", LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
		Now:               func() time.Time { return now },
	}

	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("reconcile committed outcome: processed=%v err=%v", processed, err)
	}
	queue.mu.Lock()
	reconciliationClaims := queue.reconciliationClaims
	ordinaryClaims := queue.claimCalls
	reconciliationSucceeded := queue.reconciliationSucceeded
	reconciliationNeedsConfirmation := queue.reconciliationNeedsConfirmation
	queue.mu.Unlock()
	if processedJob.WorkID != "work-reconcile-success" || processedJob.AttemptCount != 3 {
		t.Fatalf("reconciliation changed stable work identity: %+v", processedJob)
	}
	if !reconciliationOnly {
		t.Fatal("outcome_unknown processor did not receive the no-send reconciliation fence")
	}
	if reconciliationClaims != 1 || ordinaryClaims != 0 ||
		reconciliationSucceeded != 1 || reconciliationNeedsConfirmation != 0 {
		t.Fatalf(
			"reconciliation claims=%d ordinary=%d succeeded=%d confirmation=%d",
			reconciliationClaims, ordinaryClaims,
			reconciliationSucceeded, reconciliationNeedsConfirmation,
		)
	}
}

func TestProblemSourceReprocessWorkerParksEveryUnresolvedReconciliation(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "ambiguous physical outcome", err: ErrModelInvocationRequiresReconciliation},
		{name: "domain confirmation", err: &ProblemSourceReprocessNeedsConfirmationError{
			Code: "source_risk_requires_confirmation", Detail: "source remains risky",
		}},
		{name: "local processing error", err: errors.New("local result projection unavailable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queue := &sourceReprocessQueueSpy{
				reconciliationJob: k12storage.ProblemSourceReprocessJob{
					WorkID: "work-reconcile-unresolved", OwnerScope: "owner-1",
					AgentName: "mingming", JobID: "job-1", Action: "retake",
					InputRevision: 2, AttemptCount: 4,
					AffectedProblemIDs: []string{"problem-1"},
				},
				reconciliationClaimable: true,
			}
			worker := &ProblemSourceReprocessWorker{
				Records: queue,
				Processor: sourceReprocessProcessorFunc(func(
					context.Context,
					k12storage.ProblemSourceReprocessJob,
				) error {
					return tt.err
				}),
				WorkerID: "worker-reconcile-unresolved", LeaseDuration: time.Second,
				HeartbeatInterval: 100 * time.Millisecond,
			}

			processed, err := worker.RunOnce(context.Background())
			if err != nil || !processed {
				t.Fatalf("reconcile unresolved result: processed=%v err=%v", processed, err)
			}
			queue.mu.Lock()
			claims := queue.reconciliationClaims
			ordinaryClaims := queue.claimCalls
			succeeded := queue.reconciliationSucceeded
			confirmation := queue.reconciliationNeedsConfirmation
			failure := queue.reconciliationFailure
			attemptCount := queue.reconciliationJob.AttemptCount
			queue.mu.Unlock()
			if claims != 1 || ordinaryClaims != 0 || succeeded != 0 || confirmation != 1 {
				t.Fatalf(
					"unresolved reconciliation claims=%d ordinary=%d succeeded=%d confirmation=%d",
					claims, ordinaryClaims, succeeded, confirmation,
				)
			}
			if failure.Code != "provider_outcome_unresolved" || failure.Detail == "" ||
				!failure.RetryAt.IsZero() || attemptCount != 4 {
				t.Fatalf("unresolved terminal evidence=%+v attempt_count=%d", failure, attemptCount)
			}
		})
	}
}

func TestProblemSourceReprocessWorkerNeverResolvesAfterReconciliationHeartbeatFence(t *testing.T) {
	queue := &sourceReprocessQueueSpy{
		reconciliationJob: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-reconcile-fenced", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "retake", InputRevision: 2, AttemptCount: 2,
			AffectedProblemIDs: []string{"problem-1"},
		},
		reconciliationClaimable:    true,
		reconciliationHeartbeatErr: k12storage.ErrProblemSourceReprocessReconciliationFenced,
	}
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(
			ctx context.Context,
			_ k12storage.ProblemSourceReprocessJob,
		) error {
			<-ctx.Done()
			return ctx.Err()
		}),
		WorkerID: "worker-reconcile-fenced", LeaseDuration: 100 * time.Millisecond,
		HeartbeatInterval: time.Millisecond,
	}

	processed, err := worker.RunOnce(context.Background())
	if !processed || !errors.Is(
		err,
		k12storage.ErrProblemSourceReprocessReconciliationFenced,
	) {
		t.Fatalf("fenced reconciliation: processed=%v err=%v", processed, err)
	}
	queue.mu.Lock()
	succeeded := queue.reconciliationSucceeded
	confirmation := queue.reconciliationNeedsConfirmation
	queue.mu.Unlock()
	if succeeded != 0 || confirmation != 0 {
		t.Fatalf(
			"fenced reconciliation mutated terminal state: succeeded=%d confirmation=%d",
			succeeded, confirmation,
		)
	}
}

func TestProblemSourceReprocessWorkerNeverRetriesAmbiguousRecognitionPhysicalCalls(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "provider outcome unknown",
			err:  ErrRecognitionPhysicalCallOutcomeUnknown,
		},
		{
			name: "another worker owns sent child",
			err:  ErrRecognitionPhysicalCallObservedInFlight,
		},
		{
			name: "succeeded child lacks source result commit",
			err:  ErrModelInvocationRequiresReconciliation,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queue := &sourceReprocessQueueSpy{
				job: k12storage.ProblemSourceReprocessJob{
					WorkID: "work-recognition-ambiguous", OwnerScope: "owner-1",
					AgentName: "mingming", JobID: "job-1", Action: "retake",
					InputRevision: 2, AffectedProblemIDs: []string{"problem-1"},
				},
				claimable: true, heartbeatObserved: make(chan struct{}),
			}
			worker := &ProblemSourceReprocessWorker{
				Records: queue,
				Processor: sourceReprocessProcessorFunc(func(
					context.Context,
					k12storage.ProblemSourceReprocessJob,
				) error {
					return tt.err
				}),
				WorkerID:      "worker-recognition-ambiguous",
				LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
			}

			processed, err := worker.RunOnce(context.Background())
			if err != nil || !processed {
				t.Fatalf("settle ambiguous recognition call: processed=%v err=%v", processed, err)
			}
			_, succeeded, confirmation, retryable, unknown := queue.counts()
			if succeeded != 0 || confirmation != 0 || retryable != 0 || unknown != 1 {
				t.Fatalf(
					"ambiguous recognition settlement succeeded=%d confirmation=%d retryable=%d unknown=%d",
					succeeded, confirmation, retryable, unknown,
				)
			}
		})
	}
}

func TestProblemSourceReprocessWorkerNeverSettlesAfterHeartbeatFence(t *testing.T) {
	heartbeatObserved := make(chan struct{})
	queue := &sourceReprocessQueueSpy{
		job: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-fenced", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "correct_text", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-1"},
		},
		claimable: true, heartbeatObserved: heartbeatObserved,
		heartbeatErr: k12storage.ErrProblemSourceReprocessFenced,
	}
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(ctx context.Context, _ k12storage.ProblemSourceReprocessJob) error {
			<-ctx.Done()
			return nil
		}),
		WorkerID: "worker-fenced", LeaseDuration: 100 * time.Millisecond,
		HeartbeatInterval: time.Millisecond,
	}

	processed, err := worker.RunOnce(context.Background())
	if !processed || !errors.Is(err, k12storage.ErrProblemSourceReprocessFenced) {
		t.Fatalf("fenced worker: processed=%v err=%v", processed, err)
	}
	_, succeeded, needsConfirmation, retryable, outcomeUnknown := queue.counts()
	if succeeded != 0 || needsConfirmation != 0 || retryable != 0 || outcomeUnknown != 0 {
		t.Fatalf(
			"fenced worker mutated terminal state: succeeded=%d needs_confirmation=%d retryable=%d unknown=%d",
			succeeded, needsConfirmation, retryable, outcomeUnknown,
		)
	}
}

func TestProblemSourceReprocessWorkerStartupPollClaimsWorkThatBecomesDueWithoutAnotherCommand(t *testing.T) {
	succeededObserved := make(chan struct{})
	queue := &sourceReprocessQueueSpy{
		job: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-due-later", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "resume", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-1"},
		},
		heartbeatObserved: make(chan struct{}), succeededObserved: succeededObserved,
	}
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(context.Context, k12storage.ProblemSourceReprocessJob) error {
			return nil
		}),
		WorkerID: "worker-poll", LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond, PollInterval: time.Millisecond,
	}
	if !worker.Start() {
		t.Fatal("startup recovery poll was not accepted")
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := worker.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown source reprocess poll: %v", err)
		}
	})
	queue.makeClaimable()
	select {
	case <-succeededObserved:
	case <-time.After(time.Second):
		t.Fatal("due source reprocess was not claimed by the startup poll")
	}
}

func TestProblemSourceReprocessWorkerQuiesceCancelsDrainsFencesAndResumes(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	queue := &sourceReprocessQueueSpy{
		job: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-quiesce", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "correct_text", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-1"}, AttemptCount: 4,
		},
		claimable: true, heartbeatObserved: make(chan struct{}),
	}
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(
			ctx context.Context,
			_ k12storage.ProblemSourceReprocessJob,
		) error {
			callsMu.Lock()
			calls++
			call := calls
			callsMu.Unlock()
			if call == 1 {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				return ctx.Err()
			}
			return nil
		}),
		WorkerID: "worker-quiesce", LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond, MaxAttempts: 5,
	}
	if !worker.Nudge() {
		t.Fatal("initial source drain was not accepted")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("source processor did not enter blocking work")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := worker.Quiesce(drainCtx)
	if err != nil {
		t.Fatalf("quiesce source worker: %v", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("quiesce returned before the active processor observed cancellation")
	}
	_, succeeded, confirmation, retryable, unknown := queue.counts()
	released, reconciliationReleased := queue.releaseCounts()
	if succeeded != 0 || confirmation != 0 || retryable != 0 || unknown != 0 ||
		released != 1 || reconciliationReleased != 0 {
		t.Fatalf(
			"cancelled drain settlement succeeded=%d confirmation=%d retryable=%d unknown=%d released=%d reconciliation_released=%d",
			succeeded, confirmation, retryable, unknown, released, reconciliationReleased,
		)
	}
	queue.makeClaimable()
	succeededObserved := queue.observeNextSuccess()
	if worker.Nudge() {
		t.Fatal("quiesced worker accepted a new drain")
	}
	release()
	release()
	select {
	case <-succeededObserved:
	case <-time.After(time.Second):
		t.Fatal("idempotent release did not resume the durable source queue")
	}
	callsMu.Lock()
	gotCalls := calls
	callsMu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("source processor calls=%d, want cancelled+resumed calls", gotCalls)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := worker.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown resumed source worker: %v", err)
	}
}

func TestProblemSourceReprocessWorkerQuiesceReleasesOutcomeUnknownWithoutTerminalizing(
	t *testing.T,
) {
	entered := make(chan struct{})
	queue := &sourceReprocessQueueSpy{
		reconciliationJob: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-unknown-quiesce", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "retake", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-1"},
			Status:             k12storage.ProblemSourceReprocessOutcomeUnknown,
		},
		reconciliationClaimable: true,
		heartbeatObserved:       make(chan struct{}),
	}
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(
			ctx context.Context,
			_ k12storage.ProblemSourceReprocessJob,
		) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}),
		WorkerID: "worker-unknown-quiesce", LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
	}
	if !worker.Nudge() {
		t.Fatal("outcome_unknown reconciliation was not scheduled")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("outcome_unknown reconciliation did not enter processor")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := worker.Quiesce(ctx)
	if err != nil {
		t.Fatalf("quiesce outcome_unknown reconciliation: %v", err)
	}
	queue.mu.Lock()
	status := queue.reconciliationJob.Status
	succeeded := queue.reconciliationSucceeded
	confirmation := queue.reconciliationNeedsConfirmation
	queue.mu.Unlock()
	_, reconciliationReleased := queue.releaseCounts()
	if status != k12storage.ProblemSourceReprocessOutcomeUnknown ||
		succeeded != 0 || confirmation != 0 || reconciliationReleased != 1 {
		t.Fatalf(
			"cancelled reconciliation status=%s succeeded=%d confirmation=%d released=%d",
			status, succeeded, confirmation, reconciliationReleased,
		)
	}
	release()
	if err := worker.Wait(ctx); err != nil {
		t.Fatalf("wait after reconciliation release: %v", err)
	}
	if err := worker.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestProblemSourceReprocessWorkerOverlappingQuiesceHoldsFenceUntilLastRelease(
	t *testing.T,
) {
	queue := &sourceReprocessQueueSpy{heartbeatObserved: make(chan struct{})}
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(
			context.Context,
			k12storage.ProblemSourceReprocessJob,
		) error {
			return nil
		}),
		WorkerID: "worker-overlapping-quiesce", LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseFirst, err := worker.Quiesce(ctx)
	if err != nil {
		t.Fatalf("first quiesce: %v", err)
	}
	releaseSecond, err := worker.Quiesce(ctx)
	if err != nil {
		t.Fatalf("second quiesce: %v", err)
	}
	releaseFirst()
	if worker.Nudge() {
		t.Fatal("first release removed a fence still held by the second caller")
	}
	releaseSecond()
	if !worker.Nudge() {
		t.Fatal("last release did not restore source reprocess scheduling")
	}
	if err := worker.Wait(ctx); err != nil {
		t.Fatalf("wait after final release: %v", err)
	}
	if err := worker.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestImageTaskCoordinatorQuiesceAgentAlsoDrainsSourceReprocessWorker(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	queue := &sourceReprocessQueueSpy{
		job: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-agent-delete", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "resume", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-1"},
		},
		claimable: true, heartbeatObserved: make(chan struct{}),
	}
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(
			ctx context.Context,
			_ k12storage.ProblemSourceReprocessJob,
		) error {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-ctx.Done()
			select {
			case <-cancelled:
			default:
				close(cancelled)
			}
			return ctx.Err()
		}),
		WorkerID: "worker-agent-delete", LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
	}
	coordinator := &ImageTaskCoordinator{SourceReprocess: worker}
	if !worker.Nudge() {
		t.Fatal("source work was not scheduled")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("source work did not enter processor")
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resume, err := coordinator.QuiesceAgent(drainCtx, "mingming")
	if err != nil {
		t.Fatalf("coordinator quiesce did not drain source worker: %v", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("coordinator returned before source processor cancellation")
	}
	queue.makeClaimable()
	if worker.Nudge() {
		t.Fatal("coordinator Agent quiesce left source worker unfenced")
	}
	// Do not let the deliberately blocking second fixture run after release.
	queue.mu.Lock()
	queue.claimable = false
	queue.mu.Unlock()
	resume()
	resume()
	if !worker.Nudge() {
		t.Fatal("coordinator resume left source worker permanently paused")
	}
	if err := worker.Wait(drainCtx); err != nil {
		t.Fatalf("wait for resumed source drain: %v", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := worker.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown source worker: %v", err)
	}
}

func advanceProblemInputRevisionForSourceWorker(
	t *testing.T,
	o *GradingOrchestrator,
	job GradingJobView,
	question RecognizedQuestion,
	revision int,
	digest string,
	canonical string,
) {
	t.Helper()
	tx, err := o.deps.Records.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		UPDATE k12_problem_input_revisions
		SET current_disposition='superseded',updated_at=updated_at+1
		WHERE agent_name=? AND submission_id=? AND structure_version=1
		  AND problem_id=? AND input_revision=? AND current_disposition='current'`,
		job.Record.AgentName,
		job.Fields.SubmissionID,
		question.ProblemID,
		revision-1,
	); err != nil {
		t.Fatalf("supersede prior immutable input: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO k12_problem_input_revisions (
			agent_name,submission_id,structure_version,problem_id,input_revision,
			page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
			question_canonical_markdown,answer_canonical_markdown,input_digest,
			current_disposition,origin_command_receipt_id,origin_kind,created_at,updated_at
		)
		SELECT agent_name,submission_id,structure_version,problem_id,?,
		       page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
		       ?,answer_canonical_markdown,?,'current',NULL,'command',created_at,updated_at+1
		FROM k12_problem_input_revisions
		WHERE agent_name=? AND submission_id=? AND structure_version=1
		  AND problem_id=? AND input_revision=?`,
		revision,
		canonical,
		digest,
		job.Record.AgentName,
		job.Fields.SubmissionID,
		question.ProblemID,
		revision-1,
	); err != nil {
		t.Fatalf("append immutable corrected input: %v", err)
	}
	if _, err := tx.Exec(`
		UPDATE k12_problem_structure_members
		SET input_revision=?
		WHERE agent_name=? AND submission_id=? AND structure_version=1
		  AND problem_id=? AND input_revision=?`,
		revision,
		job.Record.AgentName,
		job.Fields.SubmissionID,
		question.ProblemID,
		revision-1,
	); err != nil {
		t.Fatalf("advance current structure input head: %v", err)
	}
	if _, err := tx.Exec(`
		UPDATE k12_attempts
		SET confirmed_version=?,input_digest=?,updated_at=updated_at+1
		WHERE agent_name=? AND submission_id=? AND problem_id=? AND confirmed_version=?`,
		revision,
		digest,
		job.Record.AgentName,
		job.Fields.SubmissionID,
		question.ProblemID,
		revision-1,
	); err != nil {
		t.Fatalf("advance Attempt input binding: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit corrected input fixture: %v", err)
	}
}

func TestGradingOrchestratorProblemSourceReprocessCorrectTextAssessesOnlyAffectedCurrentRevision(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "corrected affected", Subject: "数学",
			StudentAnswer: "2", AnswerState: AnswerStatePresent,
		},
		{
			Question: "unaffected", Subject: "数学",
			StudentAnswer: "3", AnswerState: AnswerStatePresent,
		},
	}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "source-worker-correct-exact-set")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	affected := run.questions[0]
	unaffected := run.questions[1]
	grader.outcomes = map[string]GradeOutcome{
		affected.Question: {
			Verdict: VerdictDisagree, WrongStep: "旧输入首步", ErrorCause: "旧输入误判",
		},
	}
	if _, err := o.assessDurablePhotoItem(
		context.Background(), o.deps, job, run.req, PhotoModeGrade, affected,
	); err != nil {
		t.Fatalf("seed superseded wrong assessment: %v", err)
	}
	grader.outcomes = nil
	seededMistakes, err := o.deps.Records.ListByScope(
		context.Background(), job.Record.AgentName, k12.CollectionMistakes, "",
	)
	if err != nil || len(seededMistakes) != 1 || seededMistakes[0].Status != k12.StatusNew {
		t.Fatalf("seeded source mistake=%+v err=%v", seededMistakes, err)
	}
	if _, err := o.deps.Records.DB().Exec(`UPDATE k12_grading_assessment_items
		SET current_disposition='superseded'
		WHERE agent_name=? AND job_id=? AND problem_id=? AND current_disposition='current'`,
		job.Record.AgentName, jobID, affected.ProblemID); err != nil {
		t.Fatalf("mark original wrong assessment superseded: %v", err)
	}
	solver.mu.Lock()
	solver.calls = map[string]int{}
	solver.mu.Unlock()
	grader.mu.Lock()
	grader.calls = map[string]int{}
	grader.mu.Unlock()
	unresolved := k12.GradingItemInvocation{
		InvocationID: "source-worker-unaffected-unknown", AgentName: job.Record.AgentName,
		JobID: jobID, ProblemID: unaffected.ProblemID, AttemptID: unaffected.AttemptID,
		Operation: k12.GradingItemOperationGrade, OperationAttempt: 99,
		RequestDigest: "sha256:unaffected-old-input",
		RouteSnapshot: k12.GradingModelSnapshot{Provider: "test", Model: "test-model", Route: "default"},
	}
	if _, _, err := o.deps.Records.PrepareGradingItemInvocation(context.Background(), unresolved); err != nil {
		t.Fatal(err)
	}
	if _, err := o.deps.Records.MarkGradingItemInvocationSent(context.Background(), job.Record.AgentName, unresolved.InvocationID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(context.Background(), job.Record.AgentName, unresolved.InvocationID, "provider", "timeout"); err != nil {
		t.Fatal(err)
	}
	job, err = o.deps.saveGradingJob(context.Background(), job, k12.GradingStageFailedRetryable)
	if err != nil {
		t.Fatal(err)
	}
	const currentDigest = "sha256:source-worker-correct-v2"
	advanceProblemInputRevisionForSourceWorker(
		t, o, job, affected, 2, currentDigest, "corrected affected",
	)
	work := k12storage.ProblemSourceReprocessJob{
		WorkID: "work-correct", CommandReceiptID: "receipt-correct",
		OwnerScope: "owner-1", AgentName: job.Record.AgentName,
		DispatchID: "dispatch-correct",
		JobID:      jobID, ProblemID: affected.ProblemID, Action: "correct_text",
		StructureVersion: 1, InputRevision: 2, InputDigest: "sha256:work-correct",
		AffectedProblemIDs: []string{affected.ProblemID},
	}
	current, err := o.deps.loadCurrentConfirmedQuestions(
		context.Background(), job.Record.AgentName, job.Fields.SubmissionID,
	)
	if err != nil {
		t.Fatalf("load current corrected immutable input: %v", err)
	}
	for _, question := range current {
		if question.ProblemID == affected.ProblemID &&
			(question.ConfirmedVersion != 2 || question.InputDigest != currentDigest) {
			t.Fatalf(
				"corrected immutable input projection=(v%d,%q), want (v2,%q)",
				question.ConfirmedVersion,
				question.InputDigest,
				currentDigest,
			)
		}
	}
	if err := o.ProcessProblemSourceReprocess(context.Background(), work); err != nil {
		t.Fatalf("process corrected immutable input: %v", err)
	}
	seededMistakes, err = o.deps.Records.ListByScope(
		context.Background(), job.Record.AgentName, k12.CollectionMistakes, "",
	)
	if err != nil || len(seededMistakes) != 1 {
		t.Fatalf("source correction changed mistake cardinality: mistakes=%+v err=%v", seededMistakes, err)
	}
	correctedFields, parseErr := k12.ParseMistakeFields(seededMistakes[0].Fields)
	if parseErr != nil || seededMistakes[0].Status != k12.StatusArchived ||
		correctedFields.ArchivedReason != "source_correction" ||
		correctedFields.ReviewStage != 0 || correctedFields.LastRetriedAt != 0 {
		t.Fatalf("source correction must retract its unique mistake without retry evidence: record=%+v fields=%+v err=%v",
			seededMistakes[0], correctedFields, parseErr)
	}
	if solver.callCount("corrected affected") != 1 || grader.callCount("corrected affected") != 1 {
		t.Fatalf(
			"affected current input calls solver=%d grader=%d, want one each",
			solver.callCount("corrected affected"), grader.callCount("corrected affected"),
		)
	}
	if solver.callCount("unaffected") != 0 || grader.callCount("unaffected") != 0 {
		t.Fatalf(
			"unaffected problem reached provider: solver=%d grader=%d",
			solver.callCount("unaffected"), grader.callCount("unaffected"),
		)
	}
	receipt, err := o.deps.Records.GetGradingAssessmentItem(
		context.Background(), job.Record.AgentName, jobID, affected.ProblemID,
	)
	if err != nil || receipt.InputRevision != 2 || receipt.InputDigest != currentDigest {
		t.Fatalf("current affected assessment=%+v err=%v", receipt, err)
	}
	if _, err := o.deps.Records.GetGradingAssessmentItem(
		context.Background(), job.Record.AgentName, jobID, unaffected.ProblemID,
	); err == nil {
		t.Fatalf("unaffected problem %s unexpectedly has an assessment", unaffected.ProblemID)
	}

	if err := o.ProcessProblemSourceReprocess(context.Background(), work); err != nil {
		t.Fatalf("replay corrected immutable input: %v", err)
	}
	if solver.callCount("corrected affected") != 1 || grader.callCount("corrected affected") != 1 {
		t.Fatalf(
			"exact current input replay resent provider: solver=%d grader=%d",
			solver.callCount("corrected affected"), grader.callCount("corrected affected"),
		)
	}
	if solver.callCount("unaffected") != 0 || grader.callCount("unaffected") != 0 {
		t.Fatalf(
			"exact-set replay touched unrelated provider: solver=%d grader=%d",
			solver.callCount("unaffected"), grader.callCount("unaffected"),
		)
	}
	storedUnknown, err := o.deps.Records.GetGradingItemInvocation(context.Background(), job.Record.AgentName, unresolved.InvocationID)
	if err != nil || storedUnknown.Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf("unrelated unknown changed: invocation=%+v err=%v", storedUnknown, err)
	}
	storedJob, err := o.deps.GetGradingJob(context.Background(), job.Record.AgentName, jobID)
	if err != nil || storedJob.Record.Status != k12.GradingStageFailedRetryable {
		t.Fatalf("source reprocess changed page stage: job=%+v err=%v", storedJob, err)
	}

	unresolved.InvocationID = "source-worker-affected-old-unknown"
	unresolved.ProblemID = affected.ProblemID
	unresolved.AttemptID = affected.AttemptID
	unresolved.RequestDigest = "sha256:affected-old-input"
	if _, _, err := o.deps.Records.PrepareGradingItemInvocation(context.Background(), unresolved); err != nil {
		t.Fatal(err)
	}
	if _, err := o.deps.Records.MarkGradingItemInvocationSent(context.Background(), job.Record.AgentName, unresolved.InvocationID); err != nil {
		t.Fatal(err)
	}
	blocked := o.ProcessProblemSourceReprocess(context.Background(), work)
	var confirmation *ProblemSourceReprocessNeedsConfirmationError
	if !errors.As(blocked, &confirmation) || !strings.Contains(confirmation.Detail, unresolved.InvocationID) || !strings.Contains(confirmation.Detail, "sent") {
		t.Fatalf("affected sent invocation was not preserved: err=%v confirmation=%+v", blocked, confirmation)
	}
	if _, err := o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(context.Background(), job.Record.AgentName, unresolved.InvocationID, "provider", "timeout"); err != nil {
		t.Fatal(err)
	}
	blocked = o.ProcessProblemSourceReprocess(context.Background(), work)
	if !errors.As(blocked, &confirmation) || !strings.Contains(confirmation.Detail, unresolved.InvocationID) || !strings.Contains(confirmation.Detail, "outcome_unknown") {
		t.Fatalf("affected historical unknown was not preserved: err=%v confirmation=%+v", blocked, confirmation)
	}
	if solver.callCount("corrected affected") != 1 || grader.callCount("corrected affected") != 1 || solver.callCount("unaffected") != 0 || grader.callCount("unaffected") != 0 {
		t.Fatal("unresolved affected invocation allowed another provider call")
	}
}

func TestGradingOrchestratorProblemSourceReprocessSerializesWithRunGradingJob(t *testing.T) {
	baseSolver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "serialized affected", Subject: "数学",
		StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}, baseSolver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "source-worker-shared-job-lock")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	affected := run.questions[0]
	const currentDigest = "sha256:source-worker-shared-lock-v2"
	advanceProblemInputRevisionForSourceWorker(
		t, o, job, affected, 2, currentDigest, "serialized affected v2",
	)
	blocking := &sourceReprocessBlockingSolver{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	o.deps.Solver = blocking
	work := k12storage.ProblemSourceReprocessJob{
		WorkID: "work-shared-job-lock", CommandReceiptID: "receipt-shared-job-lock",
		OwnerScope: "owner-1", DispatchID: "dispatch-shared-job-lock",
		AgentName: job.Record.AgentName, JobID: jobID,
		ProblemID: affected.ProblemID, Action: "correct_text",
		StructureVersion: 1, InputRevision: 2,
		InputDigest:        "sha256:work-shared-job-lock",
		AffectedProblemIDs: []string{affected.ProblemID},
	}

	sourceDone := make(chan error, 1)
	go func() {
		sourceDone <- o.ProcessProblemSourceReprocess(context.Background(), work)
	}()
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("source reprocess did not reach its durable solve operation")
	}
	sharedLockHeld := !o.jobLock(jobID).TryLock()
	if !sharedLockHeld {
		o.jobLock(jobID).Unlock()
	}

	runDone := make(chan error, 1)
	go func() {
		_, err := o.RunGradingJob(context.Background(), jobID)
		runDone <- err
	}()
	close(blocking.release)
	var sourceErr, runErr error
	select {
	case sourceErr = <-sourceDone:
	case <-time.After(10 * time.Second):
		t.Fatal("source reprocess did not finish")
	}
	select {
	case runErr = <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("normal grading run did not finish")
	}
	if !sharedLockHeld {
		t.Fatal("source reprocess did not own the canonical grading job mutex during provider work")
	}
	if sourceErr != nil || runErr != nil {
		t.Fatalf("serialized source/normal run errors: source=%v normal=%v", sourceErr, runErr)
	}
	if blocking.callCount() != 1 || grader.callCount("serialized affected v2") != 1 {
		t.Fatalf(
			"concurrent source/normal run duplicated assessment: solve=%d grade=%d",
			blocking.callCount(), grader.callCount("serialized affected v2"),
		)
	}
	var artifactCount int
	if err := o.deps.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_grading_final_artifacts WHERE job_id=?`,
		jobID,
	).Scan(&artifactCount); err != nil {
		t.Fatal(err)
	}
	if artifactCount != 1 {
		t.Fatalf("concurrent source/normal run final artifacts=%d, want 1", artifactCount)
	}
}

func TestGradingOrchestratorProblemSourceReprocessResumeIsOCRFreeAndDoesNotTouchUnrelatedProblem(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "resume affected", Subject: "数学",
			StudentAnswer: "2", AnswerState: AnswerStatePresent,
		},
		{
			Question: "resume unaffected", Subject: "数学",
			StudentAnswer: "3", AnswerState: AnswerStatePresent,
		},
	}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "source-worker-resume-exact-set")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	affected := run.questions[0]
	unaffected := run.questions[1]
	seedBUG20260726031SkipReceipt(t, o, jobID, affected, "source-worker-resume")
	const currentDigest = "sha256:source-worker-resume-v2"
	advanceProblemInputRevisionForSourceWorker(
		t, o, job, affected, 2, currentDigest, affected.CanonicalMarkdown,
	)
	work := k12storage.ProblemSourceReprocessJob{
		WorkID: "work-resume", CommandReceiptID: "receipt-resume",
		OwnerScope: "owner-1", AgentName: job.Record.AgentName,
		DispatchID: "dispatch-resume",
		JobID:      jobID, ProblemID: affected.ProblemID, Action: "resume",
		StructureVersion: 1, InputRevision: 2, InputDigest: "sha256:work-resume",
		AffectedProblemIDs: []string{affected.ProblemID},
	}
	recognizer, ok := o.deps.Recognizer.(*countingRecognizer)
	if !ok || recognizer.calls != 1 {
		t.Fatalf("unexpected recognition fixture before resume: ok=%v calls=%d", ok, recognizer.calls)
	}

	if err := o.ProcessProblemSourceReprocess(context.Background(), work); err != nil {
		t.Fatalf("process resumed immutable input: %v", err)
	}
	if recognizer.calls != 1 {
		t.Fatalf("resume invoked OCR %d additional times, want zero", recognizer.calls-1)
	}
	if solver.callCount("resume affected") != 1 || grader.callCount("resume affected") != 1 {
		t.Fatalf(
			"resumed affected calls solver=%d grader=%d, want one each",
			solver.callCount("resume affected"), grader.callCount("resume affected"),
		)
	}
	if solver.callCount("resume unaffected") != 0 || grader.callCount("resume unaffected") != 0 {
		t.Fatalf(
			"resume touched unrelated problem: solver=%d grader=%d",
			solver.callCount("resume unaffected"), grader.callCount("resume unaffected"),
		)
	}
	var skipDisposition string
	if err := o.deps.Records.DB().QueryRow(`
		SELECT current_disposition
		FROM k12_problem_skip_receipts
		WHERE job_id=? AND problem_id=? AND input_revision=1`,
		jobID,
		affected.ProblemID,
	).Scan(&skipDisposition); err != nil {
		t.Fatal(err)
	}
	if skipDisposition != "superseded" {
		t.Fatalf("resume left prior skip disposition=%q", skipDisposition)
	}
	if _, err := o.deps.Records.GetGradingAssessmentItem(
		context.Background(), job.Record.AgentName, jobID, unaffected.ProblemID,
	); err == nil {
		t.Fatalf("unaffected problem %s unexpectedly has an assessment", unaffected.ProblemID)
	}
}

func TestGradingOrchestratorProblemSourceReprocessParksStaleRevisionBeforeProviderCall(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "stale source", Subject: "数学",
		StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "source-worker-stale-revision")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	work := k12storage.ProblemSourceReprocessJob{
		WorkID: "work-stale", CommandReceiptID: "receipt-stale",
		OwnerScope: "owner-1", AgentName: job.Record.AgentName,
		DispatchID: "dispatch-stale",
		JobID:      jobID, ProblemID: run.questions[0].ProblemID, Action: "correct_text",
		StructureVersion: 1, InputRevision: 2, InputDigest: "sha256:work-stale",
		AffectedProblemIDs: []string{run.questions[0].ProblemID},
	}

	err := o.ProcessProblemSourceReprocess(context.Background(), work)
	var confirmation *ProblemSourceReprocessNeedsConfirmationError
	if !errors.As(err, &confirmation) || confirmation.Code != "input_revision_changed" {
		t.Fatalf("stale source revision err=%v confirmation=%+v", err, confirmation)
	}
	if solver.callCount("stale source") != 0 || grader.callCount("stale source") != 0 {
		t.Fatalf(
			"stale source reached provider: solver=%d grader=%d",
			solver.callCount("stale source"), grader.callCount("stale source"),
		)
	}
}

func TestMapProblemSourceRecognitionRetakeUsesStableStructureAndIgnoresUnrelatedCandidates(t *testing.T) {
	work := k12storage.ProblemSourceReprocessJob{
		WorkID: "work-map", Action: "retake",
		AffectedProblemIDs: []string{"problem-1"},
	}
	current := []RecognizedQuestion{
		{
			ProblemID: "problem-1", Question: "old one",
			SourceNumberPath: []string{"1"}, DisplayLabel: "1",
			Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "3",
		},
		{
			ProblemID: "problem-2", Question: "old two",
			SourceNumberPath: []string{"2"}, DisplayLabel: "2",
			Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "5",
		},
	}
	recognized := []RecognizedQuestion{
		{
			Question: "new unrelated", SourceNumberPath: []string{"2"}, DisplayLabel: "2",
			Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "6",
		},
		{
			Question: "new affected", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
			Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		},
	}
	items, err := mapProblemSourceRecognitionExactSet(work, current, recognized)
	if err != nil {
		t.Fatalf("map stable retake exact-set: %v", err)
	}
	if len(items) != 1 || items[0].ProblemID != "problem-1" ||
		items[0].QuestionCanonicalMarkdown != "new affected" ||
		items[0].AnswerCanonicalMarkdown != "4" {
		t.Fatalf("stable exact-set mapping=%+v", items)
	}
}

func TestMapProblemSourceRecognitionOnlySelectRegionAllowsExplicitOneToOneMapping(t *testing.T) {
	current := []RecognizedQuestion{{
		ProblemID: "problem-1", Question: "old", Subject: "数学",
		AnswerState: AnswerStatePresent, StudentAnswer: "3",
	}}
	recognized := []RecognizedQuestion{{
		Question: "new", Subject: "数学",
		AnswerState: AnswerStatePresent, StudentAnswer: "4",
	}}
	retake := k12storage.ProblemSourceReprocessJob{
		WorkID: "work-retake-unlabeled", Action: "retake",
		AffectedProblemIDs: []string{"problem-1"},
	}
	if _, err := mapProblemSourceRecognitionExactSet(
		retake, current, recognized,
	); err == nil {
		t.Fatal("unlabeled retake was guessed as a stable mapping")
	}
	selectRegion := retake
	selectRegion.WorkID = "work-select-explicit"
	selectRegion.Action = "select_region"
	if items, err := mapProblemSourceRecognitionExactSet(
		selectRegion, current, recognized,
	); err != nil || len(items) != 1 || items[0].ProblemID != "problem-1" {
		t.Fatalf("explicit one-to-one selected crop mapping=%+v err=%v", items, err)
	}
}

type sourceReprocessIntegrationFixture struct {
	coordinator  *ImageTaskCoordinator
	orchestrator *GradingOrchestrator
	recognizer   *sourceReprocessPhysicalRecognizer
	repository   *PageAssetRepository
	solver       *itemResumeSolver
	grader       *itemResumeGrader
	dispatchID   string
	job          GradingJobView
	run          *gradingRun
}

type sourceCurrentTipsSpy struct {
	mu       sync.Mutex
	concepts []string
}

type sourceActionRaceSolver struct {
	delegate *itemResumeSolver
	problem  string
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (s *sourceActionRaceSolver) Solve(
	ctx context.Context,
	problem string,
	studentAnswer string,
	subject string,
) (SolveResult, error) {
	if problem == s.problem {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return SolveResult{}, ctx.Err()
		}
	}
	return s.delegate.Solve(ctx, problem, studentAnswer, subject)
}

type sourceReprocessLogBuffer struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (b *sourceReprocessLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.Write(p)
}

func (b *sourceReprocessLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.String()
}

func (s *sourceCurrentTipsSpy) GenerateTutoringTipsReview(
	_ context.Context,
	_ string,
	knowledgePoint string,
	_ string,
) (string, error) {
	s.mu.Lock()
	s.concepts = append(s.concepts, knowledgePoint)
	s.mu.Unlock()
	return "只基于当前确认事实生成。", nil
}

func (s *sourceCurrentTipsSpy) saw(concept string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, current := range s.concepts {
		if current == concept {
			return true
		}
	}
	return false
}

func newSourceReprocessIntegrationFixture(
	t *testing.T,
	retakeQuestions []RecognizedQuestion,
) sourceReprocessIntegrationFixture {
	t.Helper()
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	initialQuestions := []RecognizedQuestion{
		{
			Question: "old affected", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
			Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "3",
			KnowledgePoints:              []string{"整数加法"},
			RecognitionConfidence:        float64Pointer(0.42),
			EvidenceTranscriptions:       []string{"old affected"},
			AnswerEvidenceTranscriptions: []string{"3"},
		},
		{
			Question: "unrelated", SourceNumberPath: []string{"2"}, DisplayLabel: "2",
			Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "5",
			KnowledgePoints:              []string{"整数加法"},
			RecognitionConfidence:        float64Pointer(0.42),
			EvidenceTranscriptions:       []string{"unrelated"},
			AnswerEvidenceTranscriptions: []string{"5"},
		},
	}
	recognizer := &sourceReprocessPhysicalRecognizer{
		batches: [][]RecognizedQuestion{initialQuestions, retakeQuestions},
	}
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	deps, _ := newPipeline(t, solver, grader, nil)
	deps.Recognizer = recognizer
	deps.PhotoAnnotator = &photoAnnotatorFake{}
	deps.ParentTeachingGuide = &parentTeachingGuideSpy{}
	deps.PageAssets = assetstore.PageStore{}
	deps.Profiles = newMemProfiles()
	deps.Profiles.(*memProfiles).m["mingming"] = k12.ChildProfile{
		ChildName: "小明", GradeTerm: "五年级上",
	}
	deps.GradingBudgetSnapshot = orchestratorTestBudget()
	deps.Now = func() int64 { return time.Now().Unix() }
	resolveSnapshot := func(
		requested k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		if requested.Provider == "" {
			requested.Provider = "hexclaw-gpt"
		}
		if requested.Model == "" {
			requested.Model = k12.RecognizingPolicyModel
		}
		requested.Route = requested.Provider + "/" + requested.Model
		requested.Capability = "vision"
		requested.RecognizingRequestPolicy = k12.ApprovedRecognizingRequestPolicy()
		return k12.NormalizeGradingModelSnapshot(requested), nil
	}
	orchestrator := trackGradingOrchestrator(
		t,
		NewGradingOrchestrator(
			deps, resolveSnapshot, WithGradingRunDir(t.TempDir()),
		),
	)
	repository := &PageAssetRepository{Records: deps.Records}
	coordinator := &ImageTaskCoordinator{
		Records: deps.Records, PageAssets: repository,
		Classifier: &imageTaskClassifierStub{result: ImageTaskClassification{
			Intent:         k12.ImageTaskIntentCompletedHomework,
			IntentEvidence: []string{"completed homework"}, Confidence: 1,
		}},
		Grading: orchestrator, ResolveRoute: imageTaskRouteForTest,
		ResolveGrade: func(context.Context, string) (string, error) {
			return "五年级上", nil
		},
		GradingBudgetSnapshot: orchestratorTestBudget(),
		Now:                   func() int64 { return time.Now().Unix() },
	}
	ready, err := repository.Persist(
		context.Background(), "guardian-1", "mingming",
		validPNGFixture(t, "source-reprocess-original"),
	)
	if err != nil {
		t.Fatalf("persist original ready PageAsset: %v", err)
	}
	prepared, created, err := coordinator.Create(
		context.Background(),
		CreateImageTaskInput{
			OwnerScope: "guardian-1", AgentName: "mingming", LearnerID: "learner-1",
			SourceKind: k12.ImageTaskSourceDesktop, SourceRef: "source-reprocess-message",
			SourceSessionID: "source-reprocess-session",
			SourceAssetRefs: []string{ready.Metadata.PageAssetID},
			MessageIntent:   "请批改这份作业", AttemptGeneration: 1,
			RouteRequest: k12.ImageTaskRouteSnapshot{
				Provider: "hexclaw-gpt", Model: k12.RecognizingPolicyModel,
				SelectionSource: "explicit", TimeoutMS: 30_000,
			},
		},
	)
	if err != nil || !created {
		t.Fatalf("create source reprocess ImageTask: created=%v err=%v", created, err)
	}
	view, err := coordinator.Run(
		context.Background(), "mingming", prepared.Dispatch.DispatchID,
	)
	if err != nil || view.Homework == nil || view.Homework.GradingJobID == "" {
		t.Fatalf("route source reprocess ImageTask to grading: view=%+v err=%v", view, err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := orchestrator.WaitForIdle(waitCtx); err != nil {
		t.Fatalf("wait initial grading recognition: %v", err)
	}
	jobID := view.Homework.GradingJobID
	jobView, err := deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil || jobView.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("initial grading stage=%v err=%v", jobView.Record.Status, err)
	}
	run, job := confirmSourceReprocessFixtureWithoutRun(t, orchestrator, jobID)
	return sourceReprocessIntegrationFixture{
		coordinator: coordinator, orchestrator: orchestrator,
		recognizer: recognizer, repository: repository,
		solver: solver, grader: grader,
		dispatchID: prepared.Dispatch.DispatchID, job: job, run: run,
	}
}

func float64Pointer(value float64) *float64 { return &value }

func confirmSourceReprocessFixtureWithoutRun(
	t *testing.T,
	o *GradingOrchestrator,
	jobID string,
) (*gradingRun, GradingJobView) {
	t.Helper()
	jobLock := o.jobLock(jobID)
	jobLock.Lock()
	defer jobLock.Unlock()
	run := o.lookup(jobID)
	if run == nil {
		t.Fatal("missing source reprocess grading runtime before confirmation")
	}
	candidate := *run
	candidate.questions = cloneRecognizedQuestions(run.questions)
	candidate.anchored = cloneRecognizedQuestions(run.anchored)
	confirmation := ConfirmPhotoGradingInput{
		Corrections: make([]GradingQuestionCorrection, 0, len(candidate.questions)),
	}
	for _, question := range candidate.questions {
		if NormalizeRecognizedQuestion(question).ConfirmationRequired {
			confirmation.Corrections = append(
				confirmation.Corrections,
				GradingQuestionCorrection{
					ProblemID: question.ProblemID, Confirmed: true,
				},
			)
		}
	}
	if err := applyAndValidateGradingConfirmation(&candidate, confirmation); err != nil {
		t.Fatalf("apply explicit source fixture confirmation: %v", err)
	}
	job, err := o.deps.GetGradingJob(context.Background(), run.agentName, jobID)
	if err != nil {
		t.Fatalf("get source fixture job before confirmation: %v", err)
	}
	confirmedFacts := candidate.questions
	if candidate.anchored != nil {
		confirmedFacts = candidate.anchored
	}
	if err := o.persistProblemAttemptFacts(
		context.Background(), run.agentName, job.Fields.SubmissionID, confirmedFacts,
	); err != nil {
		t.Fatalf("persist source fixture Problem/Attempt facts: %v", err)
	}
	if err := o.persistRun(jobID, &candidate); err != nil {
		t.Fatalf("persist source fixture confirmed runtime: %v", err)
	}
	if _, err := o.deps.ConfirmGradingJob(
		context.Background(), run.agentName, jobID,
		[]string{"canonical-recognition:" + CanonicalRecognizedQuestionsDigest(candidate.questions)},
	); err != nil {
		t.Fatalf("confirm source fixture grading job: %v", err)
	}
	run.questions = candidate.questions
	run.anchored = candidate.anchored
	job, err = o.deps.GetGradingJob(context.Background(), run.agentName, jobID)
	if err != nil || job.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("confirmed source fixture stage=%v err=%v", job.Record.Status, err)
	}
	return run, job
}

func (f sourceReprocessIntegrationFixture) commitRetake(
	t *testing.T,
) ProblemSourceActionResult {
	t.Helper()
	ready, err := f.repository.Persist(
		context.Background(), "guardian-1", "mingming",
		validPNGFixture(t, "source-reprocess-retake"),
	)
	if err != nil {
		t.Fatalf("persist retake ready PageAsset: %v", err)
	}
	affected := f.run.questions[0]
	result, err := f.coordinator.CommitProblemSourceAction(
		context.Background(),
		ProblemSourceActionCommand{
			OwnerScope: "guardian-1", TrustedAgentName: "mingming",
			DispatchID: f.dispatchID, ProblemID: affected.ProblemID,
			IdempotencyKey: "source-reprocess-retake-command", Action: "retake",
			StructureVersion: 1, ExpectedInputRevision: 1,
			Payload: []byte(fmt.Sprintf(
				`{"page_asset_id":%q}`, ready.Metadata.PageAssetID,
			)),
		},
	)
	if err != nil {
		t.Fatalf("commit retake source action: %v", err)
	}
	return result
}

func (f sourceReprocessIntegrationFixture) claimRetake(
	t *testing.T,
) k12storage.ProblemSourceReprocessJob {
	t.Helper()
	f.commitRetake(t)
	work, claimed, err := f.coordinator.Records.ClaimProblemSourceReprocessJob(
		context.Background(), "source-worker-integration", time.Now(), time.Minute,
	)
	if err != nil || !claimed {
		t.Fatalf("claim retake source work: claimed=%v err=%v", claimed, err)
	}
	return work
}

func (f sourceReprocessIntegrationFixture) claimCorrectText(
	t *testing.T,
	canonical string,
	idempotencyKey string,
) k12storage.ProblemSourceReprocessJob {
	t.Helper()
	affected := f.run.questions[0]
	if _, err := f.coordinator.Records.DB().Exec(`
		UPDATE k12_problems
		SET transcription_confidence=0.99,
		    confirmation_required=0,
		    confirmation_reasons_json='[]'
		WHERE agent_name='mingming' AND problem_id=?`, affected.ProblemID); err != nil {
		t.Fatalf("stabilize explicit correct_text fixture confidence: %v", err)
	}
	command, err := f.coordinator.CommitProblemSourceAction(
		context.Background(),
		ProblemSourceActionCommand{
			OwnerScope: "guardian-1", TrustedAgentName: "mingming",
			DispatchID: f.dispatchID, ProblemID: affected.ProblemID,
			IdempotencyKey: idempotencyKey,
			Action:         "correct_text", StructureVersion: 1, ExpectedInputRevision: 1,
			Payload: []byte(fmt.Sprintf(
				`{"question_canonical_markdown":%q}`, canonical,
			)),
		},
	)
	if err != nil {
		t.Fatalf("commit correct_text source action: %v", err)
	}
	work, claimed, err := f.coordinator.Records.ClaimProblemSourceReprocessJob(
		context.Background(), "source-worker-correct-text-closure", time.Now(), time.Minute,
	)
	if err != nil || !claimed || work.CommandReceiptID != command.CommandReceiptID {
		t.Fatalf("claim correct_text work=%+v claimed=%v err=%v", work, claimed, err)
	}
	return work
}

func assertSourceFixtureCurrentAttemptBinding(
	t *testing.T,
	fixture sourceReprocessIntegrationFixture,
	work k12storage.ProblemSourceReprocessJob,
) {
	t.Helper()
	questions, err := fixture.orchestrator.deps.loadCurrentConfirmedQuestions(
		context.Background(), work.AgentName, fixture.job.Fields.SubmissionID,
	)
	if err != nil {
		t.Fatalf("load current source questions: %v", err)
	}
	foundTarget := false
	for _, question := range questions {
		var confirmedVersion int
		var inputDigest string
		if err := fixture.coordinator.Records.DB().QueryRow(`
			SELECT confirmed_version,input_digest
			FROM k12_attempts
			WHERE agent_name=? AND attempt_id=? AND problem_id=?`,
			work.AgentName, question.AttemptID, question.ProblemID,
		).Scan(&confirmedVersion, &inputDigest); err != nil {
			t.Fatal(err)
		}
		if question.ConfirmedVersion != confirmedVersion || question.InputDigest != inputDigest {
			t.Fatalf(
				"current source/Attempt binding question=(v%d,%q) attempt=(v%d,%q)",
				question.ConfirmedVersion, question.InputDigest, confirmedVersion, inputDigest,
			)
		}
		foundTarget = foundTarget || question.ProblemID == work.ProblemID
	}
	if !foundTarget {
		t.Fatalf("current source question %s missing", work.ProblemID)
	}
}

func TestImageTaskProblemSourceRetakeCommitsV73AssessesExactSetAndReplaysWithoutResend(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{
		{
			Question: "new unrelated", SourceNumberPath: []string{"2"}, DisplayLabel: "2",
			Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "6",
			RecognitionConfidence:        float64Pointer(0.99),
			EvidenceTranscriptions:       []string{"new unrelated"},
			AnswerEvidenceTranscriptions: []string{"6"},
		},
		{
			Question: "retaken affected", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
			Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
			KnowledgePoints:              []string{"整数加法"},
			RecognitionConfidence:        float64Pointer(0.99),
			EvidenceTranscriptions:       []string{"retaken affected"},
			AnswerEvidenceTranscriptions: []string{"4"},
		},
	})
	work := fixture.claimRetake(t)
	if err := fixture.coordinator.ProcessProblemSourceReprocess(
		context.Background(), work,
	); err != nil {
		t.Fatalf("process retake source work: %v", err)
	}
	commit, err := fixture.coordinator.Records.GetProblemSourceRecognitionResultByWork(
		context.Background(), "guardian-1", work.WorkID,
	)
	if err != nil || commit.SourceInputRevision != 2 ||
		commit.ResultInputRevision != 3 || len(commit.Items) != 1 ||
		commit.Items[0].ProblemID != work.AffectedProblemIDs[0] ||
		commit.Items[0].QuestionCanonicalMarkdown != "retaken affected" ||
		commit.Items[0].Subject != "数学" {
		t.Fatalf("V73 source recognition commit=%+v err=%v", commit, err)
	}
	if fixture.solver.callCount("retaken affected") != 1 ||
		fixture.grader.callCount("retaken affected") != 1 {
		t.Fatalf(
			"affected assessment solve=%d grade=%d",
			fixture.solver.callCount("retaken affected"),
			fixture.grader.callCount("retaken affected"),
		)
	}
	if fixture.solver.callCount("new unrelated") != 0 ||
		fixture.grader.callCount("new unrelated") != 0 ||
		fixture.solver.callCount("unrelated") != 0 ||
		fixture.grader.callCount("unrelated") != 0 {
		t.Fatal("retake touched an unrelated problem")
	}
	receipt, err := fixture.coordinator.Records.GetGradingAssessmentItem(
		context.Background(), "mingming", work.JobID, work.AffectedProblemIDs[0],
	)
	if err != nil || receipt.InputRevision != 3 ||
		receipt.InputDigest != commit.Items[0].InputDigest {
		t.Fatalf("v3 affected assessment receipt=%+v err=%v", receipt, err)
	}
	beforeCalls, beforeSends := fixture.recognizer.counts()
	beforeAffectedSolve := fixture.solver.callCount("retaken affected")
	beforeAffectedGrade := fixture.grader.callCount("retaken affected")
	beforeUnrelatedSolve := fixture.solver.callCount("unrelated")
	beforeUnrelatedGrade := fixture.grader.callCount("unrelated")
	if err := fixture.coordinator.ProcessProblemSourceReprocess(
		context.Background(), work,
	); err != nil {
		t.Fatalf("replay committed source result: %v", err)
	}
	afterCalls, afterSends := fixture.recognizer.counts()
	if beforeCalls != 2 || beforeSends != 2 ||
		afterCalls != beforeCalls || afterSends != beforeSends {
		t.Fatalf(
			"committed replay resent OCR before=(%d,%d) after=(%d,%d)",
			beforeCalls, beforeSends, afterCalls, afterSends,
		)
	}
	if fixture.solver.callCount("retaken affected") != beforeAffectedSolve ||
		fixture.grader.callCount("retaken affected") != beforeAffectedGrade ||
		fixture.solver.callCount("unrelated") != beforeUnrelatedSolve ||
		fixture.grader.callCount("unrelated") != beforeUnrelatedGrade {
		t.Fatal("committed retake replay resent canonical solve/grade operations")
	}

	markedAt := time.Now().UTC()
	reconcileAt := markedAt.Add(time.Millisecond)
	if err := fixture.coordinator.Records.MarkProblemSourceReprocessOutcomeUnknown(
		context.Background(),
		work.Lease(),
		k12storage.ProblemSourceReprocessFailure{
			Code: "provider_outcome_unknown", Detail: "response receipt was lost",
			RetryAt: reconcileAt,
		},
		markedAt,
	); err != nil {
		t.Fatalf("park committed V73 work for reconciliation: %v", err)
	}
	worker := &ProblemSourceReprocessWorker{
		Records: fixture.coordinator.Records, Processor: fixture.coordinator,
		WorkerID: "source-reconcile-v73", LeaseDuration: time.Minute,
		HeartbeatInterval: 10 * time.Second,
		Now:               func() time.Time { return reconcileAt.Add(time.Millisecond) },
	}
	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("reconcile committed V73 work: processed=%v err=%v", processed, err)
	}
	reconciled, err := fixture.coordinator.Records.GetProblemSourceReprocessJob(
		context.Background(), "guardian-1", work.WorkID,
	)
	if err != nil || reconciled.Status != k12storage.ProblemSourceReprocessSucceeded ||
		reconciled.AttemptCount != work.AttemptCount ||
		reconciled.ReconciliationAttemptCount != 1 {
		t.Fatalf("reconciled V73 durable job=%+v err=%v", reconciled, err)
	}
	finalCalls, finalSends := fixture.recognizer.counts()
	if finalCalls != beforeCalls || finalSends != beforeSends {
		t.Fatalf(
			"V73 reconciliation resent OCR before=(%d,%d) after=(%d,%d)",
			beforeCalls, beforeSends, finalCalls, finalSends,
		)
	}
}

func assessSourceFixtureUnrelatedForFullCoverage(
	t *testing.T,
	fixture sourceReprocessIntegrationFixture,
) {
	t.Helper()
	unrelated := fixture.run.questions[1]
	if _, err := fixture.orchestrator.assessDurablePhotoItem(
		context.Background(),
		fixture.orchestrator.deps,
		fixture.job,
		fixture.run.req,
		PhotoModeGrade,
		unrelated,
	); err != nil {
		t.Fatalf("assess unrelated problem before source reprocess: %v", err)
	}
}

func TestCanonicalRunnerDoesNotUseStaleRuntimeBeforeRetakeV73Result(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "retaken current", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		RecognitionConfidence:        float64Pointer(0.99),
		EvidenceTranscriptions:       []string{"retaken current"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	fixture.commitRetake(t)
	beforeOCRCalls, beforeOCRSends := fixture.recognizer.counts()
	tips := &sourceCurrentTipsSpy{}
	restartedDeps := fixture.orchestrator.deps
	restartedDeps.TutoringTipsReview = tips
	restarted := trackGradingOrchestrator(
		t,
		NewGradingOrchestrator(
			restartedDeps,
			nil,
			WithGradingRunDir(fixture.orchestrator.runDir),
		),
	)

	view, _ := restarted.RunGradingJob(
		context.Background(), fixture.job.Record.RecordID,
	)
	if view.Record != nil && view.Record.Status == k12.GradingStageCompleted {
		t.Fatal("canonical runner completed before the V73 retake result existed")
	}
	persisted, err := restarted.deps.GetGradingJob(
		context.Background(), "mingming", fixture.job.Record.RecordID,
	)
	persistedStage := "missing"
	if persisted.Record != nil {
		persistedStage = persisted.Record.Status
	}
	if err != nil || persistedStage != k12.GradingStageAssessing {
		t.Fatalf(
			"canonical job must remain recoverable while V73 is pending: stage=%v err=%v",
			persistedStage,
			err,
		)
	}
	if _, err := fixture.coordinator.Records.GetCurrentGradingFinalArtifactByJob(
		context.Background(), "mingming", fixture.job.Record.RecordID,
	); err == nil {
		t.Fatal("canonical runner published a current-generation artifact before V73")
	}
	if fixture.solver.callCount("old affected") != 0 ||
		fixture.grader.callCount("old affected") != 0 ||
		fixture.solver.callCount("unrelated") != 0 ||
		fixture.grader.callCount("unrelated") != 0 {
		stage := "missing"
		if view.Record != nil {
			stage = view.Record.Status
		}
		t.Fatalf(
			"canonical runner sent stale pre-V73 facts affected=(%d,%d) unrelated=(%d,%d) stage=%s",
			fixture.solver.callCount("old affected"),
			fixture.grader.callCount("old affected"),
			fixture.solver.callCount("unrelated"),
			fixture.grader.callCount("unrelated"),
			stage,
		)
	}
	afterOCRCalls, afterOCRSends := fixture.recognizer.counts()
	if afterOCRCalls != beforeOCRCalls || afterOCRSends != beforeOCRSends {
		t.Fatalf(
			"canonical runner bypassed the source worker OCR before=(%d,%d) after=(%d,%d)",
			beforeOCRCalls, beforeOCRSends, afterOCRCalls, afterOCRSends,
		)
	}
	tips.mu.Lock()
	tipsCalls := len(tips.concepts)
	tips.mu.Unlock()
	if tipsCalls != 0 {
		t.Fatalf("canonical runner sent %d summary calls before V73", tipsCalls)
	}
	var aggregateInvocations, itemInvocations int
	if err := fixture.coordinator.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_model_invocations
		WHERE agent_name='mingming' AND job_id=? AND stage='assessing'`,
		fixture.job.Record.RecordID,
	).Scan(&aggregateInvocations); err != nil {
		t.Fatal(err)
	}
	if err := fixture.coordinator.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_grading_item_invocations
		WHERE agent_name='mingming' AND job_id=?`,
		fixture.job.Record.RecordID,
	).Scan(&itemInvocations); err != nil {
		t.Fatal(err)
	}
	if aggregateInvocations != 0 || itemInvocations != 0 {
		t.Fatalf(
			"pre-V73 gate wrote model ledgers aggregate=%d items=%d",
			aggregateInvocations,
			itemInvocations,
		)
	}
}

func TestRecoverGradingJobsParksAssessingJobWhileCurrentSourceRecognitionIsPending(
	t *testing.T,
) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "recovered retake", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		RecognitionConfidence:        float64Pointer(0.99),
		EvidenceTranscriptions:       []string{"recovered retake"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	fixture.commitRetake(t)
	before, err := fixture.orchestrator.deps.GetGradingJob(
		context.Background(), "mingming", fixture.job.Record.RecordID,
	)
	if err != nil || before.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("pre-recovery grading job=%+v err=%v", before, err)
	}
	beforeOCRCalls, beforeOCRSends := fixture.recognizer.counts()
	tips := &sourceCurrentTipsSpy{}
	restartedDeps := fixture.orchestrator.deps
	restartedDeps.TutoringTipsReview = tips
	restarted := trackGradingOrchestrator(
		t,
		NewGradingOrchestrator(
			restartedDeps,
			nil,
			WithGradingRunDir(fixture.orchestrator.runDir),
		),
	)

	logs := &sourceReprocessLogBuffer{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	defer slog.SetDefault(previousLogger)
	recovered, err := restarted.RecoverGradingJobs(
		context.Background(), []string{"mingming"},
	)
	if err != nil || recovered != 1 {
		t.Fatalf("recover pending-source grading jobs=%d err=%v", recovered, err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := restarted.WaitForIdle(waitCtx); err != nil {
		t.Fatalf("wait pending-source recovery: %v", err)
	}

	after, err := restarted.deps.GetGradingJob(
		context.Background(), "mingming", fixture.job.Record.RecordID,
	)
	if err != nil || after.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("post-recovery grading job=%+v err=%v", after, err)
	}
	if after.Fields.AttemptCount != before.Fields.AttemptCount ||
		after.Fields.FailureKind != before.Fields.FailureKind ||
		after.Fields.Retryable != before.Fields.Retryable ||
		after.Fields.FailedStage != before.Fields.FailedStage {
		t.Fatalf(
			"pending-source recovery mutated retry/failure state before=%+v after=%+v",
			before.Fields,
			after.Fields,
		)
	}
	if fixture.solver.callCount("old affected") != 0 ||
		fixture.grader.callCount("old affected") != 0 ||
		fixture.solver.callCount("unrelated") != 0 ||
		fixture.grader.callCount("unrelated") != 0 {
		t.Fatal("startup recovery sent pre-V73 solve/grade requests")
	}
	afterOCRCalls, afterOCRSends := fixture.recognizer.counts()
	if afterOCRCalls != beforeOCRCalls || afterOCRSends != beforeOCRSends {
		t.Fatalf(
			"startup recovery bypassed source worker OCR before=(%d,%d) after=(%d,%d)",
			beforeOCRCalls,
			beforeOCRSends,
			afterOCRCalls,
			afterOCRSends,
		)
	}
	tips.mu.Lock()
	tipsCalls := len(tips.concepts)
	tips.mu.Unlock()
	if tipsCalls != 0 {
		t.Fatalf("startup recovery sent %d summary calls before V73", tipsCalls)
	}
	var aggregateInvocations, itemInvocations int
	if err := fixture.coordinator.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_model_invocations
		WHERE agent_name='mingming' AND job_id=? AND stage='assessing'`,
		fixture.job.Record.RecordID,
	).Scan(&aggregateInvocations); err != nil {
		t.Fatal(err)
	}
	if err := fixture.coordinator.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_grading_item_invocations
		WHERE agent_name='mingming' AND job_id=?`,
		fixture.job.Record.RecordID,
	).Scan(&itemInvocations); err != nil {
		t.Fatal(err)
	}
	if aggregateInvocations != 0 || itemInvocations != 0 {
		t.Fatalf(
			"startup recovery wrote model ledgers aggregate=%d items=%d",
			aggregateInvocations,
			itemInvocations,
		)
	}
	jobLog := ""
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, fixture.job.Record.RecordID) {
			jobLog += line
		}
	}
	if strings.Contains(jobLog, "状态机已落库对应失败态") ||
		!strings.Contains(jobLog, "等待 V73") {
		t.Fatalf("pending-source recovery log misclassified as failure: %q", jobLog)
	}
}

func TestProblemSourceActionCannotCommitInsideCanonicalProviderRun(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "unused race retake", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		RecognitionConfidence:        float64Pointer(0.99),
		EvidenceTranscriptions:       []string{"unused race retake"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	providerEntered := make(chan struct{})
	providerRelease := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(providerRelease)
		}
	}()
	fixture.orchestrator.deps.Solver = &sourceActionRaceSolver{
		delegate: fixture.solver,
		problem:  "old affected",
		entered:  providerEntered,
		release:  providerRelease,
	}
	ready, err := fixture.repository.Persist(
		context.Background(),
		"guardian-1",
		"mingming",
		validPNGFixture(t, "source-action-vs-provider-race"),
	)
	if err != nil {
		t.Fatalf("persist race retake PageAsset: %v", err)
	}

	type runOutcome struct {
		view GradingJobView
		err  error
	}
	runDone := make(chan runOutcome, 1)
	go func() {
		view, runErr := fixture.orchestrator.RunGradingJob(
			context.Background(), fixture.job.Record.RecordID,
		)
		runDone <- runOutcome{view: view, err: runErr}
	}()
	select {
	case <-providerEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("canonical provider run did not reach controlled send boundary")
	}
	jobLock := fixture.orchestrator.jobLock(fixture.job.Record.RecordID)
	if jobLock.TryLock() {
		jobLock.Unlock()
		t.Fatal("canonical provider run did not hold the per-Job fence")
	}

	type commandOutcome struct {
		result ProblemSourceActionResult
		err    error
	}
	commandDone := make(chan commandOutcome, 1)
	go func() {
		result, commandErr := fixture.coordinator.CommitProblemSourceAction(
			context.Background(),
			ProblemSourceActionCommand{
				OwnerScope: "guardian-1", TrustedAgentName: "mingming",
				DispatchID:     fixture.dispatchID,
				ProblemID:      fixture.run.questions[0].ProblemID,
				IdempotencyKey: "source-action-provider-race-retake",
				Action:         "retake", StructureVersion: 1, ExpectedInputRevision: 1,
				Payload: []byte(fmt.Sprintf(
					`{"page_asset_id":%q}`, ready.Metadata.PageAssetID,
				)),
			},
		)
		commandDone <- commandOutcome{result: result, err: commandErr}
	}()
	select {
	case outcome := <-commandDone:
		t.Fatalf(
			"source action crossed the canonical provider fence before provider release: result=%+v err=%v",
			outcome.result,
			outcome.err,
		)
	case <-time.After(500 * time.Millisecond):
		// The provider entry channel establishes the ordering. This timeout is
		// only a deadlock bound: the command must be waiting on the same Job lock.
	}
	var receiptCount int
	if err := fixture.coordinator.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_problem_source_action_receipts
		WHERE owner_scope='guardian-1' AND idempotency_key=?`,
		"source-action-provider-race-retake",
	).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if receiptCount != 0 {
		t.Fatalf("source action committed %d V72 receipts inside provider run", receiptCount)
	}

	close(providerRelease)
	released = true
	select {
	case outcome := <-runDone:
		if outcome.err != nil || outcome.view.Record.Status != k12.GradingStageCompleted {
			t.Fatalf("canonical runner after provider release=%+v err=%v", outcome.view.Record, outcome.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canonical runner did not leave the fenced provider run")
	}
	select {
	case outcome := <-commandDone:
		if !errors.Is(outcome.err, k12storage.ErrProblemSourceActionConflict) {
			t.Fatalf("linearized late source action err=%v, want final-artifact conflict", outcome.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("source action did not resume after canonical Job released its fence")
	}
}

func TestProblemSourceActionFailsClosedWhenConfiguredGradingRuntimeHasNoFence(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "unused fence fixture", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		RecognitionConfidence:        float64Pointer(0.99),
		EvidenceTranscriptions:       []string{"unused fence fixture"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	fixture.coordinator.Grading = &imageTaskGradingStub{}
	const idempotencyKey = "source-action-missing-grading-fence"
	_, err := fixture.coordinator.CommitProblemSourceAction(
		context.Background(),
		ProblemSourceActionCommand{
			OwnerScope: "guardian-1", TrustedAgentName: "mingming",
			DispatchID:     fixture.dispatchID,
			ProblemID:      fixture.run.questions[0].ProblemID,
			IdempotencyKey: idempotencyKey, Action: "correct_text",
			StructureVersion: 1, ExpectedInputRevision: 1,
			Payload: []byte(`{"question_canonical_markdown":"must not commit"}`),
		},
	)
	if !errors.Is(err, ErrProblemSourceActionFenceUnavailable) {
		t.Fatalf("missing grading fence err=%v, want fail-closed", err)
	}
	var receiptCount int
	if err := fixture.coordinator.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_problem_source_action_receipts
		WHERE owner_scope='guardian-1' AND idempotency_key=?`,
		idempotencyKey,
	).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if receiptCount != 0 {
		t.Fatalf("missing grading fence committed %d source action receipts", receiptCount)
	}
}

func TestProblemSourcePartialCoverageLeavesJobAssessingWithoutUnrelatedCalls(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "unused retake batch", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		KnowledgePoints:              []string{"整数加法"},
		RecognitionConfidence:        float64Pointer(0.99),
		EvidenceTranscriptions:       []string{"unused retake batch"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	work := fixture.claimCorrectText(
		t, "corrected affected partial", "source-reprocess-partial-closure",
	)
	assertSourceFixtureCurrentAttemptBinding(t, fixture, work)

	if err := fixture.coordinator.ProcessProblemSourceReprocess(
		context.Background(), work,
	); err != nil {
		t.Fatalf("process partial source coverage: %v", err)
	}
	current, err := fixture.orchestrator.deps.GetGradingJob(
		context.Background(), "mingming", work.JobID,
	)
	if err != nil || current.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("partial source coverage job=%+v err=%v, want assessing", current, err)
	}
	if _, err := fixture.coordinator.Records.GetCurrentGradingFinalArtifactByJob(
		context.Background(), "mingming", work.JobID,
	); err == nil {
		t.Fatal("partial source coverage published a final artifact")
	}
	if fixture.solver.callCount("corrected affected partial") != 1 ||
		fixture.grader.callCount("corrected affected partial") != 1 ||
		fixture.solver.callCount("unrelated") != 0 ||
		fixture.grader.callCount("unrelated") != 0 {
		t.Fatalf(
			"source worker calls affected=(%d,%d) unrelated=(%d,%d), want affected one each and unrelated zero",
			fixture.solver.callCount("corrected affected partial"),
			fixture.grader.callCount("corrected affected partial"),
			fixture.solver.callCount("unrelated"),
			fixture.grader.callCount("unrelated"),
		)
	}
}

func TestProblemSourceFullCoverageCompletesCanonicalJobAndRestartReplayDoesNotResend(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "unused retake batch", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		KnowledgePoints:              []string{"整数加法"},
		RecognitionConfidence:        float64Pointer(0.99),
		EvidenceTranscriptions:       []string{"unused retake batch"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	assessSourceFixtureUnrelatedForFullCoverage(t, fixture)
	work := fixture.claimCorrectText(
		t, "corrected affected full", "source-reprocess-full-closure",
	)
	assertSourceFixtureCurrentAttemptBinding(t, fixture, work)

	if err := fixture.coordinator.ProcessProblemSourceReprocess(
		context.Background(), work,
	); err != nil {
		t.Fatalf("process full source coverage: %v", err)
	}
	completed, err := fixture.orchestrator.deps.GetGradingJob(
		context.Background(), "mingming", work.JobID,
	)
	if err != nil || completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("canonical grading job after full source coverage=%+v err=%v", completed, err)
	}

	beforeSolverAffected := fixture.solver.callCount("corrected affected full")
	beforeGraderAffected := fixture.grader.callCount("corrected affected full")
	beforeSolverUnrelated := fixture.solver.callCount("unrelated")
	beforeGraderUnrelated := fixture.grader.callCount("unrelated")
	beforeOCRCalls, beforeOCRSends := fixture.recognizer.counts()
	restarted := trackGradingOrchestrator(
		t,
		NewGradingOrchestrator(
			fixture.orchestrator.deps,
			nil,
			WithGradingRunDir(fixture.orchestrator.runDir),
		),
	)
	if err := restarted.ProcessProblemSourceReprocess(context.Background(), work); err != nil {
		t.Fatalf("replay completed source work after restart: %v", err)
	}
	replayed, err := restarted.RunGradingJob(context.Background(), work.JobID)
	if err != nil || replayed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("replay canonical completed job after restart=%+v err=%v", replayed.Record, err)
	}
	if fixture.solver.callCount("corrected affected full") != beforeSolverAffected ||
		fixture.grader.callCount("corrected affected full") != beforeGraderAffected ||
		fixture.solver.callCount("unrelated") != beforeSolverUnrelated ||
		fixture.grader.callCount("unrelated") != beforeGraderUnrelated {
		t.Fatal("restart replay resent a solve or grade provider operation")
	}
	afterOCRCalls, afterOCRSends := fixture.recognizer.counts()
	if afterOCRCalls != beforeOCRCalls || afterOCRSends != beforeOCRSends {
		t.Fatalf(
			"restart replay resent OCR before=(%d,%d) after=(%d,%d)",
			beforeOCRCalls, beforeOCRSends, afterOCRCalls, afterOCRSends,
		)
	}
	var artifactCount int
	if err := fixture.coordinator.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_grading_final_artifacts WHERE job_id=?`,
		work.JobID,
	).Scan(&artifactCount); err != nil {
		t.Fatal(err)
	}
	if artifactCount != 1 {
		t.Fatalf("restart replay final artifacts=%d, want 1", artifactCount)
	}
}

func TestProblemSourceRetakeFinalTutoringTipsUseCurrentV73Facts(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "retaken affected current", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		KnowledgePoints:              []string{"分数乘法"},
		RecognitionConfidence:        float64Pointer(0.99),
		EvidenceTranscriptions:       []string{"retaken affected current"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	tips := &sourceCurrentTipsSpy{}
	fixture.orchestrator.deps.TutoringTipsReview = tips
	assessSourceFixtureUnrelatedForFullCoverage(t, fixture)
	work := fixture.claimRetake(t)
	if err := fixture.coordinator.ProcessProblemSourceReprocess(
		context.Background(), work,
	); err != nil {
		t.Fatalf("process retake through finalizer: %v", err)
	}
	artifact, err := fixture.coordinator.Records.GetGradingFinalArtifactByJob(
		context.Background(), "mingming", work.JobID,
	)
	if err != nil {
		t.Fatalf("load retake final artifact: %v", err)
	}
	if !strings.Contains(artifact.CanonicalMarkdown, "retaken affected current") ||
		strings.Contains(artifact.CanonicalMarkdown, "old affected") {
		t.Fatalf("retake final artifact mixed source revisions:\n%s", artifact.CanonicalMarkdown)
	}
	if !tips.saw("分数乘法") {
		t.Fatalf("retake tutoring tips concepts=%v, want current V73 concept", tips.concepts)
	}
}

func TestProblemSourceCorrectTextFinalTutoringTipsUseCurrentV72Stem(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "unused retake batch", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		KnowledgePoints: []string{"整数加法"},
	}})
	fixture.orchestrator.deps.TutoringTipsReview = &sourceCurrentTipsSpy{}
	assessSourceFixtureUnrelatedForFullCoverage(t, fixture)
	affected := fixture.run.questions[0]
	if _, err := fixture.coordinator.Records.DB().Exec(`
		UPDATE k12_problems
		SET transcription_confidence=0.99,
		    confirmation_required=0,
		    confirmation_reasons_json='[]'
		WHERE agent_name='mingming' AND problem_id=?`, affected.ProblemID); err != nil {
		t.Fatalf("stabilize explicit correct_text fixture confidence: %v", err)
	}
	command, err := fixture.coordinator.CommitProblemSourceAction(
		context.Background(),
		ProblemSourceActionCommand{
			OwnerScope: "guardian-1", TrustedAgentName: "mingming",
			DispatchID: fixture.dispatchID, ProblemID: affected.ProblemID,
			IdempotencyKey: "source-reprocess-correct-text-current-tips",
			Action:         "correct_text", StructureVersion: 1, ExpectedInputRevision: 1,
			Payload: []byte(`{"question_canonical_markdown":"corrected affected current"}`),
		},
	)
	if err != nil {
		t.Fatalf("commit correct_text source action: %v", err)
	}
	work, claimed, err := fixture.coordinator.Records.ClaimProblemSourceReprocessJob(
		context.Background(), "source-worker-correct-text-tips", time.Now(), time.Minute,
	)
	if err != nil || !claimed || work.CommandReceiptID != command.CommandReceiptID {
		t.Fatalf("claim correct_text work=%+v claimed=%v err=%v", work, claimed, err)
	}
	if err := fixture.coordinator.ProcessProblemSourceReprocess(
		context.Background(), work,
	); err != nil {
		t.Fatalf("process correct_text through finalizer: %v", err)
	}
	artifact, err := fixture.coordinator.Records.GetGradingFinalArtifactByJob(
		context.Background(), "mingming", work.JobID,
	)
	if err != nil {
		t.Fatalf("load correct_text final artifact: %v", err)
	}
	tipsMarker := "# 这份作业的辅导要点"
	tipsOffset := strings.Index(artifact.CanonicalMarkdown, tipsMarker)
	if tipsOffset < 0 {
		t.Fatalf("correct_text final artifact missing tutoring tips section:\n%s", artifact.CanonicalMarkdown)
	}
	tipsMarkdown := artifact.CanonicalMarkdown[tipsOffset:]
	if !strings.Contains(tipsMarkdown, "corrected affected current") ||
		strings.Contains(tipsMarkdown, "old affected") {
		t.Fatalf("correct_text final artifact mixed source revisions:\n%s", artifact.CanonicalMarkdown)
	}
}

func TestImageTaskProblemSourceOutcomeUnknownReconciliationNeverResendsPhysicalCall(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "ambiguous affected", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		RecognitionConfidence:        float64Pointer(0.99),
		EvidenceTranscriptions:       []string{"ambiguous affected"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	fixture.recognizer.mu.Lock()
	fixture.recognizer.sendErrors = []error{nil, context.DeadlineExceeded}
	fixture.recognizer.mu.Unlock()
	command := fixture.commitRetake(t)

	now := time.Now().UTC()
	worker := &ProblemSourceReprocessWorker{
		Records: fixture.coordinator.Records, Processor: fixture.coordinator,
		WorkerID: "source-worker-outcome-unknown", LeaseDuration: time.Minute,
		HeartbeatInterval: 10 * time.Second,
		Now:               func() time.Time { return now },
	}
	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("run ambiguous physical source work: processed=%v err=%v", processed, err)
	}
	var workID string
	if err := fixture.coordinator.Records.DB().QueryRow(`
		SELECT work_id
		FROM k12_problem_source_reprocess_jobs
		WHERE command_receipt_id=?`, command.CommandReceiptID,
	).Scan(&workID); err != nil {
		t.Fatalf("resolve source work from committed receipt: %v", err)
	}
	unknown, err := fixture.coordinator.Records.GetProblemSourceReprocessJob(
		context.Background(), "guardian-1", workID,
	)
	if err != nil || unknown.Status != k12storage.ProblemSourceReprocessOutcomeUnknown ||
		unknown.FailureCode != "provider_outcome_unknown" ||
		unknown.NextReconcileAtMilli != now.Add(defaultProblemSourceReprocessReconcileGrace).UnixMilli() {
		t.Fatalf("ambiguous durable source job=%+v err=%v", unknown, err)
	}
	beforeCalls, beforeSends := fixture.recognizer.counts()
	if beforeCalls != 2 || beforeSends != 2 {
		t.Fatalf("initial physical calls=(%d,%d), want initial+retake", beforeCalls, beforeSends)
	}

	now = time.UnixMilli(unknown.NextReconcileAtMilli).Add(time.Millisecond)
	processed, err = worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("reconcile ambiguous physical source work: processed=%v err=%v", processed, err)
	}
	resolved, err := fixture.coordinator.Records.GetProblemSourceReprocessJob(
		context.Background(), "guardian-1", workID,
	)
	if err != nil || resolved.Status != k12storage.ProblemSourceReprocessNeedsConfirmation ||
		resolved.FailureCode != "provider_outcome_unresolved" ||
		resolved.AttemptCount != unknown.AttemptCount ||
		resolved.ReconciliationAttemptCount != 1 {
		t.Fatalf("terminal unresolved source job=%+v err=%v", resolved, err)
	}
	afterCalls, afterSends := fixture.recognizer.counts()
	if afterCalls != beforeCalls || afterSends != beforeSends {
		t.Fatalf(
			"outcome_unknown reconciliation resent physical call before=(%d,%d) after=(%d,%d)",
			beforeCalls, beforeSends, afterCalls, afterSends,
		)
	}
}

func TestImageTaskProblemSourceOutcomeUnknownWithoutPhysicalLedgerFailsClosedWithoutSend(
	t *testing.T,
) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "must not be sent", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		RecognitionConfidence:        float64Pointer(0.99),
		EvidenceTranscriptions:       []string{"must not be sent"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	work := fixture.claimRetake(t)
	markedAt := time.Now().UTC()
	reconcileAt := markedAt.Add(time.Millisecond)
	if err := fixture.coordinator.Records.MarkProblemSourceReprocessOutcomeUnknown(
		context.Background(),
		work.Lease(),
		k12storage.ProblemSourceReprocessFailure{
			Code: "provider_outcome_unknown", Detail: "legacy missing physical ledger",
			RetryAt: reconcileAt,
		},
		markedAt,
	); err != nil {
		t.Fatalf("mark outcome_unknown without physical ledger: %v", err)
	}
	beforeCalls, beforeSends := fixture.recognizer.counts()
	worker := &ProblemSourceReprocessWorker{
		Records: fixture.coordinator.Records, Processor: fixture.coordinator,
		WorkerID: "source-reconcile-missing-ledger", LeaseDuration: time.Minute,
		HeartbeatInterval: 10 * time.Second,
		Now:               func() time.Time { return reconcileAt.Add(time.Millisecond) },
	}
	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("reconcile missing physical ledger: processed=%v err=%v", processed, err)
	}
	resolved, err := fixture.coordinator.Records.GetProblemSourceReprocessJob(
		context.Background(), "guardian-1", work.WorkID,
	)
	if err != nil || resolved.Status != k12storage.ProblemSourceReprocessNeedsConfirmation ||
		resolved.FailureCode != "provider_outcome_unresolved" {
		t.Fatalf("missing-ledger reconciliation=%+v err=%v", resolved, err)
	}
	afterCalls, afterSends := fixture.recognizer.counts()
	if afterCalls != beforeCalls || afterSends != beforeSends {
		t.Fatalf(
			"missing-ledger reconciliation sent provider call before=(%d,%d) after=(%d,%d)",
			beforeCalls,
			beforeSends,
			afterCalls,
			afterSends,
		)
	}
	if _, err := fixture.coordinator.Records.GetProblemSourceRecognitionResultByWork(
		context.Background(), "guardian-1", work.WorkID,
	); !errors.Is(err, k12storage.ErrProblemSourceRecognitionNotFound) {
		t.Fatalf("missing-ledger reconciliation invented V73 result: %v", err)
	}
}

func TestImageTaskProblemSourceRetakeRiskParksBeforeAssessment(t *testing.T) {
	fixture := newSourceReprocessIntegrationFixture(t, []RecognizedQuestion{{
		Question: "low confidence affected", SourceNumberPath: []string{"1"}, DisplayLabel: "1",
		Subject: "数学", AnswerState: AnswerStatePresent, StudentAnswer: "4",
		RecognitionConfidence:        float64Pointer(0.42),
		EvidenceTranscriptions:       []string{"low confidence affected"},
		AnswerEvidenceTranscriptions: []string{"4"},
	}})
	work := fixture.claimRetake(t)
	err := fixture.coordinator.ProcessProblemSourceReprocess(context.Background(), work)
	var confirmation *ProblemSourceReprocessNeedsConfirmationError
	if !errors.As(err, &confirmation) ||
		confirmation.Code != "source_risk_requires_confirmation" {
		t.Fatalf("risky retake err=%v confirmation=%+v", err, confirmation)
	}
	facts, err := fixture.coordinator.Records.ListCurrentProblemSourceRecognitionFacts(
		context.Background(), "mingming", fixture.job.Fields.SubmissionID,
	)
	fact := facts[work.AffectedProblemIDs[0]]
	if err != nil || !fact.ConfirmationRequired ||
		fact.InputRevision != 3 || fact.Subject != "数学" {
		t.Fatalf("current risky V73 fact=%+v err=%v", fact, err)
	}
	if fixture.solver.callCount("low confidence affected") != 0 ||
		fixture.grader.callCount("low confidence affected") != 0 {
		t.Fatal("risky source recognition reached assessment providers")
	}
	if _, err := fixture.coordinator.Records.GetGradingAssessmentItem(
		context.Background(), "mingming", work.JobID, work.AffectedProblemIDs[0],
	); err == nil {
		t.Fatal("risky source recognition published an assessment receipt")
	}
}
