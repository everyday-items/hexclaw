package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type subjectCaptureSolver struct{ subject, constraint string }

func (s *subjectCaptureSolver) Solve(context.Context, string, string, string) (SolveResult, error) {
	return SolveResult{Solution: "ok"}, nil
}
func (s *subjectCaptureSolver) SolveSubject(_ context.Context, subject, _, _, constraint string) (SolveResult, error) {
	s.subject = subject
	s.constraint = constraint
	return SolveResult{Solution: "ok"}, nil
}

type subjectCaptureGrader struct{ subject string }

func (g *subjectCaptureGrader) Grade(context.Context, string, string, string) (GradeOutcome, error) {
	return GradeOutcome{Verdict: VerdictAgree}, nil
}
func (g *subjectCaptureGrader) GradeSubject(_ context.Context, subject, _, _, _ string) (GradeOutcome, error) {
	g.subject = subject
	return GradeOutcome{Verdict: VerdictAgree}, nil
}

func TestGradeHomeworkProblem_PassesSubjectToAdapters(t *testing.T) {
	s, g := &subjectCaptureSolver{}, &subjectCaptureGrader{}
	d, _ := newPipeline(t, s, g, nil)
	_, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "英语", Grade: "五年级上", Problem: "fill", StudentAnswer: "goes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.subject != "英语" || g.subject != "英语" {
		t.Fatalf("subject lost: solver=%q grader=%q", s.subject, g.subject)
	}
	if s.constraint != "" {
		t.Fatalf("English solve received math-only constraint: %q", s.constraint)
	}
}

type failingSolver struct{}

func (failingSolver) Solve(context.Context, string, string, string) (SolveResult, error) {
	return SolveResult{}, errors.New("provider down")
}

type failingGrader struct{}

func (failingGrader) Grade(context.Context, string, string, string) (GradeOutcome, error) {
	return GradeOutcome{}, errors.New("metadata unavailable")
}

func TestGradeHomeworkProblem_DownstreamFailuresAreTyped(t *testing.T) {
	t.Run("solver", func(t *testing.T) {
		d, _ := newPipeline(t, failingSolver{}, fakeGrader{}, nil)
		_, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{AgentName: "mingming", Grade: "五年级上", Problem: "1+1"})
		if !errors.Is(err, ErrSolveFailed) {
			t.Fatalf("solver err=%v want ErrSolveFailed", err)
		}
	})
	t.Run("grader", func(t *testing.T) {
		d, _ := newPipeline(t, fakeSolver{}, failingGrader{}, nil)
		// 已答题（StudentAnswer 非空）才走批改路径——单一真相源分叉后，空作答走解题不触 grader。
		_, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{AgentName: "mingming", Grade: "五年级上", Problem: "1+1", StudentAnswer: "3"})
		if !errors.Is(err, ErrSolveFailed) {
			t.Fatalf("grader err=%v want ErrSolveFailed", err)
		}
	})
}

func TestGradeHomeworkProblem_RejectsInvalidGrade(t *testing.T) {
	d, _ := newPipeline(t, panicSolver{}, panicGrader{}, nil)
	_, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{AgentName: "mingming", Grade: "大学", Problem: "1+1"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid grade err=%v want ErrInvalidInput", err)
	}
}

func TestAllSolveEntrypointsRejectInvalidGrade(t *testing.T) {
	d, _ := newPipeline(t, panicSolver{}, panicGrader{}, nil)
	ctx := context.Background()
	checks := []struct {
		name string
		fn   func() error
	}{
		{name: "tutor", fn: func() error {
			_, err := d.TutorTurn(ctx, TutorTurnRequest{AgentName: "mingming", PriorStage: StageHint2, ParentMessage: "直接讲"}, "1+1", "大学")
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.fn(); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err=%v want ErrInvalidInput", err)
			}
		})
	}
}

func TestGradeHomeworkProblem_RecognizedKnowledgePointWinsAndReturns(t *testing.T) {
	d, store := newPipeline(t, fakeSolver{}, fakeGrader{outcome: GradeOutcome{
		Verdict: VerdictDisagree, KnowledgePoint: "模型幻觉知识点", ErrorCause: "计算失误",
	}}, nil)
	res, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "kp", Problem: "3.8x3", StudentAnswer: "10.4", KnowledgePoints: []string{"小数乘法"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome.KnowledgePoint != "小数乘法" {
		t.Fatalf("result kp=%q want recognized kp", res.Outcome.KnowledgePoint)
	}
	rec, err := store.Get(context.Background(), res.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := k12.ParseMistakeFields(rec.Fields)
	if err != nil || f.KnowledgePoint != "小数乘法" {
		t.Fatalf("stored kp=%q err=%v", f.KnowledgePoint, err)
	}
}

func TestGradeHomeworkProblem_PersistsSubjectForCrossSubjectReview(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{outcome: GradeOutcome{Verdict: VerdictDisagree, ErrorCause: "单位错误"}}, nil)
	res, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "物理", Grade: "初二上", SourceSession: "physics",
		Problem: "速度是多少", StudentAnswer: "20m/s", KnowledgePoints: []string{"速度"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := d.Records.Get(context.Background(), res.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := k12.ParseMistakeFields(rec.Fields)
	if err != nil || f.Subject != "物理" {
		t.Fatalf("stored subject=%q err=%v", f.Subject, err)
	}
}
