package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// SinglePracticeGenerationCoordinator 管理错题与积累共享逐题任务的进程内 worker。
// SQLite 是队列与状态真相源；本协调器只合并重复调度并提供生命周期控制。
type SinglePracticeGenerationCoordinator struct {
	Deps        *Deps
	Records     *k12storage.Store
	BaseContext context.Context

	workerMu    sync.Mutex
	runCtx      context.Context
	runCancel   context.CancelCauseFunc
	active      map[string]bool
	workerCount int
	workerIdle  chan struct{}
	sealed      bool
}

func (c *SinglePracticeGenerationCoordinator) validate() error {
	if c == nil || c.Deps == nil || c.Records == nil {
		return fmt.Errorf("single practice generation coordinator dependencies unavailable")
	}
	return nil
}

func (c *SinglePracticeGenerationCoordinator) initWorkerRuntimeLocked() {
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

func (c *SinglePracticeGenerationCoordinator) StartAsync(
	agentName, generationJobID string,
) bool {
	if c.validate() != nil {
		return false
	}
	agentName = strings.TrimSpace(agentName)
	generationJobID = strings.TrimSpace(generationJobID)
	if agentName == "" || generationJobID == "" {
		return false
	}
	job, err := c.Records.GetPracticeGenerationJobByID(
		context.Background(), agentName, generationJobID,
	)
	if err != nil || job.Scope != "single" || job.RetiredAt != 0 ||
		(job.Status != k12.PracticeGenerationQueued &&
			job.Status != k12.PracticeGenerationGenerating &&
			job.Status != k12.PracticeGenerationValidating) {
		return false
	}
	key := agentName + "\x00" + generationJobID
	c.workerMu.Lock()
	c.initWorkerRuntimeLocked()
	if c.sealed || c.active[key] {
		c.workerMu.Unlock()
		return false
	}
	if c.active == nil {
		c.active = map[string]bool{}
	}
	c.active[key] = true
	if c.workerCount == 0 {
		c.workerIdle = make(chan struct{})
	}
	c.workerCount++
	runCtx := c.runCtx
	c.workerMu.Unlock()

	go func() {
		defer func() {
			c.workerMu.Lock()
			delete(c.active, key)
			c.workerCount--
			if c.workerCount == 0 {
				close(c.workerIdle)
			}
			c.workerMu.Unlock()
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error(
					"K12 single practice worker panic; durable checkpoint retained",
					"agent", agentName,
					"generation_job_id", generationJobID,
					"panic", recovered,
				)
			}
		}()
		var processErr error
		switch job.SourceKind {
		case k12.PracticeGenerationSourceAccumulation:
			_, _, _, processErr = c.Deps.ProcessAccumulationPracticeGeneration(
				runCtx, agentName, generationJobID,
			)
		default:
			_, processErr = c.Deps.ProcessSinglePracticeGeneration(
				runCtx, agentName, generationJobID,
			)
		}
		if processErr != nil {
			slog.Warn(
				"K12 single practice worker stopped at durable checkpoint",
				"agent", agentName,
				"generation_job_id", generationJobID,
				"err", processErr,
			)
		}
	}()
	return true
}

func (c *SinglePracticeGenerationCoordinator) Recover(ctx context.Context) (int, error) {
	if err := c.validate(); err != nil {
		return 0, err
	}
	jobs, err := c.Records.ListRecoverableSinglePracticeGenerations(ctx)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, job := range jobs {
		if c.StartAsync(job.AgentName, job.GenerationJobID) {
			recovered++
		}
	}
	return recovered, nil
}

func (c *SinglePracticeGenerationCoordinator) Wait(ctx context.Context) error {
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

func (c *SinglePracticeGenerationCoordinator) Shutdown(ctx context.Context) error {
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
