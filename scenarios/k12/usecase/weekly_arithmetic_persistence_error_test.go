package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type failingWeeklyCandidates struct{ err error }

func (s failingWeeklyCandidates) GenerateWeeklyPracticeCandidates(
	context.Context,
	usecase.WeeklyPracticeCandidateRequest,
) ([]usecase.WeeklyPracticeCandidate, error) {
	return nil, s.err
}

func newWeeklyArithmeticPersistenceFailureDeps(t *testing.T) (usecase.Deps, k12.WeeklyPracticePlan) {
	t.Helper()
	d := newDataDeps(t, "xiaoming")
	d.Now = func() int64 { return 1785081600 }
	configureWeeklyBundle(t, &d, true)
	plan, _, err := d.EnsureWeeklyPracticePlan(context.Background(), usecase.EnsureWeeklyPracticePlanRequest{
		AgentName: "xiaoming", IdempotencyKey: "weekly-arithmetic-persistence-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, plan
}

func rejectWeeklyArithmeticReady(t *testing.T, d usecase.Deps) {
	t.Helper()
	if _, err := d.Records.DB().Exec(`CREATE TRIGGER reject_weekly_arithmetic_ready
        BEFORE UPDATE OF state ON k12_weekly_arithmetic_batches
        WHEN NEW.state = 'ready'
        BEGIN SELECT RAISE(ABORT, 'injected terminal persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWeeklyArithmeticBatch_PropagatesTerminalPersistenceFailure(t *testing.T) {
	d, plan := newWeeklyArithmeticPersistenceFailureDeps(t)
	d.WeeklyCandidates = &countingWeeklyCandidates{}
	rejectWeeklyArithmeticReady(t, d)

	batch, replay, err := d.CreateWeeklyArithmeticBatchWithItemCount(
		context.Background(), "xiaoming", plan.PlanID, plan.Revision, 1, "create-terminal-write-failure",
	)
	if err == nil {
		t.Fatalf("终态持久化失败不得伪成功: batch=%+v replay=%t", batch, replay)
	}
	if replay {
		t.Fatal("首次创建不得标记 replay")
	}
	stored, getErr := d.Records.GetWeeklyArithmeticBatch(context.Background(), "xiaoming", batch.BatchID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.State != k12.WeeklyArithmeticPreparing {
		t.Fatalf("被拒绝的终态写不得伪造 ready: state=%s", stored.State)
	}
}

func TestRetryWeeklyArithmeticBatch_PropagatesTerminalPersistenceFailure(t *testing.T) {
	d, plan := newWeeklyArithmeticPersistenceFailureDeps(t)
	// 先让真实 Store 正常写出可重试终态，再在重试的 ready 终态写入处注入失败。
	d.WeeklyCandidates = failingWeeklyCandidates{err: errors.New("upstream unavailable")}
	batch, replay, err := d.CreateWeeklyArithmeticBatchWithItemCount(
		context.Background(), "xiaoming", plan.PlanID, plan.Revision, 1, "prepare-retryable-batch",
	)
	if err != nil || replay {
		t.Fatalf("准备可重试批次: batch=%+v replay=%t err=%v", batch, replay, err)
	}
	stored, err := d.Records.GetWeeklyArithmeticBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil || stored.State != k12.WeeklyArithmeticFailedRetryable {
		t.Fatalf("准备可重试批次失败: state=%s err=%v", stored.State, err)
	}
	d.WeeklyCandidates = &countingWeeklyCandidates{}
	rejectWeeklyArithmeticReady(t, d)

	batch, replay, err = d.RetryWeeklyArithmeticBatch(
		context.Background(), "xiaoming", batch.BatchID, "retry-terminal-write-failure",
	)
	if err == nil {
		t.Fatalf("重试终态持久化失败不得伪成功: batch=%+v replay=%t", batch, replay)
	}
	if replay {
		t.Fatal("首次重试不得标记 replay")
	}
	stored, getErr := d.Records.GetWeeklyArithmeticBatch(context.Background(), "xiaoming", batch.BatchID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.State != k12.WeeklyArithmeticPreparing {
		t.Fatalf("被拒绝的重试终态写不得伪造 ready: state=%s", stored.State)
	}
}
