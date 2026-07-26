package usecase_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestArtFeedbackRejectsDeferredCritiqueButAllowsFollowUpAfterCompleteCritique(t *testing.T) {
	deferred := "在继续点评前，请先让孩子说一说这次画的任务和故事；等孩子讲完后，我再给完整点评。"
	if got := usecase.WorkFeedbackRedlineViolation(deferred); got == "" {
		t.Fatalf("deferred non-critique must be rejected: %q", deferred)
	}

	completeThenFollowUp := "画面中央的人物面积最大，右下角的小猫与彩虹形成呼应；下一次可以试着加深地面颜色。完整点评后，可以问孩子最想保留哪一处。"
	if got := usecase.WorkFeedbackRedlineViolation(completeThenFollowUp); got != "" {
		t.Fatalf("a follow-up after complete critique must remain valid: %s", got)
	}
}

func TestArtFeedbackWithoutTaskOrIntentStillCompletesFromVisibleEvidence(t *testing.T) {
	d := newDataDeps(t)
	gen := &fakeWorkFeedbackSolver{
		feedback: "画面中央的人物面积最大，右下角的小猫与左上角彩虹形成呼应；下一次可以试着加深地面颜色。",
	}
	d.Solver = gen
	id, _, err := d.CreateCreativeWork(
		context.Background(),
		"xiaoming",
		"session-art-direct",
		k12.CreativeWorkFields{
			WorkType: k12.WorkTypeArt,
			Versions: []k12.CreativeWorkVersion{{SourceAssetID: "asset-1"}},
		},
	)
	if err != nil {
		t.Fatalf("create art work: %v", err)
	}

	view, err := d.GenerateWorkFeedback(context.Background(), "xiaoming", id)
	if err != nil {
		t.Fatalf("art feedback without task/intent must complete: %v", err)
	}
	if view.Record.Status != k12.WorkStatusFeedbackReady {
		t.Fatalf("status=%q, want feedback_ready", view.Record.Status)
	}
	feedback := view.Fields.Versions[0].StructuredFeedback
	if feedback == nil || len(feedback.Observations) == 0 ||
		len(feedback.Suggestions) == 0 ||
		!strings.Contains(feedback.ProjectionMarkdown, "## 可见证据") {
		t.Fatalf("complete canonical critique missing: %#v", feedback)
	}
	if gen.lastReq.Task != "" || gen.lastReq.Intent != "" {
		t.Fatalf("missing task/intent must remain missing facts: %+v", gen.lastReq)
	}
}
