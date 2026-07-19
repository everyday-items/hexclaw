package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 复批逐题结论联动错题（架构设计 §3.8 历史与回传复批 第 3、4 条）RED 契约：
//   - 复批按题给结论：通过题若有 source_problem_id → 走 MarkRetried 链路积掌握证据；
//     未通过题 → 对应 Mistake 复习轮次重置首档，due_at 使用 v1 首档 1 天（§4.6）。
//   - 部分回传：允许多次调用，每次覆盖已给结论的题，幂等；
//     全部入卷题都有结论后卷才转 graded（§3.8 第 4 条）。

// seedRegradePaper 造一张两题卷：item-a 关联错题 mid，item-b 无来源题；固化并整卷回传（submitted）。
func seedRegradePaper(t *testing.T, d Deps, mid string) string {
	t.Helper()
	ctx := context.Background()
	itemA := k12.PracticeItem{
		ItemID: "item-a", SourceProblemID: mid, Subject: "数学",
		AddedVia: k12.PracticeAddedViaSingleVariant, QuestionMarkdown: "3.8×3=?",
		ExpectedAnswerMarkdown: "11.4", VerificationStatus: k12.PracticeItemVerified,
		VerificationEvidence: "独立验算",
	}
	itemB := k12.PracticeItem{
		ItemID: "item-b", Subject: "数学", AddedVia: k12.PracticeAddedViaWeekly,
		QuestionMarkdown: "2.8×0.65=?", ExpectedAnswerMarkdown: "1.82",
		VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
	}
	setID, _, err := d.AddToBasket(ctx, "mingming", "s1", itemA)
	if err != nil {
		t.Fatalf("装篮 A: %v", err)
	}
	if _, _, err := d.AddToBasket(ctx, "mingming", "s1", itemB); err != nil {
		t.Fatalf("装篮 B: %v", err)
	}
	if _, _, err := d.FinalizeBasket(ctx, "mingming", setID, "print", ""); err != nil {
		t.Fatalf("固化: %v", err)
	}
	submitWholeSetInternal(t, d, "mingming", setID)
	return setID
}

func TestGradeResults_FeedbackToMistakes(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{}) // now()=1000
	ctx := context.Background()
	mid := seedMistake(t, d, "s1", "小数乘法", "计算失误", 500)
	setID := seedRegradePaper(t, d, mid)

	// ① 部分结论：item-a 未通过 → 错题轮次回首档，due 使用 v1 的 1 天；卷保持 submitted。
	v, err := d.GradePracticeSetItems(ctx, "mingming", setID, []PracticeGradeResult{{ItemID: "item-a", Correct: false}})
	if err != nil {
		t.Fatalf("部分结论复批: %v", err)
	}
	if v.Record.Status != k12.PracticeStatusSubmitted {
		t.Fatalf("仍有题无结论，卷不得转 graded（§3.8 第 4 条），got %s", v.Record.Status)
	}
	rec, err := d.Records.Get(ctx, mid)
	if err != nil {
		t.Fatal(err)
	}
	if rec.DueAt == nil || *rec.DueAt != 1000+FirstReviewInterval {
		t.Errorf("未通过题应把错题 due 重置到 v1 首档 1 天，got %v", rec.DueAt)
	}
	if f, _ := k12.ParseMistakeFields(rec.Fields); f.ReviewStage != 0 {
		t.Errorf("未通过应重置复习轮次回首档（§4.6），got %d", f.ReviewStage)
	}

	// ② 补传结论覆盖：A 改判通过 + B 通过 → 全部有结论，卷转 graded；通过题走 MarkRetried 积证据。
	v, err = d.GradePracticeSetItems(ctx, "mingming", setID, []PracticeGradeResult{
		{ItemID: "item-a", Correct: true}, {ItemID: "item-b", Correct: true},
	})
	if err != nil {
		t.Fatalf("全结论复批: %v", err)
	}
	if v.Record.Status != k12.PracticeStatusGraded {
		t.Fatalf("全部入卷题有结论后卷应转 graded，got %s", v.Record.Status)
	}
	// #4c：复批结论挂在练习项（归属固化时铸造的 practice_problem_id），不挂来源错题 Problem；
	// source_problem_id 仅作 derived_from 回写链。
	for _, it := range v.Fields.Items {
		if it.ResultCorrect == nil {
			t.Errorf("入卷题 %s 应有逐题结论", it.ItemID)
		}
		if it.PaperSeq > 0 && it.PracticeProblemID == "" {
			t.Errorf("入卷题 %s 应有铸造的 practice_problem_id 供复批结论归属", it.ItemID)
		}
		if it.PracticeProblemID != "" && it.PracticeProblemID == it.SourceProblemID {
			t.Errorf("复批归属不得复用来源错题 Problem：%s", it.ItemID)
		}
	}
	rec, _ = d.Records.Get(ctx, mid)
	if rec.Status != k12.StatusRetried {
		t.Errorf("通过题应走 MarkRetried 链路（→retried），got %s", rec.Status)
	}
	f, _ := k12.ParseMistakeFields(rec.Fields)
	if f.ReviewStage != 1 {
		t.Errorf("通过后应进下一档（ReviewStage=1），got %d", f.ReviewStage)
	}
	wantDue := int64(1000 + 3*86400) // v1 阶梯 rung1 = 3 天
	if rec.DueAt == nil || *rec.DueAt != wantDue {
		t.Errorf("通过后到期应按阶梯推进到 %d，got %v", wantDue, rec.DueAt)
	}

	// ③ 幂等：重复同一结论不再推进阶梯、不重复积证据。
	if _, err := d.GradePracticeSetItems(ctx, "mingming", setID, []PracticeGradeResult{{ItemID: "item-a", Correct: true}}); err != nil {
		t.Fatalf("重复结论应幂等成功: %v", err)
	}
	rec2, _ := d.Records.Get(ctx, mid)
	f2, _ := k12.ParseMistakeFields(rec2.Fields)
	if f2.ReviewStage != 1 || rec2.Status != k12.StatusRetried {
		t.Errorf("重复结论不得二次推进阶梯: stage=%d status=%s", f2.ReviewStage, rec2.Status)
	}
}

func TestGradeResults_RejectUnknownAndBlockedItems(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	setID := seedRegradePaper(t, d, seedMistake(t, d, "s2", "小数乘法", "计算失误", 500))

	if _, err := d.GradePracticeSetItems(ctx, "mingming", setID, []PracticeGradeResult{{ItemID: "no-such", Correct: true}}); err == nil {
		t.Error("卷外题给结论应被拒")
	}
	// 卷仍是 submitted，未被部分失败污染成 graded。
	v, _ := d.GetPracticeSet(ctx, "mingming", setID)
	if v.Record.Status != k12.PracticeStatusSubmitted {
		t.Errorf("失败调用不得改卷状态，got %s", v.Record.Status)
	}
}

// TestGradeResults_LegacyEmptyResultsAllPass 空结论数组 = 旧行为整卷通过（后向兼容，
// 仅翻状态、不联动错题——新契约由 results 数组承载）。
func TestGradeResults_LegacyEmptyResultsAllPass(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	mid := seedMistake(t, d, "s3", "小数乘法", "计算失误", 500)
	setID := seedRegradePaper(t, d, mid)

	if err := d.GradePracticeSet(ctx, "mingming", setID); err != nil {
		t.Fatalf("旧行为复批: %v", err)
	}
	v, _ := d.GetPracticeSet(ctx, "mingming", setID)
	if v.Record.Status != k12.PracticeStatusGraded {
		t.Fatalf("旧行为应整卷 graded，got %s", v.Record.Status)
	}
	// 旧行为不产生错题联动副作用。
	rec, _ := d.Records.Get(ctx, mid)
	if rec.Status == k12.StatusRetried {
		t.Error("旧行为（空结论）不应联动错题状态")
	}
}
