package usecase

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func seedBUG20260726031SkipReceipt(
	t *testing.T,
	o *GradingOrchestrator,
	jobID string,
	q RecognizedQuestion,
	suffix string,
) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := o.deps.Records.DB().Exec(`
		INSERT INTO k12_problem_skip_receipts (
			skip_receipt_id,agent_name,job_id,problem_id,structure_version,input_revision,
			result_digest,current_disposition,published_revision,superseded_at,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		"skip-"+suffix, "mingming", jobID, q.ProblemID, 1, q.ConfirmedVersion,
		"sha256:skip-"+suffix, k12.GradingAssessmentDispositionCurrent, 1, 0, now, now,
	); err != nil {
		t.Fatalf("seed current skip receipt: %v", err)
	}
}

func TestBUG_20260726_031_AwaitingSourceDoesNotBlockClearSiblingOrCreateAssessment(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "q-awaiting-source", Subject: "数学",
			StudentAnswer: "", AnswerState: AnswerStateUnclear,
		},
		{
			Question: "q-clear", Subject: "数学",
			StudentAnswer: "2", AnswerState: AnswerStatePresent,
		},
	}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "bug-031-awaiting-source-isolation")

	view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil {
		t.Fatalf("clear sibling must continue while one source awaits resolution: %v", err)
	}
	if solver.callCount("q-awaiting-source") != 0 || grader.callCount("q-awaiting-source") != 0 {
		t.Errorf("awaiting source reached solve/grade: solver=%d grader=%d",
			solver.callCount("q-awaiting-source"), grader.callCount("q-awaiting-source"))
	}
	if solver.callCount("q-clear") != 1 || grader.callCount("q-clear") != 1 {
		t.Errorf("clear sibling did not continue independently: solver=%d grader=%d",
			solver.callCount("q-clear"), grader.callCount("q-clear"))
	}
	run := o.lookup(jobID)
	if run == nil || len(run.questions) != 2 {
		t.Fatal("missing frozen two-problem runtime")
	}
	var awaitingAssessments, clearAssessments int
	if err := o.deps.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_grading_assessment_items
		WHERE job_id=? AND problem_id=?`,
		jobID, run.questions[0].ProblemID,
	).Scan(&awaitingAssessments); err != nil {
		t.Fatal(err)
	}
	if err := o.deps.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_grading_assessment_items
		WHERE job_id=? AND problem_id=?`,
		jobID, run.questions[1].ProblemID,
	).Scan(&clearAssessments); err != nil {
		t.Fatal(err)
	}
	if awaitingAssessments != 0 {
		t.Errorf("awaiting source created %d Assessment rows; it is not a grading verdict", awaitingAssessments)
	}
	if clearAssessments != 1 {
		t.Errorf("clear sibling Assessment rows=%d, want one durable result", clearAssessments)
	}
	if view.Record.Status == k12.GradingStageCompleted {
		t.Errorf("page completed while one problem still has no result-or-skip disposition")
	}
}

func TestBUG_20260726_031_CurrentSkipHasZeroAssessmentMistakeReviewAndLearningEffects(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{
		calls: map[string]int{},
		outcomes: map[string]GradeOutcome{
			"q-skip": {
				Verdict: VerdictDisagree, ErrorCause: "不应生成的错因",
				WrongStep: "不应生成的错步",
			},
		},
	}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q-skip", Subject: "数学",
		StudentAnswer: "3", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "bug-031-skip-independent-disposition")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	q := run.questions[0]
	seedBUG20260726031SkipReceipt(t, o, jobID, q, "independent")

	if _, err := o.assessDurablePhotoItem(
		context.Background(), o.deps, job, run.req, PhotoModeGrade, q,
	); err != nil {
		t.Fatalf("replaying a current skip disposition must be locally recoverable: %v", err)
	}
	if solver.callCount("q-skip") != 0 || grader.callCount("q-skip") != 0 {
		t.Errorf("current skip reached model: solver=%d grader=%d",
			solver.callCount("q-skip"), grader.callCount("q-skip"))
	}
	var assessments, mistakes, reviews, learningEvents int
	if err := o.deps.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_grading_assessment_items WHERE job_id=? AND problem_id=?`,
		jobID, q.ProblemID,
	).Scan(&assessments); err != nil {
		t.Fatal(err)
	}
	if err := o.deps.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_mistakes WHERE agent_name='mingming'`,
	).Scan(&mistakes); err != nil {
		t.Fatal(err)
	}
	if err := o.deps.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM k12_mistakes
		WHERE agent_name='mingming' AND due_at IS NOT NULL`,
	).Scan(&reviews); err != nil {
		t.Fatal(err)
	}
	if err := o.deps.Records.DB().QueryRow(`
		SELECT COUNT(*) FROM outbox_events
		WHERE agent_name='mingming' AND event_type='k12.mistake.recorded'`,
	).Scan(&learningEvents); err != nil {
		t.Fatal(err)
	}
	if assessments != 0 || mistakes != 0 || reviews != 0 || learningEvents != 0 {
		t.Errorf("skip polluted grading/learning facts: assessments=%d mistakes=%d reviews=%d learning_events=%d",
			assessments, mistakes, reviews, learningEvents)
	}
}

func TestBUG_20260726_031_ResumeSkippedProblemCreatesNewRevisionAndOnlyRunsDependencyGroup(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "q-resume", Subject: "数学",
			StudentAnswer: "1", AnswerState: AnswerStatePresent,
		},
		{
			Question: "q-unaffected", Subject: "数学",
			StudentAnswer: "2", AnswerState: AnswerStatePresent,
		},
	}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "bug-031-resume-minimal-dependency")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	q1, q2 := run.questions[0], run.questions[1]
	if _, err := o.assessDurablePhotoItem(
		context.Background(), o.deps, job, run.req, PhotoModeGrade, q2,
	); err != nil {
		t.Fatalf("seed unaffected sibling receipt: %v", err)
	}
	seedBUG20260726031SkipReceipt(t, o, jobID, q1, "resume-v1")

	const resumedDigest = "sha256:resume-v2"
	if _, err := o.deps.Records.DB().Exec(`
		UPDATE k12_attempts
		SET confirmed_version=2,input_digest=?,updated_at=updated_at+1
		WHERE agent_name='mingming' AND attempt_id=? AND problem_id=?`,
		resumedDigest, q1.AttemptID, q1.ProblemID,
	); err != nil {
		t.Fatalf("freeze resumed source revision: %v", err)
	}
	q1.ConfirmedVersion = 2
	q1.InputDigest = resumedDigest
	if _, err := o.assessDurablePhotoItem(
		context.Background(), o.deps, job, run.req, PhotoModeGrade, q1,
	); err != nil {
		t.Fatalf("resume skipped problem revision: %v", err)
	}
	if solver.callCount("q-resume") != 1 || grader.callCount("q-resume") != 1 {
		t.Errorf("resumed problem calls solver=%d grader=%d, want one new revision only",
			solver.callCount("q-resume"), grader.callCount("q-resume"))
	}
	if solver.callCount("q-unaffected") != 1 || grader.callCount("q-unaffected") != 1 {
		t.Errorf("unaffected sibling was recomputed: solver=%d grader=%d",
			solver.callCount("q-unaffected"), grader.callCount("q-unaffected"))
	}
	var skipDisposition string
	if err := o.deps.Records.DB().QueryRow(`
		SELECT current_disposition FROM k12_problem_skip_receipts
		WHERE job_id=? AND problem_id=? AND input_revision=1`,
		jobID, q1.ProblemID,
	).Scan(&skipDisposition); err != nil {
		t.Fatal(err)
	}
	if skipDisposition != k12.GradingAssessmentDispositionSuperseded {
		t.Errorf("resumed revision left old skip disposition=%q, want superseded", skipDisposition)
	}
	var currentRevision int
	if err := o.deps.Records.DB().QueryRow(`
		SELECT input_revision FROM k12_grading_assessment_items
		WHERE job_id=? AND problem_id=? AND current_disposition='current'`,
		jobID, q1.ProblemID,
	).Scan(&currentRevision); err != nil {
		t.Fatal(err)
	}
	if currentRevision != 2 {
		t.Errorf("resumed current assessment revision=%d, want 2", currentRevision)
	}
}

type bug20260726031BlockingSolver struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func (s *bug20260726031BlockingSolver) Solve(
	ctx context.Context,
	_, _, _ string,
) (SolveResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return SolveResult{
			Solution: "2",
			Evidence: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec},
		}, nil
	case <-ctx.Done():
		return SolveResult{}, ctx.Err()
	}
}

func (s *bug20260726031BlockingSolver) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestBUG_20260726_031_SkipVsLateCallbackHasOneCurrentWinnerAndOneRealCostReceipt(t *testing.T) {
	solver := &bug20260726031BlockingSolver{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q-race", Subject: "数学",
		StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}, nil, grader)
	o.deps.Solver = solver
	jobID := runItemResumeJobToAssessing(t, o, "bug-031-skip-callback-race")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	q := run.questions[0]

	result := make(chan error, 1)
	go func() {
		_, err := o.assessDurablePhotoItem(
			context.Background(), o.deps, job, run.req, PhotoModeGrade, q,
		)
		result <- err
	}()
	select {
	case <-solver.started:
	case <-time.After(time.Second):
		t.Fatal("solve provider call did not reach sent boundary")
	}
	seedBUG20260726031SkipReceipt(t, o, jobID, q, "callback-race")
	close(solver.release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("late callback reconciliation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("late callback did not finish")
	}

	var currentDispositions int
	if err := o.deps.Records.DB().QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM k12_grading_assessment_items
			 WHERE job_id=? AND problem_id=? AND current_disposition='current')
			+
			(SELECT COUNT(*) FROM k12_problem_skip_receipts
			 WHERE job_id=? AND problem_id=? AND current_disposition='current')`,
		jobID, q.ProblemID, jobID, q.ProblemID,
	).Scan(&currentDispositions); err != nil {
		t.Fatal(err)
	}
	if currentDispositions != 1 {
		t.Errorf("callback/skip race produced %d current dispositions, want one CAS winner",
			currentDispositions)
	}
	if solver.callCount() != 1 {
		t.Errorf("real sent solve calls=%d, want exactly one", solver.callCount())
	}
	if grader.callCount("q-race") != 0 {
		t.Errorf("skip won while solve was sent, but late callback started %d grade calls",
			grader.callCount("q-race"))
	}
	var solveCosts, distinctSolveCosts int
	if err := o.deps.Records.DB().QueryRow(`
		SELECT COUNT(*),COUNT(DISTINCT cost_receipt_id)
		FROM k12_grading_item_invocations
		WHERE job_id=? AND problem_id=? AND operation='solve' AND cost_receipt_id<>''`,
		jobID, q.ProblemID,
	).Scan(&solveCosts, &distinctSolveCosts); err != nil {
		t.Fatal(err)
	}
	if solveCosts != 1 || distinctSolveCosts != 1 {
		t.Errorf("real sent solve cost evidence rows=%d distinct=%d, want exactly one",
			solveCosts, distinctSolveCosts)
	}
}
