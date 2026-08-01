package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type textbookCatalogRepository interface {
	RecoverTextbookCatalogJobs(context.Context, time.Time, int) error
	ClaimTextbookCatalogJob(context.Context, string, time.Time, time.Duration) (k12storage.TextbookCatalogJobClaim, bool, error)
	RenewTextbookCatalogJob(context.Context, k12storage.TextbookCatalogJobClaim, time.Time, time.Duration) error
	LoadTextbookCatalogSource(context.Context, k12storage.TextbookCatalogJobClaim, time.Time) (k12storage.TextbookCatalogSource, error)
	PublishTextbookCatalog(context.Context, k12storage.TextbookCatalogJobClaim, k12storage.TextbookCatalogPublication, time.Time) error
	FailTextbookCatalogJob(context.Context, k12storage.TextbookCatalogJobClaim, k12storage.TextbookCatalogFailure, time.Time) error
}

type TextbookCatalogWorkerConfig struct {
	WorkerID          string
	Lease             time.Duration
	HeartbeatInterval time.Duration
	ExtractTimeout    time.Duration
	MaxAttempts       int
	RetryBase         time.Duration
	RetryMax          time.Duration
	RecoveryBatch     int
	Now               func() time.Time
}

type TextbookCatalogWorker struct {
	repository textbookCatalogRepository
	extractor  TextbookCatalogExtractor
	config     TextbookCatalogWorkerConfig
}

func NewTextbookCatalogWorker(
	repository textbookCatalogRepository,
	extractor TextbookCatalogExtractor,
	config TextbookCatalogWorkerConfig,
) *TextbookCatalogWorker {
	return &TextbookCatalogWorker{repository: repository, extractor: extractor, config: config}
}

// RunOnce claims at most one durable job. The heartbeat owns only the current
// lease; source loading, extraction and publication are all fenced by it.
func (w *TextbookCatalogWorker) RunOnce(ctx context.Context) (bool, error) {
	if err := w.valid(); err != nil {
		return false, err
	}
	now := w.now()
	if err := w.repository.RecoverTextbookCatalogJobs(ctx, now, w.config.RecoveryBatch); err != nil {
		return false, err
	}
	claim, found, err := w.repository.ClaimTextbookCatalogJob(ctx, w.config.WorkerID, now, w.config.Lease)
	if err != nil || !found {
		return false, err
	}
	if claim.Attempt < 1 || claim.Attempt > w.config.MaxAttempts {
		err = fmt.Errorf("catalog attempt %d exceeds bounded policy", claim.Attempt)
		return true, w.recordFailure(ctx, claim, err, true)
	}

	operationCtx, cancel := context.WithTimeout(ctx, w.config.ExtractTimeout)
	defer cancel()
	stopHeartbeat := w.startHeartbeat(operationCtx, cancel, claim)
	source, err := w.repository.LoadTextbookCatalogSource(operationCtx, claim, w.now())
	var publication k12storage.TextbookCatalogPublication
	if err == nil {
		publication, err = w.extractor.Extract(operationCtx, source)
	}
	if heartbeatErr := stopHeartbeat(); heartbeatErr != nil {
		return true, heartbeatErr
	}
	if err == nil {
		err = w.repository.PublishTextbookCatalog(ctx, claim, publication, w.now())
	}
	if err == nil {
		return true, nil
	}
	if errors.Is(err, k12storage.ErrTextbookCatalogJobFenced) {
		return true, err
	}
	terminal := errors.Is(err, ErrTextbookCatalogEvidenceInsufficient) ||
		errors.Is(err, k12storage.ErrTextbookCatalogSourceIncomplete) ||
		errors.Is(err, records.ErrIllegalTransition) ||
		claim.Attempt >= w.config.MaxAttempts
	return true, w.recordFailure(ctx, claim, err, terminal)
}

func (w *TextbookCatalogWorker) valid() error {
	if w == nil || w.repository == nil || w.extractor == nil || strings.TrimSpace(w.config.WorkerID) == "" ||
		w.config.Lease <= 0 || w.config.HeartbeatInterval <= 0 ||
		w.config.HeartbeatInterval >= w.config.Lease || w.config.ExtractTimeout <= 0 ||
		w.config.MaxAttempts < 1 || w.config.RetryBase <= 0 || w.config.RetryMax < w.config.RetryBase {
		return fmt.Errorf("invalid textbook catalog worker configuration")
	}
	return nil
}

func (w *TextbookCatalogWorker) recordFailure(
	ctx context.Context,
	claim k12storage.TextbookCatalogJobClaim,
	cause error,
	terminal bool,
) error {
	now := w.now()
	failure := k12storage.TextbookCatalogFailure{
		Code: "catalog_transient_failure", Message: "识别失败",
		Terminal: terminal,
	}
	if errors.Is(cause, ErrTextbookCatalogEvidenceInsufficient) ||
		errors.Is(cause, k12storage.ErrTextbookCatalogSourceIncomplete) ||
		errors.Is(cause, records.ErrIllegalTransition) {
		failure.Code = "catalog_evidence_incomplete"
		failure.Message = "识别失败"
		failure.Terminal = true
	}
	if !failure.Terminal {
		failure.RetryAt = now.Add(w.retryDelay(claim.Attempt))
	}
	transitionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := w.repository.FailTextbookCatalogJob(
		transitionCtx, claim, failure, now,
	); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (w *TextbookCatalogWorker) now() time.Time {
	if w.config.Now != nil {
		return w.config.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *TextbookCatalogWorker) retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := w.config.RetryBase
	for index := 1; index < attempt && delay < w.config.RetryMax; index++ {
		if delay > w.config.RetryMax/2 {
			return w.config.RetryMax
		}
		delay *= 2
	}
	if delay > w.config.RetryMax {
		return w.config.RetryMax
	}
	return delay
}

func (w *TextbookCatalogWorker) startHeartbeat(
	ctx context.Context,
	cancel context.CancelFunc,
	claim k12storage.TextbookCatalogJobClaim,
) func() error {
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var heartbeatErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(w.config.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.repository.RenewTextbookCatalogJob(ctx, claim, w.now(), w.config.Lease); err != nil {
					if ctx.Err() != nil {
						return
					}
					mu.Lock()
					heartbeatErr = err
					mu.Unlock()
					cancel()
					return
				}
			}
		}
	}()
	return func() error {
		once.Do(func() { close(stop) })
		<-done
		mu.Lock()
		defer mu.Unlock()
		return heartbeatErr
	}
}
