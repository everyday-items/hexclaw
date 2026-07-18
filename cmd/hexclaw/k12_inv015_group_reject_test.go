package main

// K12-INV-015 反例（架构设计-v0.5.0 §7）：当前渠道只允许 direct，
// 群 conversation 永不进入业务流。
//
// 入站分流点：钉钉群消息（Metadata["conversation_type"]="2"，钉钉侧群聊约定值）
// 即使显式绑定了 K12 辅导 Agent + 单图附件，也不得进入拍照批改链路——
// handled=false 交还通用路径，编排器/批改闭包零调用。

import (
	"context"
	"testing"

	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestINV015_GroupConversationNeverEntersK12PhotoFlow(t *testing.T) {
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	processCalls := 0
	process := func(_ context.Context, _ k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		processCalls++
		return k12usecase.PhotoGradeResult{}, nil
	}
	orchestrator := &countingPhotoOrchestrator{}

	msg := k12PhotoTestMessage()
	msg.Metadata = map[string]string{
		"conversation_id":   "cidGROUP123",
		"conversation_type": "2", // 钉钉群聊
	}

	reply, handled, err := maybeHandleK12DingtalkPhotoJob(context.Background(), msg, router, orchestrator, process)
	if err != nil {
		t.Fatalf("群消息应被静默忽略而非报错: %v", err)
	}
	if handled || reply != nil {
		t.Fatalf("K12-INV-015 违规：群 conversation 进入了 K12 拍照批改链路（handled=%v reply=%#v）", handled, reply)
	}
	if processCalls != 0 || orchestrator.startCalls != 0 {
		t.Fatalf("K12-INV-015 违规：群消息触发了批改（process=%d orchestratorStart=%d）", processCalls, orchestrator.startCalls)
	}
}

// TestINV015_DirectConversationStillHandled 对照组：direct（conversation_type=1）
// 与缺省 metadata（历史消息/测试替身）保持既有行为，路由门禁不误伤私聊。
func TestINV015_DirectConversationStillHandled(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"显式 direct":   {"conversation_type": "1", "conversation_id": "cidDIRECT"},
		"缺省 metadata": nil,
	} {
		t.Run(name, func(t *testing.T) {
			router := k12PhotoTestRouter(t, true, "k12-tutor")
			process := func(_ context.Context, _ k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
				return k12usecase.PhotoGradeResult{Mode: k12usecase.PhotoModeGrade, Markdown: "## 作业批改"}, nil
			}
			msg := k12PhotoTestMessage()
			msg.Metadata = meta
			reply, handled, err := maybeHandleK12DingtalkPhoto(context.Background(), msg, router, process)
			if err != nil || !handled || reply == nil {
				t.Fatalf("direct 私聊必须照常进入 K12 链路: handled=%v reply=%#v err=%v", handled, reply, err)
			}
		})
	}
}

// countingPhotoOrchestrator 只数调用，任何进入即违规。
type countingPhotoOrchestrator struct{ startCalls int }

func (c *countingPhotoOrchestrator) StartPhotoGradingJob(context.Context, k12usecase.StartPhotoGradingInput) (k12usecase.GradingJobView, bool, error) {
	c.startCalls++
	return k12usecase.GradingJobView{}, false, nil
}
func (c *countingPhotoOrchestrator) RunGradingJob(context.Context, string) (k12usecase.GradingJobView, error) {
	return k12usecase.GradingJobView{}, nil
}
func (c *countingPhotoOrchestrator) ConfirmAndRun(context.Context, string, []string) (k12usecase.GradingJobView, error) {
	return k12usecase.GradingJobView{}, nil
}
func (c *countingPhotoOrchestrator) PhotoResult(string) (k12usecase.PhotoGradeResult, bool) {
	return k12usecase.PhotoGradeResult{}, false
}
func (c *countingPhotoOrchestrator) ReleaseGradingRun(string) {}
