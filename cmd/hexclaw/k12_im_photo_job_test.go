package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type fakeK12ImageTaskFacade struct {
	events         []string
	createInput    k12usecase.CreateImageTaskInput
	createErr      error
	rejectStart    bool
	dispatchStatus k12.ImageTaskStatus
	startCalls     int
	getCalls       int
	confirmCalls   int
	result         k12usecase.ImageTaskResult
}

type fakeK12InboundPhotoRuntime struct {
	*fakeK12ImageTaskFacade
	bundles         []k12usecase.InboundPhotoBundle
	confirmed       []k12usecase.InboundPhotoRoutingDecision
	recoverErr      error
	confirmationErr error
}

func (f *fakeK12InboundPhotoRuntime) Recoverable(
	_ context.Context, _ int,
) ([]k12usecase.InboundPhotoBundle, error) {
	if f.recoverErr != nil {
		return nil, f.recoverErr
	}
	return append([]k12usecase.InboundPhotoBundle(nil), f.bundles...), nil
}

func (f *fakeK12InboundPhotoRuntime) ConfirmRouting(
	_ context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	decision k12usecase.InboundPhotoRoutingDecision,
) (k12usecase.InboundPhotoDispatch, error) {
	if f.confirmationErr != nil {
		return k12usecase.InboundPhotoDispatch{}, f.confirmationErr
	}
	if len(f.bundles) != 1 || agentName != f.bundles[0].Receipt.AgentName ||
		receiptID != f.bundles[0].Receipt.ReceiptID ||
		expectedVersion != f.bundles[0].Dispatch.Version {
		return k12usecase.InboundPhotoDispatch{}, errors.New("unexpected routing confirmation identity")
	}
	f.confirmed = append(f.confirmed, decision)
	f.bundles[0].Dispatch.RoutingDecision = decision
	f.bundles[0].Dispatch.ConfirmationStatus = k12storage.InboundPhotoConfirmationConfirmed
	f.bundles[0].Dispatch.Version++
	return f.bundles[0].Dispatch, nil
}

func pendingInboundPhotoBundle() k12usecase.InboundPhotoBundle {
	return k12usecase.InboundPhotoBundle{
		Receipt: k12usecase.InboundPhotoReceipt{
			ReceiptID: "receipt-photo-1", AgentName: "child-tutor",
			Identity: k12usecase.InboundPhotoIdentity{
				Platform: "dingtalk", InstanceID: "bot-1", ChatID: "family-group",
				ProviderMessageID: "photo-msg-1",
			},
		},
		Dispatch: k12usecase.InboundPhotoDispatch{
			ReceiptID: "receipt-photo-1",
			InboundPhotoDispatchState: k12usecase.InboundPhotoDispatchState{
				ProcessingStatus:   k12storage.InboundPhotoAdmitted,
				RoutingDecision:    k12storage.InboundPhotoRouteAskedUser,
				ConfirmationStatus: k12storage.InboundPhotoConfirmationWaiting,
				ReplyStatus:        k12storage.InboundPhotoReplyPending,
			},
			Version: 3,
		},
	}
}

func (f *fakeK12ImageTaskFacade) PersistPageAsset(
	_ context.Context,
	ownerScope, agentName string,
	data []byte,
) (k12usecase.ReadyPageAsset, error) {
	f.events = append(f.events, "persist")
	inspection, err := assetstore.Inspect(agentName, data)
	if err != nil {
		return k12usecase.ReadyPageAsset{}, err
	}
	if _, err := assetstore.Save(agentName, data); err != nil {
		return k12usecase.ReadyPageAsset{}, err
	}
	return k12usecase.ReadyPageAsset{
		Metadata: k12storage.PageAssetMetadata{
			OwnerScope: ownerScope, AgentName: agentName,
			PageAssetID: inspection.AssetID,
		},
		Data: append([]byte(nil), data...),
	}, nil
}

func (f *fakeK12ImageTaskFacade) Create(
	_ context.Context,
	in k12usecase.CreateImageTaskInput,
) (k12usecase.ImageTaskView, bool, error) {
	f.events = append(f.events, "create")
	f.createInput = in
	if f.createErr != nil {
		return k12usecase.ImageTaskView{}, false, f.createErr
	}
	return k12usecase.ImageTaskView{Dispatch: k12.ImageTaskDispatch{
		DispatchID: "dispatch-1", AgentName: in.AgentName, LearnerID: in.LearnerID,
		Status: k12.ImageTaskStatusRouted, TaskIntent: k12.ImageTaskIntentCompletedHomework,
		Version: 2,
	}}, true, nil
}

func (f *fakeK12ImageTaskFacade) StartAsync(agentName, dispatchID string) bool {
	f.events = append(f.events, "start")
	f.startCalls++
	return !f.rejectStart && dispatchID == "dispatch-1"
}

func (*fakeK12ImageTaskFacade) Retry(context.Context, string, string, int) (k12usecase.ImageTaskView, error) {
	return k12usecase.ImageTaskView{}, errors.New("unexpected retry")
}

func (f *fakeK12ImageTaskFacade) Get(
	_ context.Context,
	agentName, dispatchID string,
) (k12usecase.ImageTaskView, error) {
	f.events = append(f.events, "get")
	f.getCalls++
	if agentName != "child-tutor" || dispatchID != "dispatch-1" {
		return k12usecase.ImageTaskView{}, errors.New("unexpected scope")
	}
	if f.dispatchStatus != "" {
		return k12usecase.ImageTaskView{Dispatch: k12.ImageTaskDispatch{
			DispatchID: dispatchID, AgentName: agentName, LearnerID: agentName,
			Status: f.dispatchStatus, Version: 2,
		}}, nil
	}
	if f.result.Kind == "creative" {
		return k12usecase.ImageTaskView{Dispatch: k12.ImageTaskDispatch{
			DispatchID: dispatchID, AgentName: agentName, LearnerID: agentName,
			Status: k12.ImageTaskStatusRouted, TaskIntent: k12.ImageTaskIntentArtwork,
			Version: 2,
		}}, nil
	}
	return k12usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			DispatchID: dispatchID, AgentName: agentName, LearnerID: agentName,
			Status: k12.ImageTaskStatusRouted, TaskIntent: k12.ImageTaskIntentCompletedHomework,
			Version: 2,
		},
		HomeworkProjection: &k12usecase.ImageTaskHomeworkProjection{
			Stage: k12.GradingStageAwaitingConfirmation,
		},
	}, nil
}

func TestWaitK12IMImageTaskResult_AwaitingConfirmationReturnsApprovedChoiceMessage(t *testing.T) {
	facade := &fakeK12ImageTaskFacade{
		dispatchStatus: k12.ImageTaskStatusAwaitingConfirmation,
	}

	reply, err := waitK12IMImageTaskResult(
		context.Background(), facade, "child-tutor", "dispatch-1",
	)
	if err != nil {
		t.Fatalf("待确认必须返回可操作消息而不是通用错误: %v", err)
	}
	if reply == nil || reply.Content != "回复 1=练习卷回传，2=新作业批改" {
		t.Fatalf("待确认消息必须复用已批准的二选一语义: %#v", reply)
	}
	if reply.Interactive == nil || len(reply.Interactive.Buttons) != 2 ||
		reply.Interactive.Buttons[0].Action != "regrade" ||
		reply.Interactive.Buttons[1].Action != "new_submission" {
		t.Fatalf("actionCard 动作必须与文字 1/2 等价: %#v", reply.Interactive)
	}
}

func TestMaybeHandleK12DingtalkPhoto_RoutingConfirmationMapsTextAndActionWithoutNewTask(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		action   string
		decision k12usecase.InboundPhotoRoutingDecision
	}{
		{name: "text one", content: " 1 ", decision: k12usecase.InboundPhotoRouteRegrade},
		{name: "text two", content: "2", decision: k12usecase.InboundPhotoRouteNewSubmission},
		{name: "action regrade", action: "regrade", decision: k12usecase.InboundPhotoRouteRegrade},
		{name: "action new submission", action: "new_submission", decision: k12usecase.InboundPhotoRouteNewSubmission},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &fakeK12InboundPhotoRuntime{
				fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{},
				bundles:                []k12usecase.InboundPhotoBundle{pendingInboundPhotoBundle()},
			}
			msg := k12PhotoTestMessage()
			msg.ID = "confirm-msg-1"
			msg.Content = tt.content
			msg.Attachments = nil
			if tt.action != "" {
				msg.Metadata = map[string]string{"interactive_action": tt.action}
			}

			reply, handled, err := maybeHandleK12DingtalkPhoto(
				context.Background(), msg,
				k12PhotoTestRouter(t, true, "k12-tutor"), runtime,
			)
			if err != nil || !handled || reply != nil {
				t.Fatalf("确认动作必须只推进耐久路由: handled=%v reply=%#v err=%v", handled, reply, err)
			}
			if len(runtime.confirmed) != 1 || runtime.confirmed[0] != tt.decision {
				t.Fatalf("确认映射错误: got=%v want=%s", runtime.confirmed, tt.decision)
			}
			if len(runtime.events) != 0 || runtime.startCalls != 0 {
				t.Fatalf("确认回复不得创建或启动第二个 ImageTask: events=%v", runtime.events)
			}
		})
	}
}

func TestMaybeHandleK12DingtalkPhoto_RoutingConfirmationReplayAfterRestartIsNoOp(t *testing.T) {
	runtime := &fakeK12InboundPhotoRuntime{
		fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{},
		bundles:                []k12usecase.InboundPhotoBundle{pendingInboundPhotoBundle()},
	}
	msg := k12PhotoTestMessage()
	msg.ID = "confirm-msg-replayed"
	msg.Content = "1"
	msg.Attachments = nil
	router := k12PhotoTestRouter(t, true, "k12-tutor")

	for attempt := 1; attempt <= 2; attempt++ {
		reply, handled, err := maybeHandleK12DingtalkPhoto(
			context.Background(), msg, router, runtime,
		)
		if err != nil || !handled || reply != nil {
			t.Fatalf("attempt %d replay must be handled no-op: handled=%v reply=%#v err=%v",
				attempt, handled, reply, err)
		}
	}
	if len(runtime.confirmed) != 1 || runtime.confirmed[0] != k12usecase.InboundPhotoRouteRegrade {
		t.Fatalf("replayed confirmation must advance V88 exactly once: %v", runtime.confirmed)
	}
	if len(runtime.events) != 0 || runtime.startCalls != 0 {
		t.Fatalf("confirmation replay must not create ImageTask: %v", runtime.events)
	}
}

func TestMaybeHandleK12DingtalkPhoto_RoutingConfirmationRequiresExactDirectChoice(t *testing.T) {
	tests := []struct {
		name    string
		content string
		meta    map[string]string
	}{
		{name: "ambiguous text", content: "1 please"},
		{name: "unknown action", meta: map[string]string{"interactive_action": "retry"}},
		{name: "group action", meta: map[string]string{
			"interactive_action": "regrade", "conversation_type": "2",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &fakeK12InboundPhotoRuntime{
				fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{},
				bundles:                []k12usecase.InboundPhotoBundle{pendingInboundPhotoBundle()},
			}
			msg := k12PhotoTestMessage()
			msg.Content = tt.content
			msg.Attachments = nil
			msg.Metadata = tt.meta

			reply, handled, err := maybeHandleK12DingtalkPhoto(
				context.Background(), msg,
				k12PhotoTestRouter(t, true, "k12-tutor"), runtime,
			)
			if err != nil || handled || reply != nil || len(runtime.confirmed) != 0 ||
				len(runtime.events) != 0 {
				t.Fatalf("非精确 direct 选择不得推进图片任务: handled=%v reply=%#v confirmed=%v events=%v err=%v",
					handled, reply, runtime.confirmed, runtime.events, err)
			}
		})
	}
}

func TestMaybeHandleK12DingtalkPhoto_NewImageWhileRoutePendingDoesNotCreateSecondTask(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	runtime := &fakeK12InboundPhotoRuntime{
		fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{},
		bundles:                []k12usecase.InboundPhotoBundle{pendingInboundPhotoBundle()},
	}
	msg := k12PhotoTestMessage()
	msg.ID = "photo-msg-2"

	reply, handled, err := maybeHandleK12DingtalkPhoto(
		ctx, msg,
		k12PhotoTestRouter(t, true, "k12-tutor"), runtime,
	)
	if err != nil || !handled || reply == nil ||
		reply.Content != "回复 1=练习卷回传，2=新作业批改" {
		t.Fatalf("存在待确认路由时必须继续同一安全确认: handled=%v reply=%#v err=%v", handled, reply, err)
	}
	if len(runtime.events) != 0 || runtime.startCalls != 0 {
		t.Fatalf("后续图片不得创建或启动第二个 ImageTask: events=%v", runtime.events)
	}
	if len(runtime.confirmed) != 0 {
		t.Fatalf("图片本身不得静默确认分流: %v", runtime.confirmed)
	}
}

func TestMaybeHandleK12DingtalkPhoto_RoutingStateReadFailureIsFailClosed(t *testing.T) {
	runtime := &fakeK12InboundPhotoRuntime{
		fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{},
		recoverErr:             errors.New("routing store unavailable"),
	}
	reply, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), k12PhotoTestMessage(),
		k12PhotoTestRouter(t, true, "k12-tutor"), runtime,
	)
	if !handled || reply != nil || !errors.Is(err, runtime.recoverErr) {
		t.Fatalf("无法核验待确认状态时必须 fail closed: handled=%v reply=%#v err=%v",
			handled, reply, err)
	}
	if len(runtime.events) != 0 || runtime.startCalls != 0 {
		t.Fatalf("路由状态读取失败不得创建或启动 ImageTask: events=%v", runtime.events)
	}
}

func (f *fakeK12ImageTaskFacade) Confirm(
	_ context.Context,
	in k12usecase.ConfirmImageTaskInput,
) (k12usecase.ImageTaskView, error) {
	f.events = append(f.events, "confirm")
	f.confirmCalls++
	if in.AgentName != "child-tutor" || in.DispatchID != "dispatch-1" ||
		in.ExpectedVersion != 2 || in.Intent != k12.ImageTaskIntentCompletedHomework {
		return k12usecase.ImageTaskView{}, errors.New("unexpected confirmation")
	}
	return k12usecase.ImageTaskView{}, nil
}

func (f *fakeK12ImageTaskFacade) Result(
	_ context.Context,
	agentName, dispatchID string,
) (k12usecase.ImageTaskResult, error) {
	f.events = append(f.events, "result")
	if f.confirmCalls == 0 && f.result.Kind != "creative" {
		return k12usecase.ImageTaskResult{Kind: "pending"}, nil
	}
	return f.result, nil
}

func TestMaybeHandleK12DingtalkPhoto_RoutesOnlyThroughImageTaskFacade(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	facade := &fakeK12ImageTaskFacade{result: k12usecase.ImageTaskResult{
		Kind: string(k12.ImageTaskIntentCompletedHomework),
		Photo: &k12usecase.PhotoGradeResult{
			Mode: k12usecase.PhotoModeGrade, Markdown: "## 作业批改完成",
			AnnotatedImage: &k12usecase.RenderedPhoto{
				Data: []byte("corrected png bytes"), MIME: "image/png",
			},
		},
	}}
	runtime := &fakeK12InboundPhotoRuntime{fakeK12ImageTaskFacade: facade}

	reply, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), k12PhotoTestMessage(), router, runtime,
	)
	if err != nil || !handled {
		t.Fatalf("ImageTask 路径应接管: handled=%v err=%v", handled, err)
	}
	if got := strings.Join(facade.events, ","); got != "persist,create,start,get,confirm,result" {
		t.Fatalf("入口必须先固化 dispatch，再启动并只经统一门面推进: %s", got)
	}
	in := facade.createInput
	if in.SourceKind != k12.ImageTaskSourceIM || in.SourceRef != "msg-1" ||
		in.SourceSessionID != "family-group" || in.AgentName != "child-tutor" ||
		in.LearnerID != "child-tutor" || in.AttemptGeneration != 1 ||
		in.OwnerScope != k12usecase.DefaultLocalOwnerScope {
		t.Fatalf("ImageTask 来源/幂等身份丢失: %+v", in)
	}
	if len(in.SourceAssetRefs) != 1 {
		t.Fatalf("图片必须先进入不可变资产存储: %+v", in.SourceAssetRefs)
	}
	owner, ok := assetstore.OwnerOf(in.SourceAssetRefs[0])
	if !ok || owner != "child-tutor" {
		t.Fatalf("资产 owner=%q ok=%v", owner, ok)
	}
	if facade.startCalls != 1 || facade.confirmCalls != 1 {
		t.Fatalf("启动/现有 IM 自动确认语义错误: start=%d confirm=%d",
			facade.startCalls, facade.confirmCalls)
	}
	if reply == nil || reply.Content != "## 作业批改完成" ||
		len(reply.Attachments) != 1 || reply.Attachments[0].Name != "批改后的作业.png" {
		t.Fatalf("统一门面结果未按既有钉钉投影输出: %#v", reply)
	}
}

func TestMaybeHandleK12DingtalkPhoto_CreateFailureNeverFallsBackToLegacyProvider(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	facade := &fakeK12ImageTaskFacade{createErr: errors.New("records unavailable")}
	reply, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), k12PhotoTestMessage(),
		k12PhotoTestRouter(t, true, "k12-tutor"), facade,
	)
	if !handled || err == nil || reply != nil {
		t.Fatalf("dispatch 创建失败必须诚实失败且不得旁路重复调模型: handled=%v reply=%#v err=%v",
			handled, reply, err)
	}
	if facade.startCalls != 0 || facade.getCalls != 0 {
		t.Fatalf("未固化 dispatch 不得启动后续阶段: start=%d get=%d",
			facade.startCalls, facade.getCalls)
	}
}

func TestMaybeHandleK12DingtalkPhoto_NewDispatchMustBeScheduled(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	facade := &fakeK12ImageTaskFacade{rejectStart: true}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	reply, handled, err := maybeHandleK12DingtalkPhoto(
		ctx, k12PhotoTestMessage(),
		k12PhotoTestRouter(t, true, "k12-tutor"), facade,
	)
	if !handled || reply != nil || err == nil ||
		!strings.Contains(err.Error(), "未能启动") {
		t.Fatalf("新 dispatch 排队失败必须立即报错: handled=%v reply=%#v err=%v",
			handled, reply, err)
	}
}

func TestMaybeHandleK12DingtalkPhoto_UnmatchedMessageDoesNotTouchFacade(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	facade := &fakeK12ImageTaskFacade{}
	reply, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), k12PhotoTestMessage(),
		k12PhotoTestRouter(t, false, "k12-tutor"), facade,
	)
	if err != nil || handled || reply != nil || len(facade.events) != 0 {
		t.Fatalf("未显式绑定图片应交回通用引擎: handled=%v events=%v err=%v",
			handled, facade.events, err)
	}
}

func TestMaybeHandleK12DingtalkPhoto_CreativeUsesCanonicalFeedbackProjection(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	facade := &fakeK12ImageTaskFacade{}
	facade.result = k12usecase.ImageTaskResult{
		Kind: "creative",
		CreativeWork: &k12usecase.CreativeWorkView{
			Record: &records.AgentRecord{RecordID: "work-1"},
			Fields: k12.CreativeWorkFields{Versions: []k12.CreativeWorkVersion{{
				StructuredFeedback: &k12.WorkFeedback{
					ProjectionMarkdown: "### 观察\n\n画面中的人物和小猫位置清楚。",
				},
			}}},
		},
	}
	reply, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), k12PhotoTestMessage(),
		k12PhotoTestRouter(t, true, "k12-tutor"), facade,
	)
	if err != nil || !handled || reply == nil ||
		reply.Content != "### 观察\n\n画面中的人物和小猫位置清楚。" {
		t.Fatalf("作品点评必须复用结构化点评的 canonical projection: reply=%#v err=%v", reply, err)
	}
}
