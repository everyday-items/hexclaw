package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 应用命令契约：统一 GradingJob（架构设计 §6.7 七条硬规则 + §4.10 幂等 + §5.4 字段）。
// 本轮只落领域+命令+HTTP 契约；真实编排器（worker pool/context 传播）下一轮接线。

func newGradingInput(sourceKey string) usecase.CreateGradingJobInput {
	return usecase.CreateGradingJobInput{
		SubmissionID:  "sub-1",
		SourceKind:    "im",
		SourceKey:     sourceKey,
		ModelSnapshot: k12.GradingModelSnapshot{Provider: "openrouter", Model: "test-vlm", Capability: "vision"},
	}
}

// advanceOK 推完一个自动阶段。
func advanceOK(t *testing.T, d usecase.Deps, agent, id, digest string) usecase.GradingJobView {
	t.Helper()
	v, err := d.AdvanceGradingStage(context.Background(), agent, id, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeOK, ArtifactDigest: digest,
	})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	return v
}

// driveToAwaiting 造一个 Job 并推进到 awaiting_confirmation。
func driveToAwaiting(t *testing.T, d usecase.Deps, agent string) usecase.GradingJobView {
	t.Helper()
	v, created, err := d.CreateGradingJob(context.Background(), agent, "s1", newGradingInput("msg-1"))
	if err != nil || !created {
		t.Fatalf("create: %v created=%v", err, created)
	}
	id := v.Record.RecordID
	advanceOK(t, d, agent, id, "")                 // queued → normalizing
	advanceOK(t, d, agent, id, "norm-digest")      // normalizing → recognizing
	v = advanceOK(t, d, agent, id, "recog-digest") // recognizing → awaiting_confirmation
	if v.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("应到 awaiting_confirmation, got %s", v.Record.Status)
	}
	return v
}

// driveToCompleted 推完全链（确认+锚点汇合后批改→渲染→投影→完成）。
func driveToCompleted(t *testing.T, d usecase.Deps, agent string) usecase.GradingJobView {
	t.Helper()
	ctx := context.Background()
	v := driveToAwaiting(t, d, agent)
	id := v.Record.RecordID
	if _, err := d.AdvanceGradingStage(ctx, agent, id, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated, ArtifactDigest: "anchor-digest",
	}); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	v2, err := d.ConfirmGradingJob(ctx, agent, id, []string{"q1 确认"})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if v2.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("确认+锚点汇合后应进 assessing, got %s", v2.Record.Status)
	}
	advanceOK(t, d, agent, id, "assess-digest") // assessing → rendering
	advanceOK(t, d, agent, id, "render-digest") // rendering → projecting
	v3 := advanceOK(t, d, agent, id, "proj-digest")
	if v3.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("应 completed, got %s", v3.Record.Status)
	}
	return v3
}

// TestGradingJobHappyPathFullChain 合法全链推进 + 每阶段检查点写入（规则 3 前半）。
func TestGradingJobHappyPathFullChain(t *testing.T) {
	d := newDataDeps(t, "mingming")
	v := driveToCompleted(t, d, "mingming")
	stages := map[string]bool{}
	for _, cp := range v.Fields.StageCheckpoints {
		stages[cp.Stage] = true
	}
	for _, want := range []string{
		k12.GradingStageNormalizing, k12.GradingStageRecognizing, k12.GradingStageLocating,
		k12.GradingStageAwaitingConfirmation, k12.GradingStageAssessing,
		k12.GradingStageRendering, k12.GradingStageProjecting,
	} {
		if !stages[want] {
			t.Errorf("缺少阶段检查点 %s: %v", want, v.Fields.StageCheckpoints)
		}
	}
}

// TestGradingJobRule1ConfirmWaitsForAnchor 规则 1：进入 assessing 前确认与锚点必须汇合——
// 先确认、锚点未回 → 停在 awaiting_confirmation；锚点回 located 后自动进 assessing。
func TestGradingJobRule1ConfirmWaitsForAnchor(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	v := driveToAwaiting(t, d, "mingming")
	id := v.Record.RecordID
	v2, err := d.ConfirmGradingJob(ctx, "mingming", id, nil)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if v2.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("锚点未汇合不得进 assessing, got %s", v2.Record.Status)
	}
	if v2.Fields.ConfirmationState != k12.GradingConfirmationConfirmed {
		t.Fatalf("confirmation_state 应 confirmed: %s", v2.Fields.ConfirmationState)
	}
	v3, err := d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated, ArtifactDigest: "anchor",
	})
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if v3.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("确认+锚点汇合后应进 assessing, got %s", v3.Record.Status)
	}
}

// TestGradingJobRule1AnchorDegradedDoesNotBlock 规则 1 + 等待态拆分裁决：
// 锚点超时降级 anchor_state=degraded（文字结果降级），不阻塞确认后进 assessing。
func TestGradingJobRule1AnchorDegradedDoesNotBlock(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	v := driveToAwaiting(t, d, "mingming")
	id := v.Record.RecordID
	if _, err := d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorDegraded,
	}); err != nil {
		t.Fatalf("anchor degraded: %v", err)
	}
	v2, err := d.ConfirmGradingJob(ctx, "mingming", id, nil)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if v2.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("degraded 不得阻塞批改, got %s", v2.Record.Status)
	}
	if v2.Fields.AnchorState != k12.GradingAnchorDegraded {
		t.Fatalf("anchor_state 应保持 degraded: %s", v2.Fields.AnchorState)
	}
}

// TestGradingJobRule2RenderingFailureDegrades 规则 2：rendering 失败降级为
// 「原图 + 文字列表」继续 projecting，不进 failed_retryable/failed_terminal。
func TestGradingJobRule2RenderingFailureDegrades(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	v := driveToAwaiting(t, d, "mingming")
	id := v.Record.RecordID
	d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated})
	if _, err := d.ConfirmGradingJob(ctx, "mingming", id, nil); err != nil {
		t.Fatal(err)
	}
	advanceOK(t, d, "mingming", id, "assess-digest") // → rendering
	v2, err := d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeFailed, FailureKind: "coordinate_check_failed", Retryable: false,
	})
	if err != nil {
		t.Fatalf("rendering 失败应降级续跑而非报错: %v", err)
	}
	if v2.Record.Status != k12.GradingStageProjecting {
		t.Fatalf("rendering 失败后应继续 projecting, got %s", v2.Record.Status)
	}
	degraded := false
	for _, cp := range v2.Fields.StageCheckpoints {
		if cp.Stage == k12.GradingStageRendering && cp.Degraded {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("rendering 降级应记录在检查点: %v", v2.Fields.StageCheckpoints)
	}
	if v2.Fields.FailureKind != "" {
		t.Fatalf("降级不是失败态，failure_kind 应空: %s", v2.Fields.FailureKind)
	}
}

// TestGradingJobRule3CheckpointResume 规则 3：失败重试从最近成功阶段恢复，
// 禁止重跑已成功阶段（queued 起跑直达检查点后继阶段）。
func TestGradingJobRule3CheckpointResume(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	v := driveToAwaiting(t, d, "mingming")
	id := v.Record.RecordID
	d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated})
	if _, err := d.ConfirmGradingJob(ctx, "mingming", id, nil); err != nil {
		t.Fatal(err)
	}
	// assessing 阶段可重试失败
	v2, err := d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeFailed, FailureKind: "model_timeout", Retryable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Record.Status != k12.GradingStageFailedRetryable {
		t.Fatalf("应 failed_retryable, got %s", v2.Record.Status)
	}
	v3, err := d.RetryGradingJob(ctx, "mingming", id)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if v3.Record.Status != k12.GradingStageQueued {
		t.Fatalf("重试应回 queued, got %s", v3.Record.Status)
	}
	// queued 起跑：直达 assessing，不重跑 normalizing/recognizing/确认
	v4 := advanceOK(t, d, "mingming", id, "")
	if v4.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("检查点恢复应直达 assessing（禁止重跑已成功阶段）, got %s", v4.Record.Status)
	}
}

// TestGradingJobRule4RetryLimit 规则 4：达到阶段重试上限进入 failed_terminal 并写明 failure_kind；
// 不可重试错误同样收敛 failed_terminal，不允许无限循环。
func TestGradingJobRule4RetryLimit(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	v := driveToAwaiting(t, d, "mingming")
	id := v.Record.RecordID
	d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated})
	if _, err := d.ConfirmGradingJob(ctx, "mingming", id, nil); err != nil {
		t.Fatal(err)
	}
	var last usecase.GradingJobView
	for i := 0; i < k12.GradingMaxStageAttempts; i++ {
		var err error
		last, err = d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{
			Outcome: usecase.GradingOutcomeFailed, FailureKind: "model_timeout", Retryable: true,
		})
		if err != nil {
			t.Fatalf("fail #%d: %v", i+1, err)
		}
		if last.Record.Status == k12.GradingStageFailedTerminal {
			break
		}
		if _, err := d.RetryGradingJob(ctx, "mingming", id); err != nil {
			t.Fatalf("retry #%d: %v", i+1, err)
		}
		advanceOK(t, d, "mingming", id, "") // queued → assessing
	}
	if last.Record.Status != k12.GradingStageFailedTerminal {
		t.Fatalf("达重试上限应 failed_terminal, got %s", last.Record.Status)
	}
	if last.Fields.FailureKind != "model_timeout" {
		t.Fatalf("failed_terminal 必须写明 failure_kind: %q", last.Fields.FailureKind)
	}
	if _, err := d.RetryGradingJob(ctx, "mingming", id); err == nil {
		t.Fatalf("failed_terminal 不得再重试")
	}

	// 不可重试错误：一次即收敛 failed_terminal
	v2, _, err := d.CreateGradingJob(ctx, "mingming", "s1", newGradingInput("msg-2"))
	if err != nil {
		t.Fatal(err)
	}
	advanceOK(t, d, "mingming", v2.Record.RecordID, "")
	v3, err := d.AdvanceGradingStage(ctx, "mingming", v2.Record.RecordID, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeFailed, FailureKind: "image_unreadable", Retryable: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v3.Record.Status != k12.GradingStageFailedTerminal {
		t.Fatalf("不可重试错误应收敛 failed_terminal, got %s", v3.Record.Status)
	}
}

// TestGradingJobRule5CancelBoundary 规则 5：rendering/projecting 是不可取消的短收尾阶段；
// 其余阶段可取消。取消向在途模型调用的传播（context 取消）由编排器执行，下一轮接线。
func TestGradingJobRule5CancelBoundary(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	// 可取消：awaiting_confirmation（规则 7：家长可随时显式取消）
	v := driveToAwaiting(t, d, "mingming")
	got, err := d.CancelGradingJob(ctx, "mingming", v.Record.RecordID)
	if err != nil || got.Record.Status != k12.GradingStageCancelled {
		t.Fatalf("awaiting_confirmation 应可取消: %v %v", err, got.Record)
	}

	// 不可取消：rendering
	d2 := newDataDeps(t, "gege")
	v2 := driveToAwaiting(t, d2, "gege")
	id := v2.Record.RecordID
	d2.AdvanceGradingStage(ctx, "gege", id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated})
	if _, err := d2.ConfirmGradingJob(ctx, "gege", id, nil); err != nil {
		t.Fatal(err)
	}
	advanceOK(t, d2, "gege", id, "assess") // → rendering
	if _, err := d2.CancelGradingJob(ctx, "gege", id); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("rendering 取消应被拒(ErrIllegalTransition), got %v", err)
	}
	advanceOK(t, d2, "gege", id, "render") // → projecting
	if _, err := d2.CancelGradingJob(ctx, "gege", id); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("projecting 取消应被拒, got %v", err)
	}
}

// TestGradingJobRule6ReviseCreatesNewJob 规则 6：completed 后修正 → confirmed_version+1 新 Job，
// 同 submission、新幂等键、检查点预置（首个非 queued 态 = assessing）；旧 Job 不回退。
func TestGradingJobRule6ReviseCreatesNewJob(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	old := driveToCompleted(t, d, "mingming")
	oldID := old.Record.RecordID

	nv, created, err := d.ReviseGradingJob(ctx, "mingming", oldID, "s2", []string{"q1 改为 42"})
	if err != nil || !created {
		t.Fatalf("revise: %v created=%v", err, created)
	}
	if nv.Record.RecordID == oldID {
		t.Fatalf("修正必须创建新 Job")
	}
	if nv.Fields.ConfirmedVersion != old.Fields.ConfirmedVersion+1 {
		t.Fatalf("confirmed_version 应 +1: %d", nv.Fields.ConfirmedVersion)
	}
	if nv.Fields.IdempotencyKey == old.Fields.IdempotencyKey {
		t.Fatalf("幂等键必须含 confirmed_version（§4.10）")
	}
	if nv.Fields.SubmissionID != old.Fields.SubmissionID {
		t.Fatalf("新 Job 应同 submission")
	}
	if nv.Record.Status != k12.GradingStageQueued {
		t.Fatalf("新 Job 起于 queued, got %s", nv.Record.Status)
	}
	// 检查点捷径：首个非 queued 态 = assessing
	v2 := advanceOK(t, d, "mingming", nv.Record.RecordID, "")
	if v2.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("修正重批应走 queued→assessing 捷径, got %s", v2.Record.Status)
	}
	// 旧 Job 不回退
	oldNow, err := d.GetGradingJob(ctx, "mingming", oldID)
	if err != nil || oldNow.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("旧 Job 不得回退: %v %s", err, oldNow.Record.Status)
	}
	// 同一修正重复提交幂等：返回既有新 Job
	nv2, created2, err := d.ReviseGradingJob(ctx, "mingming", oldID, "s2", []string{"q1 改为 42"})
	if err != nil || created2 || nv2.Record.RecordID != nv.Record.RecordID {
		t.Fatalf("重复修正应幂等命中既有新 Job: %v created=%v id=%s", err, created2, nv2.Record.RecordID)
	}
}

// TestGradingJobRule7NoAutoExpiry 规则 7 + §5.4 deadline：awaiting_confirmation 不设自动过期，
// 人工等待不计入 deadline（deadline=0）；确认恢复后按剩余阶段预算重新起算。
func TestGradingJobRule7NoAutoExpiry(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	v := driveToAwaiting(t, d, "mingming")
	if v.Fields.Deadline != 0 {
		t.Fatalf("awaiting_confirmation 人工等待不计入 deadline, got %d", v.Fields.Deadline)
	}
	id := v.Record.RecordID
	d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated})
	v2, err := d.ConfirmGradingJob(ctx, "mingming", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Fields.Deadline == 0 {
		t.Fatalf("确认恢复后 deadline 应重新起算")
	}
}

// TestGradingJobIdempotentCreate §4.10：同一幂等键只产生一个 GradingJob，同键返回既有 Job。
func TestGradingJobIdempotentCreate(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	v1, created1, err := d.CreateGradingJob(ctx, "mingming", "s1", newGradingInput("msg-1"))
	if err != nil || !created1 {
		t.Fatalf("首次创建: %v %v", err, created1)
	}
	v2, created2, err := d.CreateGradingJob(ctx, "mingming", "s1", newGradingInput("msg-1"))
	if err != nil {
		t.Fatal(err)
	}
	if created2 || v2.Record.RecordID != v1.Record.RecordID {
		t.Fatalf("同幂等键应返回既有 Job: created=%v id=%s want %s", created2, v2.Record.RecordID, v1.Record.RecordID)
	}
}

// TestGradingJobOwnershipIsolation 归属隔离：跨实例不可见、不可取消（多孩隔离硬边界）。
func TestGradingJobOwnershipIsolation(t *testing.T) {
	d := newDataDeps(t, "mingming", "gege")
	ctx := context.Background()
	v, _, err := d.CreateGradingJob(ctx, "mingming", "s1", newGradingInput("msg-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetGradingJob(ctx, "gege", v.Record.RecordID); err == nil {
		t.Fatalf("跨实例读取应拒绝")
	}
	if _, err := d.CancelGradingJob(ctx, "gege", v.Record.RecordID); err == nil {
		t.Fatalf("跨实例取消应拒绝")
	}
	jobs, err := d.ListGradingJobs(ctx, "gege", "")
	if err != nil || len(jobs) != 0 {
		t.Fatalf("跨实例列表应为空: %v %d", err, len(jobs))
	}
}

// TestGradingJobIllegalCommands 非法命令拒绝：awaiting_confirmation 不可用 advance ok 跳过确认；
// 非等待态 confirm 拒绝；未失败态 retry 拒绝；revise 只允许 completed。
func TestGradingJobIllegalCommands(t *testing.T) {
	d := newDataDeps(t, "mingming")
	ctx := context.Background()
	v := driveToAwaiting(t, d, "mingming")
	id := v.Record.RecordID
	if _, err := d.AdvanceGradingStage(ctx, "mingming", id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeOK}); err == nil {
		t.Fatalf("awaiting_confirmation 不得用 advance 跳过家长确认")
	}
	if _, err := d.RetryGradingJob(ctx, "mingming", id); err == nil {
		t.Fatalf("非 failed_retryable 不得 retry")
	}
	if _, _, err := d.ReviseGradingJob(ctx, "mingming", id, "s2", nil); err == nil {
		t.Fatalf("非 completed 不得 revise（规则 6 限定 completed 之后）")
	}
	v2, _, err := d.CreateGradingJob(ctx, "mingming", "s1", newGradingInput("msg-9"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConfirmGradingJob(ctx, "mingming", v2.Record.RecordID, nil); err == nil {
		t.Fatalf("queued 态不得 confirm")
	}
}
