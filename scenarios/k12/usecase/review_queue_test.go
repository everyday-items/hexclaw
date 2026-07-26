package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// seedMistake writes one durable record for review/report tests without
// invoking an external grading model.
func seedMistake(t *testing.T, d Deps, session, concept, cause string, due int64) string {
	t.Helper()
	record, err := k12.NewMistakeRecord("mingming", session, k12.MistakeFields{
		GradeTerm: "五年级上", Question: "题-" + session, KnowledgePoint: concept, ErrorCause: cause,
	})
	if err != nil {
		t.Fatal(err)
	}
	record.DueAt = &due
	if _, err := d.Records.Put(context.Background(), record); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return record.RecordID
}

func TestReviewQueueAndMastery(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{ev: SolveEvidence{EvidenceType: EvidenceNumericExec}}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedMistake(t, d, "a", "小数乘法", "计算失误", 500)
	seedMistake(t, d, "b", "分数加减", "概念不清", 900)
	seedMistake(t, d, "c", "简易方程", "方法错误", 5000)

	queue, err := d.ReviewQueue(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 || queue[0].Record.DueAt == nil || *queue[0].Record.DueAt != 500 {
		t.Fatalf("due queue ordering drifted: %+v", queue)
	}
	if err := d.MarkMastered(ctx, "mingming", id, 0); err != nil {
		t.Fatal(err)
	}
	queue, err = d.ReviewQueue(ctx, "mingming")
	if err != nil || len(queue) != 1 {
		t.Fatalf("mastered record remained due: len=%d err=%v", len(queue), err)
	}
}
