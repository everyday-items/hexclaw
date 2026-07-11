package skilladapter_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/skilladapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

// seedDueMistake 往 mingming 错题本塞一条已到期（due 过去）的错题。
func seedDueMistake(t *testing.T, deps usecase.Deps, question, kp, cause string, due int64) {
	t.Helper()
	rec, err := k12.NewMistakeRecord("mingming", "sess-1", k12.MistakeFields{
		Question: question, KnowledgePoint: kp, ErrorCause: cause,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec.DueAt = &due
	if _, err := deps.Records.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

func seedDueAccum(t *testing.T, deps usecase.Deps, content string, due int64) {
	t.Helper()
	rec, err := k12.NewAccumRecord("mingming", "accum", k12.AccumFields{Subject: "英语", EntryType: "错词", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	rec.DueAt = &due
	if _, err := deps.Records.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

func TestReviewSkill_ReturnsDuePlan(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{})            // now()=1000
	seedDueMistake(t, deps, "3.8×3", "小数乘法", "计算失误", 500) // due<now → 到期
	seedDueMistake(t, deps, "1/2+1/3", "分数加减", "通分漏了", 900)
	sk := skilladapter.NewReviewSkill(deps)

	ctx := skill.WithRoutedAgent(context.Background(), "mingming")
	res, err := sk.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "3.8×3") || !strings.Contains(res.Content, "陪练建议") {
		t.Errorf("陪练方案应含到期题 + 引导话术: %q", res.Content)
	}
	// 守答案遮罩：话术里明确"别直接报答案"。
	if !strings.Contains(res.Content, "别直接报答案") {
		t.Errorf("应含答案遮罩话术: %q", res.Content)
	}
	if res.Metadata["k12_due_count"] != "2" {
		t.Errorf("到期数应为 2, meta=%v", res.Metadata)
	}
}

func TestReviewSkill_EmptyWhenNothingDue(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{})
	sk := skilladapter.NewReviewSkill(deps)
	ctx := skill.WithRoutedAgent(context.Background(), "mingming")
	res, err := sk.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "没有到期") {
		t.Errorf("无到期应诚实告知不制造焦虑: %q", res.Content)
	}
	if res.Metadata["k12_due_count"] != "0" {
		t.Errorf("到期数应为 0, meta=%v", res.Metadata)
	}
}

func TestReviewSkill_GenerateRetryVariant(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{}) // Solver=fakeSolver{sol:"解：11.4"}
	seedDueMistake(t, deps, "3.8×3", "小数乘法", "计算失误", 500)
	sk := skilladapter.NewReviewSkill(deps)

	ctx := skill.WithRoutedAgent(context.Background(), "mingming")
	res, err := sk.Execute(ctx, map[string]any{"generate_retry": true, "grade": "五年级上"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "变式题") || !strings.Contains(res.Content, "解：11.4") {
		t.Errorf("generate_retry 应附变式题（含 fakeSolver 解）: %q", res.Content)
	}
}

func TestReviewSkill_AccumulationUsesContentAndVerbatimRetry(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{})
	seedDueAccum(t, deps, "believe", 400)
	sk := skilladapter.NewReviewSkill(deps)
	ctx := skill.WithRoutedAgent(context.Background(), "mingming")
	res, err := sk.Execute(ctx, map[string]any{"generate_retry": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "believe") || !strings.Contains(res.Content, "再默一遍") {
		t.Fatalf("cross-subject review lost content/verbatim retry: %q", res.Content)
	}
	if res.Metadata["badge"] != "verbatim-recall" {
		t.Fatalf("badge=%q want verbatim-recall", res.Metadata["badge"])
	}
}

func TestReviewSkill_NoAgentErrors(t *testing.T) {
	sk := skilladapter.NewReviewSkill(newDeps(t, usecase.GradeOutcome{}))
	if _, err := sk.Execute(context.Background(), map[string]any{}); err == nil {
		t.Error("无法确定实例应报错（不猜孩子）")
	}
}

func TestReviewSkill_MatchFalseAndName(t *testing.T) {
	sk := skilladapter.NewReviewSkill(usecase.Deps{})
	if sk.Match("复习错题") {
		t.Error("k12_review 只经 LLM 工具调用，Match 应恒 false")
	}
	if sk.Name() != "k12_review" {
		t.Errorf("工具名应 k12_review, got %q", sk.Name())
	}
}
