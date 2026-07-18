package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 钉钉入口 Job 化契约（执行计划 §3.4「入口自编排」钉钉侧收敛）：
// 收到照片 → CreateGradingJob → 编排器推进 → awaiting_confirmation 按现有 IM 交互
// （无确认停留 = 立即按识别结果确认）→ completed 后按现有投递逻辑发结果；
// 行为对家长不变。Job 创建失败回退旧自编排路径（一次切换前的过渡降级）。

type fakeK12PhotoJobOrchestrator struct {
	startCalls   int
	startInput   k12usecase.StartPhotoGradingInput
	startErr     error
	runErr       error
	runStage     string // RunGradingJob 停点 stage
	confirmCalls int
	released     []string
	result       k12usecase.PhotoGradeResult
}

func (f *fakeK12PhotoJobOrchestrator) view(stage string) k12usecase.GradingJobView {
	return k12usecase.GradingJobView{Record: &records.AgentRecord{RecordID: "job-1", Status: stage}}
}

func (f *fakeK12PhotoJobOrchestrator) StartPhotoGradingJob(_ context.Context, in k12usecase.StartPhotoGradingInput) (k12usecase.GradingJobView, bool, error) {
	f.startCalls++
	f.startInput = in
	if f.startErr != nil {
		return k12usecase.GradingJobView{}, false, f.startErr
	}
	return f.view(k12.GradingStageQueued), true, nil
}

func (f *fakeK12PhotoJobOrchestrator) RunGradingJob(context.Context, string) (k12usecase.GradingJobView, error) {
	if f.runErr != nil {
		return f.view(k12.GradingStageFailedRetryable), f.runErr
	}
	stage := f.runStage
	if stage == "" {
		stage = k12.GradingStageAwaitingConfirmation
	}
	return f.view(stage), nil
}

func (f *fakeK12PhotoJobOrchestrator) ConfirmAndRun(_ context.Context, _ string, corrections []string) (k12usecase.GradingJobView, error) {
	f.confirmCalls++
	if len(corrections) != 0 {
		return k12usecase.GradingJobView{}, errors.New("现有钉钉交互没有逐题修正，corrections 应为空（全按识别结果确认）")
	}
	return f.view(k12.GradingStageCompleted), nil
}

func (f *fakeK12PhotoJobOrchestrator) PhotoResult(string) (k12usecase.PhotoGradeResult, bool) {
	return f.result, true
}

func (f *fakeK12PhotoJobOrchestrator) ReleaseGradingRun(jobID string) {
	f.released = append(f.released, jobID)
}

// TestMaybeHandleK12DingtalkPhotoJob_RoutesThroughGradingJob 照片经统一 GradingJob 推进：
// Job 创建（source_kind=im, source_key=message_id）→ 停点自动确认 → completed 投递，
// 消息文案与附件行为与旧路径一致，旧自编排 process 不再被调用。
func TestMaybeHandleK12DingtalkPhotoJob_RoutesThroughGradingJob(t *testing.T) {
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	orch := &fakeK12PhotoJobOrchestrator{result: k12usecase.PhotoGradeResult{
		Mode:     k12usecase.PhotoModeGrade,
		Markdown: "## 作业批改完成",
		AnnotatedImage: &k12usecase.RenderedPhoto{
			Data: []byte("corrected png bytes"), MIME: "image/png",
		},
	}}
	legacyCalls := 0
	process := func(context.Context, k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		legacyCalls++
		return k12usecase.PhotoGradeResult{}, nil
	}

	reply, handled, err := maybeHandleK12DingtalkPhotoJob(context.Background(), k12PhotoTestMessage(), router, orch, process)
	if err != nil || !handled {
		t.Fatalf("job 路径应接管: handled=%v err=%v", handled, err)
	}
	if legacyCalls != 0 {
		t.Fatalf("Job 链路成功时不得再走旧自编排路径: %d", legacyCalls)
	}
	if orch.startCalls != 1 || orch.confirmCalls != 1 {
		t.Fatalf("应恰好创建 1 个 Job 并确认 1 次: start=%d confirm=%d", orch.startCalls, orch.confirmCalls)
	}
	in := orch.startInput
	if in.SourceKind != "im" || in.SourceKey != "msg-1" {
		t.Fatalf("统一幂等键来源应为 im/message_id（§4.10）: %+v", in)
	}
	if in.Photo.AgentName != "child-tutor" || in.Photo.Grade != "五年级下" || string(in.Photo.Image) != "image" {
		t.Fatalf("路由与图片入参丢失: %#v", in.Photo)
	}
	if reply == nil || reply.Content != "## 作业批改完成" {
		t.Fatalf("消息文案必须保持旧投递行为: %#v", reply)
	}
	if len(reply.Attachments) != 1 || reply.Attachments[0].Name != "批改后的作业.png" {
		t.Fatalf("批改图附件行为必须保持: %#v", reply.Attachments)
	}
	if len(orch.released) != 1 || orch.released[0] != "job-1" {
		t.Fatalf("投递后应释放编排器运行时: %v", orch.released)
	}
}

// TestMaybeHandleK12DingtalkPhotoJob_CreateFailureFallsBackToLegacy Job 创建失败 →
// 回退旧自编排路径（过渡降级），家长仍拿到批改结果。
func TestMaybeHandleK12DingtalkPhotoJob_CreateFailureFallsBackToLegacy(t *testing.T) {
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	orch := &fakeK12PhotoJobOrchestrator{startErr: errors.New("records store unavailable")}
	legacyCalls := 0
	process := func(context.Context, k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		legacyCalls++
		return k12usecase.PhotoGradeResult{Mode: k12usecase.PhotoModeGrade, Markdown: "## 旧路径批改"}, nil
	}

	reply, handled, err := maybeHandleK12DingtalkPhotoJob(context.Background(), k12PhotoTestMessage(), router, orch, process)
	if err != nil || !handled || legacyCalls != 1 {
		t.Fatalf("Job 创建失败应回退旧路径: handled=%v legacy=%d err=%v", handled, legacyCalls, err)
	}
	if reply == nil || reply.Content != "## 旧路径批改" {
		t.Fatalf("回退后仍须投递批改结果: %#v", reply)
	}
}

// TestMaybeHandleK12DingtalkPhotoJob_PipelineFailureSurfacesError Job 已创建但阶段失败
// （如识别失败落 failed_retryable）→ 与旧路径同样的错误面返回，不得二次跑旧路径重复调模型。
func TestMaybeHandleK12DingtalkPhotoJob_PipelineFailureSurfacesError(t *testing.T) {
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	stageErr := errors.New("vision provider timeout")
	orch := &fakeK12PhotoJobOrchestrator{runErr: stageErr}
	legacyCalls := 0
	process := func(context.Context, k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		legacyCalls++
		return k12usecase.PhotoGradeResult{}, nil
	}

	reply, handled, err := maybeHandleK12DingtalkPhotoJob(context.Background(), k12PhotoTestMessage(), router, orch, process)
	if !handled || !errors.Is(err, stageErr) {
		t.Fatalf("阶段失败应保持旧错误面: handled=%v err=%v", handled, err)
	}
	if legacyCalls != 0 || reply != nil {
		t.Fatalf("Job 已创建后失败不得重复调模型走旧路径: legacy=%d reply=%#v", legacyCalls, reply)
	}
}

// TestMaybeHandleK12DingtalkPhotoJob_NilOrchestratorUsesLegacyPath 编排器未装配（K12 未启用
// GradingJob 运行时）时保持旧路径行为。
func TestMaybeHandleK12DingtalkPhotoJob_NilOrchestratorUsesLegacyPath(t *testing.T) {
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	legacyCalls := 0
	process := func(context.Context, k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		legacyCalls++
		return k12usecase.PhotoGradeResult{Mode: k12usecase.PhotoModeSolve, Markdown: "## 作业解题"}, nil
	}
	reply, handled, err := maybeHandleK12DingtalkPhotoJob(context.Background(), k12PhotoTestMessage(), router, nil, process)
	if err != nil || !handled || legacyCalls != 1 || reply == nil {
		t.Fatalf("无编排器应走旧路径: handled=%v legacy=%d err=%v", handled, legacyCalls, err)
	}
}

// TestMaybeHandleK12DingtalkPhotoJob_UnmatchedMessageFallsThrough 路由门禁与旧入口一致：
// 未显式绑定 K12 辅导 Agent 的图片不得被接管。
func TestMaybeHandleK12DingtalkPhotoJob_UnmatchedMessageFallsThrough(t *testing.T) {
	router := k12PhotoTestRouter(t, false, "k12-tutor")
	orch := &fakeK12PhotoJobOrchestrator{}
	process := func(context.Context, k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		t.Fatal("未绑定消息不得进入批改")
		return k12usecase.PhotoGradeResult{}, nil
	}
	reply, handled, err := maybeHandleK12DingtalkPhotoJob(context.Background(), k12PhotoTestMessage(), router, orch, process)
	if err != nil || handled || reply != nil || orch.startCalls != 0 {
		t.Fatalf("未绑定图片应放行给通用引擎: handled=%v start=%d err=%v", handled, orch.startCalls, err)
	}
}
