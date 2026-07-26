package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// BUG-20260725-013: “家长确认已会”是主观确认，不是系统掌握证据。
// 它必须保留原学习状态，只记录确认时间、安排一次抽查，并把到期日顺延到下一档。
func TestMarkMasteredRecordsParentConfirmationWithoutPromotingEvidenceMastery(t *testing.T) {
	d := newDataDeps(t)
	id := putDueMistake(t, d, "xiaoming", k12.MistakeFields{
		Subject:         "数学",
		Question:        "3.8×3=?",
		KnowledgePoint:  "小数乘法",
		CanonicalAnswer: "11.4",
		ReviewStage:     0,
	})
	before, err := d.Records.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	if err := d.MarkMastered(context.Background(), "xiaoming", id, before.Version); err != nil {
		t.Fatal(err)
	}
	after, err := d.Records.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParseMistakeFields(after.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != k12.StatusNew {
		t.Fatalf("家长确认不得写 evidence mastery，status=%q want %q", after.Status, k12.StatusNew)
	}
	if fields.ParentConfirmedAt != 1000 {
		t.Fatalf("parent_confirmed_at=%d want 1000", fields.ParentConfirmedAt)
	}
	if fields.SpotCheckState != k12.SpotCheckScheduled {
		t.Fatalf("spot_check_state=%q want %q", fields.SpotCheckState, k12.SpotCheckScheduled)
	}
	wantDue := int64(1000 + 3*86400)
	if after.DueAt == nil || *after.DueAt != wantDue {
		t.Fatalf("due_at=%v want %d", after.DueAt, wantDue)
	}
}
