package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 闭环纠偏契约（2026-07-18）：
//   - 冷启动拆只读推断（§3.1 主流程 4：推断只返回建议，不写档案；家长确认后才创建）；
//   - 学情复习完成率口径纠偏（§5.7：已复批 PracticeSetItem 数 ÷ 已固化卷 verified 题目总数）。

func TestInferProfile_ReadOnlyNeverWrites(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	d.Profiles = newMemProfiles()
	ctx := context.Background()

	res, err := d.InferProfile(ctx, "mingming", "小明", []string{"分数除法"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Inferred || res.Grade != "六年级上" || res.Created {
		t.Fatalf("应只返回建议（推断六年级上、不建档），got %+v", res)
	}
	// RED 钉死：只读推断绝不写档案。
	if p, _ := d.Profiles.GetProfile(ctx, "mingming"); p.GradeTerm != "" {
		t.Fatalf("InferProfile 不得写档案，档案却有年级 %q", p.GradeTerm)
	}
	// 教材未提供 → 建议中留空待补充，不默认人教版（§3.1 主流程 5）。
	if res.Profile.TextbookEdition != "" {
		t.Errorf("教材应留空待补充，got %q", res.Profile.TextbookEdition)
	}
}

func TestInsightReport_CompletionRatePaperProjection(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{}) // now()=1000
	ctx := context.Background()

	// 有错题但无已固化卷 → 分母 0 → -1 哨兵（前端显示「—」）。
	d.Now = nil // 与 records 落库时钟同源，避免月界脆弱
	seedMistake(t, d, "s1", "小数乘法", "计算失误", 500)
	rep, err := d.InsightReport(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReviewCompletionRate != -1 {
		t.Fatalf("无已固化卷分母为 0 应 -1（§5.7），got %v", rep.ReviewCompletionRate)
	}
	d.Now = func() int64 { return 1000 }

	// 固化一张两题卷（含 1 道阻断题，不入分母）→ 回传 → 逐题复批一题。
	items := []k12.PracticeItem{
		{ItemID: "qa", Subject: "数学", QuestionMarkdown: "3.8×3=?", ExpectedAnswerMarkdown: "11.4",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
		{ItemID: "qb", Subject: "数学", QuestionMarkdown: "2.8×0.65=?", ExpectedAnswerMarkdown: "1.82",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
		{ItemID: "qc", Subject: "科学", QuestionMarkdown: "闭合电路判断", VerificationStatus: k12.PracticeItemPending},
	}
	setID := ""
	for _, it := range items {
		id, _, err := d.AddToBasket(ctx, "mingming", "s1", it)
		if err != nil {
			t.Fatal(err)
		}
		setID = id
	}
	if _, _, err := d.FinalizeBasket(ctx, "mingming", setID, "print", ""); err != nil {
		t.Fatal(err)
	}
	rep, _ = d.InsightReport(ctx, "mingming")
	if rep.ReviewCompletionRate != 0 {
		t.Errorf("已固化 2 道 verified、0 题复批 → 0，got %v", rep.ReviewCompletionRate)
	}

	if err := d.SubmitPracticeSet(ctx, "mingming", setID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GradePracticeSetItems(ctx, "mingming", setID, []PracticeGradeResult{{ItemID: "qa", Correct: true}}); err != nil {
		t.Fatal(err)
	}
	rep, _ = d.InsightReport(ctx, "mingming")
	if rep.ReviewCompletionRate != 0.5 {
		t.Errorf("2 道入卷、1 题已复批 → 0.5，got %v", rep.ReviewCompletionRate)
	}

	if _, err := d.GradePracticeSetItems(ctx, "mingming", setID, []PracticeGradeResult{{ItemID: "qb", Correct: false}}); err != nil {
		t.Fatal(err)
	}
	rep, _ = d.InsightReport(ctx, "mingming")
	if rep.ReviewCompletionRate != 1 {
		t.Errorf("全部入卷题已复批 → 1，got %v", rep.ReviewCompletionRate)
	}
}
