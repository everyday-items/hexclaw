package usecase

import (
	"context"
	"strings"
	"testing"
)

func TestPlanTutorTurn_StartsWithFullParentReference(t *testing.T) {
	d := PlanTutorTurn(TutorTurnRequest{})
	if d.Stage != StageFull || d.Comfort || d.Escalated {
		t.Fatalf("首轮应完整家长参考、无安抚、无升级, got %+v", d)
	}
	if !strings.Contains(d.PromptHint, "正确答案") || !strings.Contains(d.PromptHint, "家长") {
		t.Errorf("指令应给家长答案与讲法: %q", d.PromptHint)
	}
}

func TestPlanTutorTurn_AdvanceOnNotUnderstand(t *testing.T) {
	d := PlanTutorTurn(TutorTurnRequest{PriorStage: StageHint1, ParentMessage: "他还是不会"})
	if d.Stage != StageFull || !d.Escalated {
		t.Fatalf("无需家长逐轮解锁完整讲法: %+v", d)
	}
}

func TestPlanTutorTurn_StudentAnswerEntersGrading(t *testing.T) {
	d := PlanTutorTurn(TutorTurnRequest{PriorStage: StageHint1, StudentAnswer: "11.6"})
	if d.Stage != StageFull {
		t.Fatalf("报了作答也应给完整家长参考: %+v", d)
	}
	if !strings.Contains(d.PromptHint, "第一个出错") {
		t.Errorf("阶段二含作答应走批改指令: %q", d.PromptHint)
	}
}

func TestPlanTutorTurn_FullRequestJumpsToStage3(t *testing.T) {
	d := PlanTutorTurn(TutorTurnRequest{PriorStage: StageHint1, ParentMessage: "别提示了，直接讲吧"})
	if d.Stage != StageFull || !d.Escalated {
		t.Fatalf("直接讲应跳阶段三: %+v", d)
	}
	if !strings.Contains(d.PromptHint, "完整") || !strings.Contains(d.PromptHint, "家长") {
		t.Errorf("完整讲解应面向家长: %q", d.PromptHint)
	}
}

func TestPlanTutorTurn_EmotionKeepsFullParentReference(t *testing.T) {
	// 安抚孩子与提供家长备课参考可以同时完成。
	d := PlanTutorTurn(TutorTurnRequest{PriorStage: StageHint2, ParentMessage: "他不会，急哭了"})
	if !d.Comfort {
		t.Fatalf("情绪应触发守门: %+v", d)
	}
	if d.Stage != StageFull {
		t.Errorf("安抚不能挡住家长完整参考, got %d", d.Stage)
	}
	if d.EmotionCue == "" || !strings.Contains(d.PromptHint, "安抚") {
		t.Errorf("守门指令应含安抚: cue=%q hint=%q", d.EmotionCue, d.PromptHint)
	}
}

func TestPlanTutorTurn_NoDoubleAdvancePastFull(t *testing.T) {
	d := PlanTutorTurn(TutorTurnRequest{PriorStage: StageFull, ParentMessage: "还是不懂"})
	if d.Stage != StageFull {
		t.Errorf("阶段三是上限, got %d", d.Stage)
	}
}

func TestTutorTurn_Stage3CallsSolver(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{solution: "解：11.4"}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()

	// 阶段三：应调 Solver 取验算解。
	res, err := d.TutorTurn(ctx, TutorTurnRequest{PriorStage: StageHint2, ParentMessage: "直接讲吧"}, "3.8×3", "五年级上")
	if err != nil {
		t.Fatal(err)
	}
	if res.Directive.Stage != StageFull {
		t.Fatalf("应阶段三, got %d", res.Directive.Stage)
	}
	if res.Solution != "解：11.4" {
		t.Errorf("阶段三应带验算解, got %q", res.Solution)
	}

	// 首轮自动完成解题，不让家长先答几轮才拿到答案。
	res1, err := d.TutorTurn(ctx, TutorTurnRequest{}, "3.8×3", "五年级上")
	if err != nil {
		t.Fatal(err)
	}
	if res1.Solution != "解：11.4" {
		t.Errorf("首轮应给完整解, got %q", res1.Solution)
	}
}

func TestTutorTurn_EmotionDoesNotSkipSolver(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{solution: "解：11.4"}, fakeGrader{}, &fakeInsights{})
	// 孩子暂停练习不应剥夺家长的已验证参考答案。
	res, err := d.TutorTurn(context.Background(),
		TutorTurnRequest{PriorStage: StageFull, ParentMessage: "他哭了"}, "3.8×3", "五年级上")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Directive.Comfort {
		t.Fatal("应守门")
	}
	if res.Solution != "解：11.4" {
		t.Errorf("安抚轮也应给家长完整解, got %q", res.Solution)
	}
}
