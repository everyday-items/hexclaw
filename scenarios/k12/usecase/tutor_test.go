package usecase

import (
	"context"
	"strings"
	"testing"
)

func TestPlanTutorTurn_StartsAtStage1(t *testing.T) {
	d := PlanTutorTurn(TutorTurnRequest{})
	if d.Stage != StageHint1 || d.Comfort || d.Escalated {
		t.Fatalf("首轮应阶段一、无守门、无升级, got %+v", d)
	}
	if !strings.Contains(d.PromptHint, "方向") {
		t.Errorf("阶段一指令应含方向性提示: %q", d.PromptHint)
	}
}

func TestPlanTutorTurn_AdvanceOnNotUnderstand(t *testing.T) {
	d := PlanTutorTurn(TutorTurnRequest{PriorStage: StageHint1, ParentMessage: "他还是不会"})
	if d.Stage != StageHint2 || !d.Escalated {
		t.Fatalf("不会应升到阶段二: %+v", d)
	}
}

func TestPlanTutorTurn_StudentAnswerEntersGrading(t *testing.T) {
	d := PlanTutorTurn(TutorTurnRequest{PriorStage: StageHint1, StudentAnswer: "11.6"})
	if d.Stage != StageHint2 {
		t.Fatalf("报了作答应进阶段二批改: %+v", d)
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
	if !strings.Contains(d.PromptHint, "变式题") {
		t.Errorf("阶段三应留变式题: %q", d.PromptHint)
	}
}

func TestPlanTutorTurn_EmotionGateHoldsStage(t *testing.T) {
	// 情绪命中 → 守门，不推进阶段（即便同时说"不会"）。
	d := PlanTutorTurn(TutorTurnRequest{PriorStage: StageHint2, ParentMessage: "他不会，急哭了"})
	if !d.Comfort {
		t.Fatalf("情绪应触发守门: %+v", d)
	}
	if d.Stage != StageHint2 {
		t.Errorf("守门应保持原阶段（暂停推进）, got %d", d.Stage)
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

	// 阶段一：不调 Solver（不给未验证答案）。
	res1, err := d.TutorTurn(ctx, TutorTurnRequest{}, "3.8×3", "五年级上")
	if err != nil {
		t.Fatal(err)
	}
	if res1.Solution != "" {
		t.Errorf("阶段一不应给解, got %q", res1.Solution)
	}
}

func TestTutorTurn_EmotionGateSkipsSolver(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{solution: "解：11.4"}, fakeGrader{}, &fakeInsights{})
	// 即使阶段被推到三，情绪守门也不给答案（暂停解题）。
	res, err := d.TutorTurn(context.Background(),
		TutorTurnRequest{PriorStage: StageFull, ParentMessage: "他哭了"}, "3.8×3", "五年级上")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Directive.Comfort {
		t.Fatal("应守门")
	}
	if res.Solution != "" {
		t.Errorf("守门轮不应给解, got %q", res.Solution)
	}
}
