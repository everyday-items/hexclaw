package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestConfirmPracticeSetItems_AdvancesReviewWithoutSystemMasteryEvidence(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	mistakeID := seedMistake(t, d, "manual-result", "小数乘法", "计算失误", 500)
	item := k12.PracticeItem{
		ItemID: "item-a", SourceProblemID: mistakeID, Subject: "数学",
		AddedVia: k12.PracticeAddedViaSingleVariant, QuestionMarkdown: "3.8×3=?",
		ExpectedAnswerMarkdown: "11.4", VerificationStatus: k12.PracticeItemVerified,
		VerificationEvidence: "独立验算",
	}
	setID, _, err := d.AddToBasket(ctx, "mingming", "manual-no-photo", item)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.FinalizeBasket(ctx, "mingming", setID, "print", ""); err != nil {
		t.Fatal(err)
	}

	view, err := d.ConfirmPracticeSetItems(ctx, "mingming", setID, []PracticeGradeResult{{
		ItemID:  "item-a",
		Correct: true,
	}})
	if err != nil {
		t.Fatalf("手动记结果: %v", err)
	}
	if got := view.Fields.Items[0].ResultEvidence; got != k12.PracticeResultHumanConfirmed {
		t.Fatalf("手动结果 evidence=%q want %q", got, k12.PracticeResultHumanConfirmed)
	}
	if len(view.Fields.ReturnAssets) != 0 || view.Fields.Items[0].Returned {
		t.Fatalf("无照片人工降级不得伪造 return_assets: %+v", view.Fields)
	}
	rec, err := d.Records.Get(ctx, mistakeID)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParseMistakeFields(rec.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status == k12.StatusMastered {
		t.Fatalf("人工确认不得形成系统掌握: status=%s fields=%+v", rec.Status, fields)
	}
	if fields.ReviewStage != 1 || rec.DueAt == nil || *rec.DueAt != 1000+3*86400 {
		t.Fatalf("人工确认仍应推进正常复习间隔: status=%s due=%v fields=%+v",
			rec.Status, rec.DueAt, fields)
	}
}
