package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// T2.4（hex-test 审计 · PRD §5.3.1）：讲解完成（渐进提示走到阶段三 StageFull）自动推进 explained。
// 原 explained 是死态（无写入路径）。tutor-turn 阶段三对某题完整讲解后，若该题在错题本(new) → explained。
func TestT2_4_FullExplanationAdvancesToExplained(t *testing.T) {
	ctx := context.Background()
	d, g := newToggleDeps(t)
	g.correct = false
	// 先判错入库该题（new）。
	if _, err := d.GradeHomeworkProblem(ctx, usecase.GradeRequest{
		AgentName: "xiaoming", Grade: "五年级上", SourceSession: "s1",
		Problem: "3.8×3=?", StudentAnswer: "11.6", KnowledgePoints: []string{"小数乘法"},
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := d.Records.ListByScope(ctx, "xiaoming", k12.CollectionMistakes, "")
	if len(recs) != 1 || recs[0].Status != k12.StatusNew {
		t.Fatalf("应入库 new, got %v", statusOf(recs))
	}

	// tutor-turn 阶段三完整讲解同题（"直接讲" 命中 fullRequestCue）。
	res, err := d.TutorTurn(ctx, usecase.TutorTurnRequest{
		AgentName: "xiaoming", PriorStage: 2, ParentMessage: "直接讲",
	}, "3.8×3=?", "五年级上")
	if err != nil {
		t.Fatal(err)
	}
	if res.Directive.Stage != usecase.StageFull {
		t.Fatalf("应到阶段三, got %d", res.Directive.Stage)
	}

	recs, _ = d.Records.ListByScope(ctx, "xiaoming", k12.CollectionMistakes, "")
	if recs[0].Status != k12.StatusExplained {
		t.Errorf("完整讲解后该题应 explained, got %q", recs[0].Status)
	}
}
