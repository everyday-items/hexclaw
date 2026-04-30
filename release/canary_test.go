package release

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewRollout_RejectsEmptyStages(t *testing.T) {
	if _, err := NewRollout(nil, nil); err == nil {
		t.Error("空 stages 应报错")
	}
}

func TestNewRollout_RejectsBadPercent(t *testing.T) {
	cases := [][]CanaryStage{
		{{Percent: -1}},
		{{Percent: 200}},
		{{Percent: 50}, {Percent: 25}}, // 非递增
		{{Percent: 50}, {Percent: 50}}, // 等量
	}
	for i, stages := range cases {
		if _, err := NewRollout(stages, nil); err == nil {
			t.Errorf("case %d: 应报错", i)
		}
	}
}

func TestRollout_AdvanceFromPending(t *testing.T) {
	stages := []CanaryStage{
		{Name: "s1", Percent: 5, MinDuration: 0},
		{Name: "s2", Percent: 50, MinDuration: 0},
		{Name: "s3", Percent: 100, MinDuration: 0},
	}
	r, err := NewRollout(stages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.State() != RolloutPending {
		t.Errorf("初始应 Pending；got %s", r.State())
	}
	if r.CurrentPercent() != 0 {
		t.Errorf("Pending 时 percent=0；got %d", r.CurrentPercent())
	}

	if err := r.Advance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.CurrentPercent() != 5 {
		t.Errorf("Advance 1 次后应在 5%%；got %d", r.CurrentPercent())
	}
	if r.State() != RolloutInProgress {
		t.Errorf("应 InProgress；got %s", r.State())
	}
}

func TestRollout_AdvanceToCompletion(t *testing.T) {
	stages := []CanaryStage{
		{Name: "s1", Percent: 5, MinDuration: 0},
		{Name: "ga", Percent: 100, MinDuration: 0},
	}
	r, _ := NewRollout(stages, nil)

	r.Advance(context.Background()) // → s1
	r.Advance(context.Background()) // → ga (= 100% → Completed)
	if r.State() != RolloutCompleted {
		t.Errorf("应 Completed；got %s", r.State())
	}
	if r.CurrentPercent() != 100 {
		t.Errorf("Completed 时应 100%%；got %d", r.CurrentPercent())
	}
}

func TestRollout_DwellTimeEnforced(t *testing.T) {
	stages := []CanaryStage{
		{Name: "s1", Percent: 5, MinDuration: 1 * time.Hour},
		{Name: "s2", Percent: 100, MinDuration: 0},
	}
	r, _ := NewRollout(stages, nil)
	now := time.Now()
	r.now = func() time.Time { return now }

	r.Advance(context.Background()) // → s1
	// 立即 Advance 应被 MinDuration 拒绝
	if err := r.Advance(context.Background()); err == nil {
		t.Error("dwell 不足应被拒")
	}
	// 跳到 dwell 之后应可推进
	r.now = func() time.Time { return now.Add(2 * time.Hour) }
	if err := r.Advance(context.Background()); err != nil {
		t.Errorf("dwell 满足后应可推进；got %v", err)
	}
}

func TestRollout_HealthGateBlocksAdvance(t *testing.T) {
	gate := HealthFunc(func(_ context.Context, _ CanaryStage) error {
		return errors.New("error rate spiking")
	})
	stages := []CanaryStage{
		{Name: "s1", Percent: 5},
		{Name: "ga", Percent: 100},
	}
	r, _ := NewRollout(stages, gate)
	r.Advance(context.Background()) // → s1
	if err := r.Advance(context.Background()); err == nil {
		t.Error("health gate 失败应阻止推进")
	}
	if r.CurrentPercent() != 5 {
		t.Errorf("应仍在 5%%；got %d", r.CurrentPercent())
	}
}

func TestRollout_HealthGatePassAllows(t *testing.T) {
	gate := HealthFunc(func(_ context.Context, _ CanaryStage) error { return nil })
	stages := []CanaryStage{
		{Name: "s1", Percent: 5},
		{Name: "ga", Percent: 100},
	}
	r, _ := NewRollout(stages, gate)
	r.Advance(context.Background())
	if err := r.Advance(context.Background()); err != nil {
		t.Errorf("health 通过应能推进；got %v", err)
	}
}

func TestRollout_Rollback(t *testing.T) {
	stages := []CanaryStage{
		{Name: "s1", Percent: 5},
		{Name: "s2", Percent: 25},
		{Name: "ga", Percent: 100},
	}
	r, _ := NewRollout(stages, nil)
	r.Advance(context.Background()) // s1
	r.Advance(context.Background()) // s2
	if r.CurrentPercent() != 25 {
		t.Fatalf("先到 25%%；got %d", r.CurrentPercent())
	}
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.CurrentPercent() != 5 {
		t.Errorf("回退到 5%%；got %d", r.CurrentPercent())
	}
	if r.State() != RolloutRolledBack {
		t.Errorf("State 应 RolledBack")
	}
}

func TestRollout_RollbackPendingErrors(t *testing.T) {
	r, _ := NewRollout([]CanaryStage{{Percent: 5}}, nil)
	if err := r.Rollback(context.Background()); err == nil {
		t.Error("Pending 状态 Rollback 应报错")
	}
}

func TestRollout_RollbackCompletedErrors(t *testing.T) {
	stages := []CanaryStage{
		{Name: "ga", Percent: 100, MinDuration: 0},
	}
	r, _ := NewRollout(stages, nil)
	r.Advance(context.Background()) // → Completed
	if err := r.Rollback(context.Background()); err == nil {
		t.Error("Completed 后不应允许 Rollback")
	}
}

func TestRollout_MarkFailed(t *testing.T) {
	r, _ := NewRollout([]CanaryStage{{Percent: 5}, {Percent: 100}}, nil)
	r.Advance(context.Background())
	r.MarkFailed("auth provider down")
	if r.State() != RolloutFailed {
		t.Error("State 应 Failed")
	}
	if r.Err() == nil {
		t.Error("Err 应返回原因")
	}
}

func TestDefaultStages_HasFour(t *testing.T) {
	stages := DefaultStages()
	if len(stages) != 4 {
		t.Errorf("应有 4 阶段；got %d", len(stages))
	}
	if stages[0].Percent != 5 || stages[3].Percent != 100 {
		t.Errorf("默认阶段错；got %v", stages)
	}
}
