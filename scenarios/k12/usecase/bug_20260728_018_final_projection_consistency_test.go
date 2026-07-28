package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestBUG20260728018CompletedHomeworkProjectionMatchesFinalArtifact(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "6×7=", Subject: "数学", AnswerState: AnswerStateBlank,
			SourceNumberPath: []string{"1"}, DisplayLabel: "1.",
			KnowledgePoints: []string{"整数乘法"},
		},
		{
			Question: "8×7=", Subject: "数学", StudentAnswer: "54",
			AnswerState:      AnswerStatePresent,
			SourceNumberPath: []string{"2"}, DisplayLabel: "2.",
			KnowledgePoints: []string{"整数乘法"},
		},
	}, solver, grader)
	o.deps.TutoringTipsReview = &bug20260726031TipsSpy{}

	jobID := runItemResumeJobToAssessing(t, o, "bug-018-final-projection")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	items := make([]PhotoGradeItem, 0, len(run.questions))
	for _, question := range run.questions {
		item, err := o.assessDurablePhotoItem(
			context.Background(), o.deps, job, run.req, PhotoModeGrade, question,
		)
		if err != nil {
			t.Fatalf("commit assessment %s: %v", question.ProblemID, err)
		}
		items = append(items, item)
	}
	forceBUG20260726031Projecting(t, o, jobID, PhotoGradeResult{Items: items})
	view, err := o.runProject(context.Background(), run, jobID)
	if err != nil {
		t.Fatalf("finalize grading job: %v", err)
	}
	if view.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("grading stage=%s, want completed", view.Record.Status)
	}

	projection, err := o.ImageTaskHomeworkProjection(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatalf("read public homework projection: %v", err)
	}
	if projection.FinalArtifact == nil {
		t.Fatal("completed projection has no final artifact")
	}
	artifact := projection.FinalArtifact
	progressive := projection.Progressive
	if progressive.StructureVersion != artifact.StructureVersion {
		t.Fatalf(
			"progressive structure_version=%d, final artifact=%d",
			progressive.StructureVersion,
			artifact.StructureVersion,
		)
	}
	if len(progressive.ProblemProgress) != artifact.TotalCount {
		t.Fatalf(
			"progressive problems=%d, final artifact total=%d",
			len(progressive.ProblemProgress),
			artifact.TotalCount,
		)
	}
	if progressive.Coverage.Status != "complete" ||
		progressive.Coverage.Total != artifact.TotalCount ||
		progressive.Coverage.Published != artifact.PublishedCount ||
		progressive.Coverage.Skipped != artifact.SkippedCount ||
		progressive.Coverage.Awaiting != 0 ||
		progressive.Coverage.Failed != 0 {
		t.Fatalf(
			"progressive coverage=%+v, final artifact=%+v",
			progressive.Coverage,
			*artifact,
		)
	}
}
