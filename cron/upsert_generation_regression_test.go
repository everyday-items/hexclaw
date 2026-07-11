package cron

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerationChecksAfterLongWorkUseFreshBudgets(t *testing.T) {
	source, err := os.ReadFile("cron.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "jobGenerationCurrent(dbCtx, job)") {
		t.Fatal("executeJob reuses its aging dbCtx after history; delivery boundary needs a fresh generation-check budget")
	}

	agentSource, err := os.ReadFile("agent_mode.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(agentSource), "jobGenerationCurrent(alertCtx, job)"); got > 1 {
		t.Fatalf("agent failure alert reuses one aging context across work (%d checks); final check must be fresh", got)
	}
}

type generationBlockingEngine struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type generationBlockingCompiler struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type lateWriteBlockingStateStore struct {
	StateStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *lateWriteBlockingStateStore) Set(jobID, key, val string) error {
	if val == "old-generation-state" {
		s.once.Do(func() { close(s.started) })
		<-s.release
	}
	return s.StateStore.Set(jobID, key, val)
}

func (c *generationBlockingCompiler) Compile(ctx context.Context, _ string, _ CompileHints) (*JobSpec, error) {
	c.once.Do(func() { close(c.started) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &JobSpec{Runtime: RuntimeStarlark, Script: `emit("healed-old-generation")`, TimeoutSec: 10}, nil
}

func (e *generationBlockingEngine) Name() string          { return "generation-blocking" }
func (e *generationBlockingEngine) Available() bool       { return true }
func (e *generationBlockingEngine) Validate(string) error { return nil }
func (e *generationBlockingEngine) Execute(ctx context.Context, spec *JobSpec) (*RunResult, error) {
	if spec.Script == "old-generation" {
		e.once.Do(func() { close(e.started) })
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &RunResult{Status: "success", Stdout: spec.Script}, nil
}

func TestUpsertJobFromScript_RunningOldGenerationCannotOverwriteReplacement(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	eng := &generationBlockingEngine{started: make(chan struct{}), release: make(chan struct{})}
	s.engines[eng.Name()] = eng
	delivered := make(chan string, 1)
	s.SetNotifier(func(_ *Job, _, _, body string) {
		delivered <- body
	})

	old := &Job{
		Name: "stable task", Type: JobTypeCron, Schedule: "*/5 * * * *", UserID: "u1",
		Status: StatusActive, SourceKey: "agent-a/daily", Spec: &JobSpec{Runtime: eng.Name(), Script: "old-generation", TimeoutSec: 30},
	}
	if err := s.AddJob(context.Background(), old); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.mu.Lock()
	old.NextRunAt = time.Now().Add(-time.Minute)
	runningCopy := *old
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.executeJob(&runningCopy)
		close(done)
	}()
	<-eng.started

	replacement, err := s.UpsertJobFromScript(context.Background(), AddJobRequest{
		Name: "stable task", Schedule: "1 0 1 1 *", UserID: "u1", SourceKey: "agent-a/daily",
	}, eng.Name(), "new-generation")
	if err != nil {
		close(eng.release)
		t.Fatalf("UpsertJobFromScript: %v", err)
	}
	wantNext := replacement.NextRunAt
	close(eng.release)
	<-done

	got, ok := s.GetJob(context.Background(), replacement.ID)
	if !ok {
		t.Fatal("replacement disappeared from scheduler")
	}
	if got.Schedule != "1 0 1 1 *" || got.Spec == nil || got.Spec.Script != "new-generation" {
		t.Fatalf("replacement definition changed: %+v", got)
	}
	if !got.NextRunAt.Equal(wantNext) {
		t.Fatalf("old generation overwrote replacement next_run_at: got=%s want=%s", got.NextRunAt, wantNext)
	}
	select {
	case body := <-delivered:
		t.Fatalf("old generation delivered after replacement: %q", body)
	default:
	}
}

func TestClaimJob_OldGenerationCannotClaimReplacement(t *testing.T) {
	s, oldCopy, replacement := newGenerationReplacementFixture(t)
	if s.claimJob(oldCopy) {
		t.Fatal("an old generation claimed the replacement row")
	}
	got, _ := s.GetJob(context.Background(), replacement.ID)
	if !got.NextRunAt.Equal(replacement.NextRunAt) {
		t.Fatalf("claim changed replacement state: got=%s want=%s", got.NextRunAt, replacement.NextRunAt)
	}
}

func TestFastForward_OldGenerationCannotAdvanceReplacement(t *testing.T) {
	s, oldCopy, replacement := newGenerationReplacementFixture(t)
	wantNext := replacement.NextRunAt
	s.fastForward(oldCopy, time.Now().Add(48*time.Hour))
	got, _ := s.GetJob(context.Background(), replacement.ID)
	if !got.NextRunAt.Equal(wantNext) {
		t.Fatalf("old generation fast-forwarded replacement: got=%s want=%s", got.NextRunAt, wantNext)
	}
}

func TestContinuousOldGenerationCannotFinishReplacement(t *testing.T) {
	s, oldCopy, replacement := newGenerationReplacementFixture(t)
	s.saveContinuousCheckpointForJob(oldCopy, continuousCheckpoint{Completed: true})
	s.runContinuousAgentJob(context.Background(), oldCopy)
	got, _ := s.GetJob(context.Background(), replacement.ID)
	if got.Status != StatusActive {
		t.Fatalf("old continuous generation changed replacement status: got=%s want=%s", got.Status, StatusActive)
	}
	if cp := s.loadContinuousCheckpointForJob(replacement); cp.Completed || cp.Tick != 0 {
		t.Fatalf("old continuous generation checkpoint leaked into replacement: %+v", cp)
	}
}

func TestContinuousCheckpoint_ReplacementGenerationRejectsLateOldWrite(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	started := make(chan string, 1)
	release := make(chan struct{})
	s.SetAgentRunner(func(_ context.Context, job *Job) (AgentResult, error) {
		started <- job.SourcePrompt
		<-release
		return AgentResult{Content: "old generation finished late\nPROGRESS: stale step\nTASK_COMPLETE: yes"}, nil
	})

	old := newContinuousJob("old continuous definition")
	old.SourceKey = "agent-a/continuous"
	if err := s.AddJob(ctx, old); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.saveContinuousCheckpoint(old.ID, continuousCheckpoint{
		Tick: 7, History: []string{"old step 7"},
	})
	runningCopy := *old
	done := make(chan *RunResult, 1)
	go func() { done <- s.runContinuousAgentJob(ctx, &runningCopy) }()

	prompt := <-started
	if !strings.Contains(prompt, "第 8 次推进") {
		close(release)
		<-done
		t.Fatalf("old run did not actually consume its checkpoint: %q", prompt)
	}
	replacement, err := s.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "replacement", Schedule: "@daily", UserID: old.UserID, SourceKey: old.SourceKey,
	}, RuntimeStarlark, `emit("new generation")`)
	if err != nil {
		close(release)
		<-done
		t.Fatalf("UpsertJobFromScript: %v", err)
	}
	beforeLateWrite := s.loadContinuousCheckpoint(replacement.ID)

	// Do not stop at the initial-read assertion: release the old generation and
	// wait until it has executed its checkpoint save. This catches the late-write
	// half of the bug instead of masking it with an early failure.
	close(release)
	result := <-done
	afterLateWrite := s.loadContinuousCheckpoint(replacement.ID)

	if result == nil || result.Status != "success" {
		t.Fatalf("old generation did not reach the stale-write boundary: %+v", result)
	}
	if beforeLateWrite.Tick != 0 || beforeLateWrite.Completed || len(beforeLateWrite.History) != 0 {
		t.Fatalf("replacement inherited old checkpoint before stale writer returned: %+v", beforeLateWrite)
	}
	if afterLateWrite.Tick != 0 || afterLateWrite.Completed || len(afterLateWrite.History) != 0 {
		t.Fatalf("late old generation write polluted replacement checkpoint: %+v", afterLateWrite)
	}
	got, _ := s.GetJob(ctx, replacement.ID)
	if got.Status != StatusActive {
		t.Fatalf("old generation changed replacement status: got=%s want=%s", got.Status, StatusActive)
	}
}

func TestStarlarkState_ReplacementGenerationRejectsLateOldWrite(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	blockingState := &lateWriteBlockingStateStore{
		StateStore: s.state,
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	s.state = blockingState
	s.engines[RuntimeStarlark].(*StarlarkEngine).SetStateStore(blockingState)

	old := &Job{
		Name: "old state writer", Type: JobTypeCron, Schedule: "@daily", UserID: "u1",
		SourceKey: "agent-a/stateful", Status: StatusActive,
		Spec: &JobSpec{Runtime: RuntimeStarlark, TimeoutSec: 30, Script: `
state_set("shared", "old-generation-state")
emit({"status":"success", "data":"old finished"})
`},
	}
	if err := s.AddJob(context.Background(), old); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	runningCopy := *old
	done := make(chan struct{})
	go func() {
		s.executeJob(&runningCopy)
		close(done)
	}()
	<-blockingState.started

	replacement, err := s.UpsertJobFromScript(context.Background(), AddJobRequest{
		Name: "replacement state reader", Schedule: "0 0 1 1 *", UserID: old.UserID, SourceKey: old.SourceKey,
	}, RuntimeStarlark, `emit({"status":"success", "data":state_get("shared", "fresh")})`)
	if err != nil {
		close(blockingState.release)
		<-done
		t.Fatalf("UpsertJobFromScript: %v", err)
	}
	close(blockingState.release)
	<-done // old generation has now performed the late state_set

	delivered := make(chan string, 1)
	s.SetNotifier(func(_ *Job, _, _, body string) { delivered <- body })
	replacementCopy := *replacement
	s.executeJob(&replacementCopy)
	select {
	case body := <-delivered:
		if body != "fresh" {
			t.Fatalf("replacement read late old-generation state: %q", body)
		}
	default:
		t.Fatal("replacement did not execute and deliver its state_get result")
	}
}

func TestSelfHeal_OldGenerationCannotOverwriteReplacement(t *testing.T) {
	s, oldCopy, replacement := newGenerationReplacementFixture(t)
	compiler := &generationBlockingCompiler{started: make(chan struct{}), release: make(chan struct{})}
	s.compiler = compiler
	seedFailures(t, s, oldCopy.ID, selfHealThreshold)

	done := make(chan struct{})
	go func() {
		s.maybeSelfHeal(context.Background(), oldCopy, &RunResult{Status: "error", Error: "boom"})
		close(done)
	}()
	<-compiler.started
	close(compiler.release)
	<-done

	got, _ := s.GetJob(context.Background(), replacement.ID)
	if got.Spec == nil || got.Spec.Script != "new-generation" {
		t.Fatalf("old generation self-heal overwrote replacement spec: %+v", got.Spec)
	}
}

func TestPersistHistory_OldGenerationCannotAppendToReplacement(t *testing.T) {
	s, oldCopy, _ := newGenerationReplacementFixture(t)
	persisted, err := s.persistHistoryForGeneration(context.Background(), oldCopy,
		"success", "", "", 1, time.Now(), "old output", "", 0, nil)
	if err != nil {
		t.Fatalf("persistHistoryForGeneration: %v", err)
	}
	if persisted {
		t.Fatal("old generation appended history after replacement")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM cron_job_runs WHERE job_id = ?`, oldCopy.ID).Scan(&count); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if count != 0 {
		t.Fatalf("old generation history rows = %d, want 0", count)
	}
}

func TestDeliverResult_OldGenerationCannotDeliverOrClearReplacementOutcome(t *testing.T) {
	s, oldCopy, replacement := newGenerationReplacementFixture(t)
	var notifications atomic.Int32
	s.SetNotifier(func(*Job, string, string, string) { notifications.Add(1) })
	s.mu.Lock()
	s.jobs[replacement.ID].LastDeliveryError = "replacement delivery state"
	s.mu.Unlock()

	s.deliverResult(oldCopy, &RunResult{Status: "success", Stdout: "old output"})
	if got := notifications.Load(); got != 0 {
		t.Fatalf("old generation delivered %d notification(s) after replacement", got)
	}
	got, _ := s.GetJob(context.Background(), replacement.ID)
	if got.LastDeliveryError != "replacement delivery state" {
		t.Fatalf("old delivery outcome polluted replacement: %q", got.LastDeliveryError)
	}
}

func TestDeliveryOutcome_ReplacementDuringCallbackIsNotPolluted(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	old := &Job{
		Name: "stable task", Type: JobTypeCron, Schedule: "*/5 * * * *", UserID: "u1",
		Status: StatusActive, SourceKey: "agent-a/daily", Spec: minimalSpec(),
	}
	if err := s.AddJob(context.Background(), old); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	var replacement *Job
	var replaceErr error
	s.SetNotifier(func(*Job, string, string, string) {
		replacement, replaceErr = s.UpsertJobFromScript(context.Background(), AddJobRequest{
			Name: "stable task", Schedule: "1 0 1 1 *", UserID: "u1", SourceKey: "agent-a/daily",
		}, RuntimeStarlark, `emit("new")`)
		if replaceErr == nil {
			s.mu.Lock()
			s.jobs[replacement.ID].LastDeliveryError = "replacement delivery state"
			s.mu.Unlock()
		}
	})

	s.deliverResult(old, &RunResult{Status: "success", Stdout: "old output"})
	if replaceErr != nil {
		t.Fatalf("replacement from notifier: %v", replaceErr)
	}
	got, _ := s.GetJob(context.Background(), replacement.ID)
	if got.LastDeliveryError != "replacement delivery state" {
		t.Fatalf("old post-callback outcome polluted replacement: %q", got.LastDeliveryError)
	}
}

func TestDeliverResult_FreshGenerationCheckPerTarget(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	job := &Job{
		Name: "multi-target", Schedule: "@hourly", UserID: "u1",
		Deliver: []string{"slow", "fast"}, Spec: minimalSpec(),
	}
	if err := s.AddJob(context.Background(), job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	var mu sync.Mutex
	var targets []string
	s.SetDeliverer(func(_ *Job, target, _ string) error {
		mu.Lock()
		targets = append(targets, target)
		mu.Unlock()
		if target == "slow" {
			time.Sleep(5100 * time.Millisecond)
		}
		return nil
	})

	s.deliverResult(job, &RunResult{Status: "success", Stdout: "payload"})
	mu.Lock()
	defer mu.Unlock()
	if len(targets) != 2 || targets[0] != "slow" || targets[1] != "fast" {
		t.Fatalf("an expired check context suppressed a later current-generation target: %v", targets)
	}
}

func TestRemoveJob_AcquiresLifecycleLockBeforeDeletingRow(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	job := &Job{Name: "remove", Schedule: "@hourly", UserID: "u1", Spec: minimalSpec()}
	if err := s.AddJob(context.Background(), job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	s.mu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- s.RemoveJob(context.Background(), job.ID)
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	var count int
	if err := db.QueryRow(`SELECT COUNT(1) FROM cron_jobs WHERE id = ?`, job.ID).Scan(&count); err != nil {
		s.mu.Unlock()
		t.Fatalf("count cron row: %v", err)
	}
	if count != 1 {
		s.mu.Unlock()
		t.Fatalf("RemoveJob deleted DB row before acquiring scheduler lifecycle lock: count=%d", count)
	}
	s.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
}

func newGenerationReplacementFixture(t *testing.T) (*Scheduler, *Job, *Job) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	eng := &generationBlockingEngine{started: make(chan struct{}), release: make(chan struct{})}
	s.engines[eng.Name()] = eng
	old := &Job{
		Name: "stable task", Type: JobTypeCron, Schedule: "*/5 * * * *", UserID: "u1",
		Status: StatusActive, SourceKey: "agent-a/daily", Continuous: true,
		Spec: &JobSpec{Runtime: eng.Name(), Script: "old-generation", TimeoutSec: 30},
	}
	if err := s.AddJob(context.Background(), old); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	oldCopy := *old
	replacement, err := s.UpsertJobFromScript(context.Background(), AddJobRequest{
		Name: "stable task", Schedule: "1 0 1 1 *", UserID: "u1", SourceKey: "agent-a/daily",
	}, eng.Name(), "new-generation")
	if err != nil {
		t.Fatalf("UpsertJobFromScript: %v", err)
	}
	return s, &oldCopy, replacement
}
