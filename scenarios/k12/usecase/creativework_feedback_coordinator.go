package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// CreativeWorkFeedbackCoordinator owns direct-text feedback workers. Image
// works remain owned by ImageTaskCoordinator; storage recovery filters the two
// domains so a generation always has exactly one runner.
type CreativeWorkFeedbackCoordinator struct {
	Deps        *Deps
	Records     *k12storage.Store
	BaseContext context.Context

	workerMu    sync.Mutex
	runCtx      context.Context
	runCancel   context.CancelCauseFunc
	agentWorkers agentWorkerFenceRegistry
	workerCount int
	workerIdle  chan struct{}
	sealed      bool
}

func (c *CreativeWorkFeedbackCoordinator) validate() error {
	if c == nil || c.Deps == nil || c.Records == nil {
		return fmt.Errorf("creative work feedback coordinator dependencies unavailable")
	}
	return nil
}

func (c *CreativeWorkFeedbackCoordinator) initWorkerRuntimeLocked() {
	if c.workerIdle == nil {
		c.workerIdle = make(chan struct{})
		close(c.workerIdle)
	}
	if c.runCtx == nil {
		base := c.BaseContext
		if base == nil {
			base = context.Background()
		}
		c.runCtx, c.runCancel = context.WithCancelCause(base)
	}
}

// StartAsync accepts only a persisted, non-terminal generation. Duplicate
// schedules in the same process collapse on the generation identity.
func (c *CreativeWorkFeedbackCoordinator) StartAsync(
	agentName, generationID string,
) bool {
	if c.validate() != nil {
		return false
	}
	agentName = strings.TrimSpace(agentName)
	generationID = strings.TrimSpace(generationID)
	if agentName == "" || generationID == "" {
		return false
	}
	generation, err := c.Records.GetWorkFeedbackGeneration(
		context.Background(), agentName, generationID,
	)
	if err != nil ||
		(generation.Status != k12.WorkFeedbackQueued &&
			generation.Status != k12.WorkFeedbackRunning) {
		return false
	}

	key := agentName + "\x00" + generationID
	c.workerMu.Lock()
	c.initWorkerRuntimeLocked()
	if c.sealed {
		c.workerMu.Unlock()
		return false
	}
	runCtx, finishWorker, accepted := c.agentWorkers.start(
		c.runCtx, agentName, key,
	)
	if !accepted {
		c.workerMu.Unlock()
		return false
	}
	if c.workerCount == 0 {
		c.workerIdle = make(chan struct{})
	}
	c.workerCount++
	c.workerMu.Unlock()

	go func() {
		defer func() {
			c.workerMu.Lock()
			c.workerCount--
			if c.workerCount == 0 {
				close(c.workerIdle)
			}
			c.workerMu.Unlock()
			finishWorker()
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error(
					"K12 CreativeWork feedback worker panic; durable checkpoint retained",
					"agent", agentName,
					"generation_id", generationID,
					"panic", recovered,
				)
			}
		}()
		if err := c.Run(runCtx, agentName, generationID); err != nil {
			slog.Warn(
				"K12 CreativeWork feedback worker stopped at durable checkpoint",
				"agent", agentName,
				"generation_id", generationID,
				"err", err,
			)
		}
	}()
	return true
}

// QuiesceAgent fences new local feedback workers for one Agent, cancels and
// drains workers already owned by this coordinator, and returns an idempotent
// resume callback for the enclosing Agent-deletion saga.
func (c *CreativeWorkFeedbackCoordinator) QuiesceAgent(
	ctx context.Context,
	agentName string,
) (func(), error) {
	if c == nil {
		return func() {}, nil
	}
	return c.agentWorkers.quiesceAgent(ctx, agentName)
}

func (c *CreativeWorkFeedbackCoordinator) Run(
	ctx context.Context,
	agentName, generationID string,
) error {
	if err := c.validate(); err != nil {
		return err
	}
	generation, err := c.Records.GetWorkFeedbackGeneration(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(generationID),
	)
	if err != nil {
		return err
	}
	switch generation.Status {
	case k12.WorkFeedbackSucceeded:
		return nil
	case k12.WorkFeedbackFailed:
		return k12storage.ErrImageTaskInvalidState
	case k12.WorkFeedbackQueued, k12.WorkFeedbackRunning:
	default:
		return k12storage.ErrImageTaskInvalidState
	}
	_, err = c.Deps.GenerateWorkFeedbackCommand(
		ctx, generation.AgentName, generation.WorkID, generation.CommandKey,
	)
	return err
}

func (c *CreativeWorkFeedbackCoordinator) recoverySafe(
	ctx context.Context,
	generation k12.WorkFeedbackGeneration,
) (bool, error) {
	operationKey := "work:" + generation.WorkID +
		":version:" + generation.GenerationID + ":feedback"
	invocation, err := c.Records.GetLatestWorkFeedbackInvocation(
		ctx, generation.AgentName, generation.WorkID, operationKey,
	)
	if errors.Is(err, k12storage.ErrImageTaskNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch invocation.Status {
	case k12.ImageTaskInvocationPrepared, k12.ImageTaskInvocationSucceeded:
		return true, nil
	default:
		// sent/outcome_unknown and failed are deliberately parked. The former
		// requires provider result-query reconciliation; the latter requires an
		// explicit user retry of the same initial generation.
		return false, nil
	}
}

func (c *CreativeWorkFeedbackCoordinator) Recover(
	ctx context.Context,
	agents []string,
) (int, error) {
	if err := c.validate(); err != nil {
		return 0, err
	}
	recovered := 0
	for _, agentName := range agents {
		generations, err := c.Records.ListDirectWorkFeedbackGenerationsForRecovery(
			ctx, strings.TrimSpace(agentName),
		)
		if err != nil {
			return recovered, err
		}
		for _, generation := range generations {
			safe, err := c.recoverySafe(ctx, generation)
			if err != nil {
				return recovered, err
			}
			if safe && c.StartAsync(generation.AgentName, generation.GenerationID) {
				recovered++
			}
		}
	}
	return recovered, nil
}

func (c *CreativeWorkFeedbackCoordinator) Wait(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.workerMu.Lock()
	c.initWorkerRuntimeLocked()
	done := c.workerIdle
	c.workerMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *CreativeWorkFeedbackCoordinator) Shutdown(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.workerMu.Lock()
	c.initWorkerRuntimeLocked()
	c.sealed = true
	done := c.workerIdle
	cancel := c.runCancel
	c.workerMu.Unlock()
	if cancel != nil {
		cancel(context.Canceled)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
