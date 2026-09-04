package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const (
	defaultProblemSourceReprocessLease          = 3 * time.Minute
	defaultProblemSourceReprocessHeartbeat      = 30 * time.Second
	defaultProblemSourceReprocessPoll           = time.Second
	defaultProblemSourceReprocessMaxAttempts    = 5
	defaultProblemSourceReprocessReconcileGrace = 30 * time.Second
	problemSourceReprocessCommitTimeout         = 5 * time.Second
)

var errProblemSourceReprocessProcessorFinished = errors.New(
	"problem source reprocess processor finished",
)

var errProblemSourceReprocessQuiesced = errors.New(
	"problem source reprocess worker quiesced",
)

type problemSourceReprocessPauseFence struct{}
type problemSourceReconciliationOnlyContextKey struct{}

func withProblemSourceReconciliationOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, problemSourceReconciliationOnlyContextKey{}, true)
}

func problemSourceReconciliationOnly(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(problemSourceReconciliationOnlyContextKey{}).(bool)
	return value
}

// ProblemSourceReprocessQueue is the lease/fencing boundary used by the
// application worker. Provider work never executes inside a SQLite
// transaction; every terminal mutation must still present the current
// owner+epoch lease.
type ProblemSourceReprocessQueue interface {
	ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
		context.Context, string, time.Time, time.Duration,
	) (k12storage.ProblemSourceReprocessJob, bool, error)
	HeartbeatProblemSourceReprocessOutcomeUnknownReconciliation(
		context.Context,
		k12storage.ProblemSourceReprocessReconciliationLease,
		time.Time,
		time.Duration,
	) (k12storage.ProblemSourceReprocessJob, error)
	ResolveProblemSourceReprocessOutcomeUnknown(
		context.Context,
		k12storage.ProblemSourceReprocessReconciliationLease,
		k12storage.ProblemSourceReprocessOutcomeUnknownResolution,
		k12storage.ProblemSourceReprocessFailure,
		time.Time,
	) (k12storage.ProblemSourceReprocessJob, error)
	ReleaseProblemSourceReprocessOutcomeUnknownReconciliation(
		context.Context,
		k12storage.ProblemSourceReprocessReconciliationLease,
		time.Time,
	) error
	ClaimProblemSourceReprocessJob(
		context.Context, string, time.Time, time.Duration,
	) (k12storage.ProblemSourceReprocessJob, bool, error)
	HeartbeatProblemSourceReprocessJob(
		context.Context, k12storage.ProblemSourceReprocessLease, time.Time, time.Duration,
	) (k12storage.ProblemSourceReprocessJob, error)
	ReleaseProblemSourceReprocessJob(
		context.Context, k12storage.ProblemSourceReprocessLease, time.Time,
	) error
	CompleteProblemSourceReprocessSucceeded(
		context.Context, k12storage.ProblemSourceReprocessLease, time.Time,
	) error
	CompleteProblemSourceReprocessNeedsConfirmation(
		context.Context,
		k12storage.ProblemSourceReprocessLease,
		k12storage.ProblemSourceReprocessFailure,
		time.Time,
	) error
	FailProblemSourceReprocessRetryable(
		context.Context,
		k12storage.ProblemSourceReprocessLease,
		k12storage.ProblemSourceReprocessFailure,
		time.Time,
	) error
	MarkProblemSourceReprocessOutcomeUnknown(
		context.Context,
		k12storage.ProblemSourceReprocessLease,
		k12storage.ProblemSourceReprocessFailure,
		time.Time,
	) error
}

// ProblemSourceReprocessProcessor owns domain execution only. It must reuse
// the canonical OCR/assessment physical-call ledgers rather than sending to a
// provider directly.
type ProblemSourceReprocessProcessor interface {
	ProcessProblemSourceReprocess(
		context.Context,
		k12storage.ProblemSourceReprocessJob,
	) error
}

// ProblemSourceReprocessNeedsConfirmationError is a fail-closed result: the
// worker can prove that automatic processing is unsafe, so the durable job is
// parked for an explicit parent decision instead of retried or guessed.
type ProblemSourceReprocessNeedsConfirmationError struct {
	Code   string
	Detail string
}

func (e *ProblemSourceReprocessNeedsConfirmationError) Error() string {
	if e == nil {
		return "problem source reprocess needs confirmation"
	}
	return strings.TrimSpace(e.Detail)
}

// ProblemSourceReprocessWorker is a process-local scheduler around the durable
// queue. Correctness does not depend on its goroutine state: restart recovery
// claims prepared/queued/due/expired work again, while storage fencing rejects
// every stale worker mutation.
type ProblemSourceReprocessWorker struct {
	Records     ProblemSourceReprocessQueue
	Processor   ProblemSourceReprocessProcessor
	WorkerID    string
	BaseContext context.Context

	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	MaxAttempts       int
	Now               func() time.Time

	mu          sync.Mutex
	runCtx      context.Context
	cancel      context.CancelFunc
	active      bool
	rerun       bool
	sealed      bool
	idle        chan struct{}
	drainCancel context.CancelCauseFunc
	pauseFence  *problemSourceReprocessPauseFence
	pauseRefs   int
	polling     bool
	pollDone    chan struct{}
}

func (w *ProblemSourceReprocessWorker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *ProblemSourceReprocessWorker) defaults() (string, time.Duration, time.Duration, int, error) {
	if w == nil || w.Records == nil || w.Processor == nil {
		return "", 0, 0, 0, fmt.Errorf("usecase: problem source reprocess worker is not configured")
	}
	workerID := strings.TrimSpace(w.WorkerID)
	if workerID == "" {
		w.mu.Lock()
		if strings.TrimSpace(w.WorkerID) == "" {
			w.WorkerID = "source-reprocess-" + idgen.ShortID()
		}
		workerID = w.WorkerID
		w.mu.Unlock()
	}
	leaseDuration := w.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = defaultProblemSourceReprocessLease
	}
	heartbeatInterval := w.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultProblemSourceReprocessHeartbeat
	}
	if heartbeatInterval >= leaseDuration {
		return "", 0, 0, 0, fmt.Errorf(
			"usecase: source reprocess heartbeat must be shorter than its lease",
		)
	}
	maxAttempts := w.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultProblemSourceReprocessMaxAttempts
	}
	return workerID, leaseDuration, heartbeatInterval, maxAttempts, nil
}

// RunOnce claims and settles at most one durable job. A processor failure is
// returned through the durable job state rather than as a scheduler error;
// only claim/heartbeat/fencing/transition failures are returned to the caller.
func (w *ProblemSourceReprocessWorker) RunOnce(ctx context.Context) (bool, error) {
	workerID, leaseDuration, heartbeatInterval, maxAttempts, err := w.defaults()
	if err != nil {
		return false, err
	}
	w.mu.Lock()
	pausedOrSealed := w.pauseFence != nil || w.sealed
	w.mu.Unlock()
	if pausedOrSealed {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	claimedAt := w.now()
	unknown, claimed, err := w.Records.ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
		ctx, workerID, claimedAt, leaseDuration,
	)
	if err != nil {
		return false, err
	}
	if claimed {
		return w.reconcileOutcomeUnknown(
			ctx, unknown, leaseDuration, heartbeatInterval,
		)
	}

	job, claimed, err := w.Records.ClaimProblemSourceReprocessJob(
		ctx, workerID, claimedAt, leaseDuration,
	)
	if err != nil || !claimed {
		return false, err
	}
	startedAt := time.Now()
	jobRef := shortSHA1([]byte(job.WorkID))
	slog.Info("K12 source reprocess job started",
		"job_ref", jobRef,
		"status", "started",
		"stage", "processing",
		"elapsed_ms", int64(0),
		"attempt", job.AttemptCount)
	lastHeartbeatLog := time.Now()
	logResult := func(status, stage, failureCode string, resultErr error) {
		args := []any{
			"job_ref", jobRef,
			"status", status,
			"stage", stage,
			"elapsed_ms", time.Since(startedAt).Milliseconds(),
			"attempt", job.AttemptCount,
		}
		if failureCode != "" {
			args = append(args, "failure_code", failureCode)
		}
		if resultErr != nil {
			args = append(args, "error_type", fmt.Sprintf("%T", resultErr))
		}
		slog.Info("K12 source reprocess job finished", args...)
	}

	processErr, heartbeatErr := w.processWithHeartbeat(
		ctx,
		heartbeatInterval,
		func(heartbeatCtx context.Context) error {
			_, heartbeatErr := w.Records.HeartbeatProblemSourceReprocessJob(
				heartbeatCtx, job.Lease(), w.now(), leaseDuration,
			)
			if heartbeatErr == nil && time.Since(lastHeartbeatLog) >= 30*time.Second {
				lastHeartbeatLog = time.Now()
				slog.Info("K12 source reprocess job heartbeat",
					"job_ref", jobRef,
					"status", "running",
					"stage", "processing",
					"elapsed_ms", time.Since(startedAt).Milliseconds(),
					"attempt", job.AttemptCount)
			}
			return heartbeatErr
		},
		func(processCtx context.Context) error {
			return w.Processor.ProcessProblemSourceReprocess(processCtx, job)
		},
	)
	if heartbeatErr != nil {
		if problemSourceReprocessLifecycleInterrupted(ctx, heartbeatErr) {
			commitCtx, cancelCommit := context.WithTimeout(
				context.WithoutCancel(ctx), problemSourceReprocessCommitTimeout,
			)
			defer cancelCommit()
			resultErr := w.Records.ReleaseProblemSourceReprocessJob(
				commitCtx, job.Lease(), w.now(),
			)
			logResult("released", "lease_released", "", resultErr)
			return true, resultErr
		}
		// The old worker no longer has authority. In particular, never translate
		// a fencing loss into a second terminal mutation.
		logResult("failed", "heartbeat", "heartbeat_failed", heartbeatErr)
		return true, heartbeatErr
	}

	settledAt := w.now()
	commitCtx, cancelCommit := context.WithTimeout(
		context.WithoutCancel(ctx), problemSourceReprocessCommitTimeout,
	)
	defer cancelCommit()
	if processErr == nil {
		resultErr := w.Records.CompleteProblemSourceReprocessSucceeded(
			commitCtx, job.Lease(), settledAt,
		)
		logResult("succeeded", "completed", "", resultErr)
		return true, resultErr
	}

	failure := problemSourceReprocessFailure(processErr)
	var confirmation *ProblemSourceReprocessNeedsConfirmationError
	switch {
	case errors.As(processErr, &confirmation):
		if code := strings.TrimSpace(confirmation.Code); code != "" {
			failure.Code = boundedProblemSourceReprocessText(code, 128)
		}
		resultErr := w.Records.CompleteProblemSourceReprocessNeedsConfirmation(
			commitCtx, job.Lease(), failure, settledAt,
		)
		logResult("needs_confirmation", "parked", failure.Code, resultErr)
		return true, resultErr
	case errors.Is(processErr, ErrGradingPhysicalCallOutcomeUnknown),
		errors.Is(processErr, ErrRecognitionPhysicalCallOutcomeUnknown),
		errors.Is(processErr, ErrRecognitionPhysicalCallObservedInFlight),
		errors.Is(processErr, ErrModelInvocationRequiresReconciliation):
		failure.Code = "provider_outcome_unknown"
		failure.RetryAt = settledAt.Add(defaultProblemSourceReprocessReconcileGrace)
		resultErr := w.Records.MarkProblemSourceReprocessOutcomeUnknown(
			commitCtx, job.Lease(), failure, settledAt,
		)
		logResult("outcome_unknown", "reconciliation_scheduled", failure.Code, resultErr)
		return true, resultErr
	case problemSourceReprocessLifecycleInterrupted(ctx, processErr):
		resultErr := w.Records.ReleaseProblemSourceReprocessJob(
			commitCtx, job.Lease(), settledAt,
		)
		logResult("released", "lease_released", "", resultErr)
		return true, resultErr
	case job.AttemptCount >= maxAttempts:
		failure.Code = "automatic_attempts_exhausted"
		resultErr := w.Records.CompleteProblemSourceReprocessNeedsConfirmation(
			commitCtx, job.Lease(), failure, settledAt,
		)
		logResult("needs_confirmation", "attempts_exhausted", failure.Code, resultErr)
		return true, resultErr
	default:
		failure.RetryAt = settledAt.Add(problemSourceReprocessRetryDelay(job.AttemptCount))
		resultErr := w.Records.FailProblemSourceReprocessRetryable(
			commitCtx, job.Lease(), failure, settledAt,
		)
		logResult("retry_wait", "retry_scheduled", failure.Code, resultErr)
		return true, resultErr
	}
}

func (w *ProblemSourceReprocessWorker) reconcileOutcomeUnknown(
	ctx context.Context,
	job k12storage.ProblemSourceReprocessJob,
	leaseDuration time.Duration,
	heartbeatInterval time.Duration,
) (bool, error) {
	startedAt := time.Now()
	jobRef := shortSHA1([]byte(job.WorkID))
	attempt := job.ReconciliationAttemptCount
	slog.Info("K12 source reprocess reconciliation started",
		"job_ref", jobRef,
		"status", "started",
		"stage", "reconciling",
		"elapsed_ms", int64(0),
		"attempt", attempt)
	lastHeartbeatLog := time.Now()
	logResult := func(status, stage, failureCode string, resultErr error) {
		args := []any{
			"job_ref", jobRef,
			"status", status,
			"stage", stage,
			"elapsed_ms", time.Since(startedAt).Milliseconds(),
			"attempt", attempt,
		}
		if failureCode != "" {
			args = append(args, "failure_code", failureCode)
		}
		if resultErr != nil {
			args = append(args, "error_type", fmt.Sprintf("%T", resultErr))
		}
		slog.Info("K12 source reprocess reconciliation finished", args...)
	}
	processErr, heartbeatErr := w.processWithHeartbeat(
		ctx,
		heartbeatInterval,
		func(heartbeatCtx context.Context) error {
			_, heartbeatErr := w.Records.HeartbeatProblemSourceReprocessOutcomeUnknownReconciliation(
				heartbeatCtx, job.ReconciliationLease(), w.now(), leaseDuration,
			)
			if heartbeatErr == nil && time.Since(lastHeartbeatLog) >= 30*time.Second {
				lastHeartbeatLog = time.Now()
				slog.Info("K12 source reprocess reconciliation heartbeat",
					"job_ref", jobRef,
					"status", "running",
					"stage", "reconciling",
					"elapsed_ms", time.Since(startedAt).Milliseconds(),
					"attempt", attempt)
			}
			return heartbeatErr
		},
		func(processCtx context.Context) error {
			return w.Processor.ProcessProblemSourceReprocess(
				withProblemSourceReconciliationOnly(processCtx),
				job,
			)
		},
	)
	if heartbeatErr != nil {
		if problemSourceReprocessLifecycleInterrupted(ctx, heartbeatErr) {
			commitCtx, cancelCommit := context.WithTimeout(
				context.WithoutCancel(ctx), problemSourceReprocessCommitTimeout,
			)
			defer cancelCommit()
			resultErr := w.Records.ReleaseProblemSourceReprocessOutcomeUnknownReconciliation(
				commitCtx, job.ReconciliationLease(), w.now(),
			)
			logResult("released", "lease_released", "", resultErr)
			return true, resultErr
		}
		logResult("failed", "heartbeat", "heartbeat_failed", heartbeatErr)
		return true, heartbeatErr
	}

	settledAt := w.now()
	commitCtx, cancelCommit := context.WithTimeout(
		context.WithoutCancel(ctx), problemSourceReprocessCommitTimeout,
	)
	defer cancelCommit()
	if processErr == nil {
		_, err := w.Records.ResolveProblemSourceReprocessOutcomeUnknown(
			commitCtx,
			job.ReconciliationLease(),
			k12storage.ProblemSourceReprocessOutcomeUnknownResolutionSucceeded,
			k12storage.ProblemSourceReprocessFailure{},
			settledAt,
		)
		logResult("succeeded", "reconciled", "", err)
		return true, err
	}
	if problemSourceReprocessLifecycleInterrupted(ctx, processErr) {
		resultErr := w.Records.ReleaseProblemSourceReprocessOutcomeUnknownReconciliation(
			commitCtx, job.ReconciliationLease(), settledAt,
		)
		logResult("released", "lease_released", "", resultErr)
		return true, resultErr
	}

	failure := problemSourceReprocessFailure(processErr)
	failure.Code = "provider_outcome_unresolved"
	failure.RetryAt = time.Time{}
	_, err := w.Records.ResolveProblemSourceReprocessOutcomeUnknown(
		commitCtx,
		job.ReconciliationLease(),
		k12storage.ProblemSourceReprocessOutcomeUnknownResolutionNeedsConfirmation,
		failure,
		settledAt,
	)
	logResult("needs_confirmation", "reconciliation_unresolved", failure.Code, err)
	return true, err
}

func problemSourceReprocessLifecycleInterrupted(
	ctx context.Context,
	err error,
) bool {
	if ctx == nil || err == nil || ctx.Err() == nil {
		return false
	}
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Cause(ctx))
}

func (w *ProblemSourceReprocessWorker) processWithHeartbeat(
	ctx context.Context,
	heartbeatInterval time.Duration,
	heartbeat func(context.Context) error,
	process func(context.Context) error,
) (processErr error, heartbeatErr error) {
	workCtx, cancelWork := context.WithCancelCause(ctx)
	heartbeatDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				heartbeatDone <- nil
				return
			case <-ticker.C:
				err := heartbeat(workCtx)
				if err == nil {
					continue
				}
				if errors.Is(
					context.Cause(workCtx),
					errProblemSourceReprocessProcessorFinished,
				) {
					heartbeatDone <- nil
					return
				}
				cancelWork(err)
				heartbeatDone <- err
				return
			}
		}
	}()

	processErr = process(workCtx)
	cancelWork(errProblemSourceReprocessProcessorFinished)
	return processErr, <-heartbeatDone
}

func problemSourceReprocessFailure(err error) k12storage.ProblemSourceReprocessFailure {
	code := "processing_failed"
	detail := "problem source reprocess failed"
	if err != nil {
		detail = boundedProblemSourceReprocessText(err.Error(), 4096)
	}
	return k12storage.ProblemSourceReprocessFailure{Code: code, Detail: detail}
}

func boundedProblemSourceReprocessText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unspecified"
	}
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func problemSourceReprocessRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		attempt = 7
	}
	return time.Duration(1<<(attempt-1)) * 5 * time.Second
}

func (w *ProblemSourceReprocessWorker) initRuntimeLocked() {
	if w.idle == nil {
		w.idle = make(chan struct{})
		close(w.idle)
	}
	if w.pollDone == nil {
		w.pollDone = make(chan struct{})
		close(w.pollDone)
	}
	if w.runCtx == nil {
		base := w.BaseContext
		if base == nil {
			base = context.Background()
		}
		w.runCtx, w.cancel = context.WithCancel(base)
	}
}

func (w *ProblemSourceReprocessWorker) pollInterval() time.Duration {
	if w.PollInterval > 0 {
		return w.PollInterval
	}
	return defaultProblemSourceReprocessPoll
}

// Start installs the restart/due-retry recovery poll. It is process-owned and
// remains live until Shutdown; individual drains still become idle between
// polls, so command nudges do not compete with a second provider loop.
func (w *ProblemSourceReprocessWorker) Start() bool {
	if _, _, _, _, err := w.defaults(); err != nil {
		return false
	}
	w.mu.Lock()
	w.initRuntimeLocked()
	if w.sealed {
		w.mu.Unlock()
		return false
	}
	if w.polling {
		w.mu.Unlock()
		return true
	}
	w.polling = true
	w.pollDone = make(chan struct{})
	ctx := w.runCtx
	interval := w.pollInterval()
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			w.polling = false
			close(w.pollDone)
			w.mu.Unlock()
		}()
		w.Nudge()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.Nudge()
			}
		}
	}()
	return true
}

// Nudge schedules a bounded drain after the command transaction commits.
// Repeated idempotency replays only set rerun; they do not create competing
// provider loops.
func (w *ProblemSourceReprocessWorker) Nudge() bool {
	if _, _, _, _, err := w.defaults(); err != nil {
		return false
	}
	w.mu.Lock()
	w.initRuntimeLocked()
	if w.sealed || w.pauseFence != nil {
		w.mu.Unlock()
		return false
	}
	if w.active {
		w.rerun = true
		w.mu.Unlock()
		return true
	}
	w.active = true
	w.rerun = false
	w.idle = make(chan struct{})
	ctx, cancel := context.WithCancelCause(w.runCtx)
	w.drainCancel = cancel
	w.mu.Unlock()

	go w.runDrain(ctx)
	return true
}

func (w *ProblemSourceReprocessWorker) runDrain(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("K12 source reprocess worker panic; durable lease will recover",
				"error_type", fmt.Sprintf("%T", recovered))
			w.finishDrain()
		}
	}()
	for {
		processed, err := w.RunOnce(ctx)
		if err != nil {
			slog.Warn("K12 source reprocess worker stopped at durable checkpoint",
				"error_type", fmt.Sprintf("%T", err))
			w.finishDrain()
			return
		}
		if processed {
			continue
		}
		w.mu.Lock()
		if w.rerun {
			w.rerun = false
			w.mu.Unlock()
			continue
		}
		w.active = false
		close(w.idle)
		w.mu.Unlock()
		return
	}
}

func (w *ProblemSourceReprocessWorker) finishDrain() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return
	}
	w.active = false
	w.rerun = false
	w.drainCancel = nil
	close(w.idle)
}

// Quiesce installs one process-wide, recoverable fence around the serial
// source-reprocess worker. New nudges and due polls cannot claim work after the
// fence is published; an already-owned drain is cancelled and awaited before
// return. The release function is idempotent and schedules a durable drain, so
// callers can safely quiesce around an Agent deletion without permanently
// sealing the worker. Overlapping callers share a reference-counted fence;
// scheduling resumes only after the last caller releases its own reference.
func (w *ProblemSourceReprocessWorker) Quiesce(
	ctx context.Context,
) (func(), error) {
	if w == nil {
		return func() {}, nil
	}
	if _, _, _, _, err := w.defaults(); err != nil {
		return func() {}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	w.initRuntimeLocked()
	if w.pauseFence == nil {
		w.pauseFence = &problemSourceReprocessPauseFence{}
	}
	fence := w.pauseFence
	w.pauseRefs++
	done := w.idle
	cancel := w.drainCancel
	w.mu.Unlock()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			w.mu.Lock()
			if w.pauseFence != fence || w.pauseRefs < 1 {
				w.mu.Unlock()
				return
			}
			w.pauseRefs--
			if w.pauseRefs != 0 {
				w.mu.Unlock()
				return
			}
			w.pauseFence = nil
			sealed := w.sealed
			w.mu.Unlock()
			if !sealed {
				w.Nudge()
			}
		})
	}
	if cancel != nil {
		cancel(errProblemSourceReprocessQuiesced)
	}
	select {
	case <-done:
		return release, nil
	case <-ctx.Done():
		return release, ctx.Err()
	}
}

func (w *ProblemSourceReprocessWorker) Wait(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	w.initRuntimeLocked()
	done := w.idle
	w.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *ProblemSourceReprocessWorker) Shutdown(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	w.initRuntimeLocked()
	w.sealed = true
	done := w.idle
	pollDone := w.pollDone
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, channel := range []<-chan struct{}{done, pollDone} {
		select {
		case <-channel:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// StartProblemSourceReprocessRecovery installs the startup/due-retry poll for
// the same worker that post-commit source-action nudges use.
func (c *ImageTaskCoordinator) StartProblemSourceReprocessRecovery() bool {
	return c != nil && c.SourceReprocess != nil && c.SourceReprocess.Start()
}
