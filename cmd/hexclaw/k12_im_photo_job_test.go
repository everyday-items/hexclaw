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
	events       []string
	createInput  k12usecase.CreateImageTaskInput
	createErr    error
	rejectStart  bool
	startCalls   int
	getCalls     int
	confirmCalls int
	result       k12usecase.ImageTaskResult
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

func (f *fakeK12ImageTaskFacade) Get(
	_ context.Context,
	agentName, dispatchID string,
) (k12usecase.ImageTaskView, error) {
	f.events = append(f.events, "get")
	f.getCalls++
	if agentName != "child-tutor" || dispatchID != "dispatch-1" {
		return k12usecase.ImageTaskView{}, errors.New("unexpected scope")
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

	reply, handled, err := maybeHandleK12DingtalkPhoto(
		context.Background(), k12PhotoTestMessage(), router, facade,
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
