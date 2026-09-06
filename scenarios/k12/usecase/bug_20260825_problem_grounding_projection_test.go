package usecase

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"

	_ "modernc.org/sqlite"
)

type problemGroundingReceiptWire struct {
	ProblemID      string `json:"problem_id"`
	Operation      string `json:"operation"`
	IdentityDigest string `json:"identity_digest"`
	GroundingEvidenceReceipt
}

func completedProblemGroundingFixture(
	t *testing.T,
) (*GradingOrchestrator, string, *gradingItemPinnedGrounding, *gradingItemGroundedPhysicalSolver, *gradingItemGroundedPhysicalGrader) {
	t.Helper()
	grounding := &gradingItemPinnedGrounding{active: "revision-a"}
	solver := &gradingItemGroundedPhysicalSolver{grounding: grounding}
	grader := &gradingItemGroundedPhysicalGrader{}
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: []RecognizedQuestion{
		{
			Question: "57+38=", Subject: "数学", StudentAnswer: "95",
			AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数加法"},
		},
		{
			Question: "26×3=", Subject: "数学", StudentAnswer: "78",
			AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数乘一位数"},
		},
	}}, nil, WithGradingRunDir(t.TempDir()))
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	o.deps.Grounding = grounding
	o.deps.TextbookOwnerID = "desktop-user"
	profile := o.deps.Profiles.(*memProfiles).m["mingming"]
	profile.TextbookEdition = "人教版"
	o.deps.Profiles.(*memProfiles).m["mingming"] = profile
	seedGradingItemActiveTextbookBinding(t, o)

	jobID := runItemResumeJobToAssessing(t, o, "problem-grounding-public-projection")
	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil {
		t.Fatalf("complete grounded grading: %v", err)
	}
	if completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("grounded grading stage=%s want completed", completed.Record.Status)
	}
	return o, jobID, grounding, solver, grader
}

func decodeProblemGroundingReceipts(
	t *testing.T,
	value any,
) []problemGroundingReceiptWire {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Receipts []problemGroundingReceiptWire `json:"problem_grounding_receipts"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Receipts
}

func assertProblemGroundingReceiptExactSet(
	t *testing.T,
	questions []RecognizedQuestion,
	receipts []problemGroundingReceiptWire,
) {
	t.Helper()
	if len(receipts) != len(questions)*2 {
		t.Fatalf("problem grounding receipts=%d want %d: %+v", len(receipts), len(questions)*2, receipts)
	}
	for questionIndex, question := range questions {
		solve := receipts[questionIndex*2]
		grade := receipts[questionIndex*2+1]
		if solve.ProblemID != question.ProblemID || grade.ProblemID != question.ProblemID {
			t.Fatalf("problem receipt order drift for %q: solve=%+v grade=%+v", question.ProblemID, solve, grade)
		}
		if solve.Operation != "solve" || grade.Operation != "grade" {
			t.Fatalf("problem %q operations=%q/%q want solve/grade", question.ProblemID, solve.Operation, grade.Operation)
		}
		if strings.TrimSpace(solve.IdentityDigest) == "" || solve.IdentityDigest != grade.IdentityDigest {
			t.Fatalf("problem %q identity drift: solve=%q grade=%q", question.ProblemID, solve.IdentityDigest, grade.IdentityDigest)
		}
		if !reflect.DeepEqual(solve.GroundingEvidenceReceipt, grade.GroundingEvidenceReceipt) {
			t.Fatalf("problem %q receipt exact-set drift: solve=%+v grade=%+v", question.ProblemID, solve, grade)
		}
	}
}

// 公开逐题投影只能复读当前 assessment 引用的 solve/grade 冻结证据，
// 且两个 operation 必须共享同一身份。
func TestK12ProblemGroundingProjectionPublishesSolveGradeExactSet(t *testing.T) {
	o, jobID, _, _, _ := completedProblemGroundingFixture(t)
	projection, err := o.ImageTaskHomeworkProjection(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	receipts := decodeProblemGroundingReceipts(t, projection)
	assertProblemGroundingReceiptExactSet(t, projection.Questions, receipts)

	raw, err := json.Marshal(receipts)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		jobID,
		"item_invocation_id",
		"result_json",
		"教材中的两位数加法依据",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("public problem grounding projection leaked %q: %s", forbidden, raw)
		}
	}
}

// 同一道题命中多个教材 chunk 时，solve/grade 必须各自公开
// 完整 receipt exact-set，且两个 operation 共用同一证据身份。
func TestK12ProblemGroundingProjectionPublishesMultiChunkSolveGradeExactSet(t *testing.T) {
	grounding := &gradingItemPinnedGrounding{active: "revision-a", multiChunk: true}
	solver := &gradingItemGroundedPhysicalSolver{grounding: grounding}
	grader := &gradingItemGroundedPhysicalGrader{}
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "57+38=", Subject: "数学", StudentAnswer: "95",
		AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数加法"},
	}}}, nil, WithGradingRunDir(t.TempDir()))
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	o.deps.Grounding = grounding
	o.deps.TextbookOwnerID = "desktop-user"
	profile := o.deps.Profiles.(*memProfiles).m["mingming"]
	profile.TextbookEdition = "人教版"
	o.deps.Profiles.(*memProfiles).m["mingming"] = profile
	seedGradingItemActiveTextbookBinding(t, o)
	seedGradingItemSecondTextbookSegment(t, o)

	jobID := runItemResumeJobToAssessing(t, o, "problem-grounding-multi-chunk")
	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil {
		t.Fatalf("complete multi-chunk grounded grading: %v", err)
	}
	if completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("multi-chunk grading stage=%s want completed", completed.Record.Status)
	}
	projection, err := o.ImageTaskHomeworkProjection(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Questions) != 1 {
		t.Fatalf("public questions=%d want 1", len(projection.Questions))
	}
	receipts := decodeProblemGroundingReceipts(t, projection)
	if len(receipts) != 4 {
		t.Fatalf("multi-chunk public receipts=%d want 4: %+v", len(receipts), receipts)
	}
	problemID := projection.Questions[0].ProblemID
	byOperation := map[string][]GroundingEvidenceReceipt{}
	identityByOperation := map[string]string{}
	for _, receipt := range receipts {
		if receipt.ProblemID != problemID {
			t.Fatalf("multi-chunk receipt problem=%q want %q", receipt.ProblemID, problemID)
		}
		byOperation[receipt.Operation] = append(
			byOperation[receipt.Operation], receipt.GroundingEvidenceReceipt,
		)
		identityByOperation[receipt.Operation] = receipt.IdentityDigest
	}
	wantChunks := []string{"segment-1", "segment-2"}
	for _, operation := range []string{"solve", "grade"} {
		got := byOperation[operation]
		if len(got) != len(wantChunks) {
			t.Fatalf("%s receipt count=%d want %d: %+v", operation, len(got), len(wantChunks), got)
		}
		for index, chunkID := range wantChunks {
			if got[index].ChunkID != chunkID {
				t.Fatalf("%s receipt[%d].chunk=%q want %q", operation, index, got[index].ChunkID, chunkID)
			}
		}
	}
	if identityByOperation["solve"] == "" ||
		identityByOperation["solve"] != identityByOperation["grade"] ||
		!reflect.DeepEqual(byOperation["solve"], byOperation["grade"]) {
		t.Fatalf(
			"multi-chunk solve/grade exact-set drift: identity=%+v receipts=%+v",
			identityByOperation, byOperation,
		)
	}
}

// 公开 operation exact-set 跟随已持久的 assessment 状态：可判分题
// 为 solve+grade；本用例中由 Provider 返回的 out_of_scope 只有 solve；
// unanswered/unclear 不得调用 Provider。
func TestK12ProblemGroundingProjectionUsesAssessmentStatusOperationExactSet(t *testing.T) {
	const outOfScopeProblem = "999+1="
	grounding := &gradingItemPinnedGrounding{active: "revision-a"}
	solver := &gradingItemGroundedPhysicalSolver{
		grounding: grounding, outOfScopeProblem: outOfScopeProblem, solution: "78",
	}
	grader := &gradingItemGroundedPhysicalGrader{}
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: []RecognizedQuestion{
		{
			Question: "57+38=", Subject: "数学", StudentAnswer: "95",
			AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数加法"},
		},
		{
			Question: outOfScopeProblem, Subject: "数学", StudentAnswer: "1000",
			AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数加法"},
		},
		{
			Question: "26×3=", Subject: "数学",
			AnswerState: AnswerStateBlank, KnowledgePoints: []string{"两位数乘一位数"},
		},
		{
			Question: "48÷6=", Subject: "数学",
			AnswerState: AnswerStateUnclear, KnowledgePoints: []string{"整数除法"},
		},
	}}, nil, WithGradingRunDir(t.TempDir()))
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	o.deps.Grounding = grounding
	o.deps.TextbookOwnerID = "desktop-user"
	profile := o.deps.Profiles.(*memProfiles).m["mingming"]
	profile.TextbookEdition = "人教版"
	o.deps.Profiles.(*memProfiles).m["mingming"] = profile
	seedGradingItemActiveTextbookBinding(t, o)

	jobID := runItemResumeJobToAssessing(t, o, "problem-grounding-status-exact-set")
	questions, ok := o.RecognizedQuestions(context.Background(), jobID)
	if !ok || len(questions) != 4 {
		t.Fatalf("recognized status questions=%d ok=%v", len(questions), ok)
	}
	unclearProblemID := ""
	for _, question := range questions {
		if question.Question == "48÷6=" {
			unclearProblemID = question.ProblemID
			break
		}
	}
	if unclearProblemID == "" {
		t.Fatal("unclear problem id is missing")
	}
	_, handled, err := o.ConfirmPhotoGradingJob(
		context.Background(), jobID, ConfirmPhotoGradingInput{
			Corrections: []GradingQuestionCorrection{{
				ProblemID:   unclearProblemID,
				Confirmed:   true,
				AnswerState: AnswerStateUnclear,
			}},
		},
	)
	if err != nil || !handled {
		t.Fatalf("confirm status grounded grading: handled=%v err=%v", handled, err)
	}
	waitForStage(t, o.deps, "mingming", jobID, k12.GradingStageCompleted)
	if solver.callCount() != 3 || grader.callCount() != 1 {
		t.Fatalf("status Provider calls solve/grade=%d/%d want 3/1", solver.callCount(), grader.callCount())
	}
	projection, err := o.ImageTaskHomeworkProjection(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	receipts := decodeProblemGroundingReceipts(t, projection)
	operationsByProblem := make(map[string][]string)
	for _, receipt := range receipts {
		operationsByProblem[receipt.ProblemID] = append(
			operationsByProblem[receipt.ProblemID], receipt.Operation,
		)
	}
	assessments, err := o.deps.Records.ListGradingAssessmentItems(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	assessmentByProblem := make(map[string]k12.GradingAssessmentItem, len(assessments))
	for _, assessment := range assessments {
		assessmentByProblem[assessment.ProblemID] = assessment
	}
	expected := map[string]struct {
		status     k12.GradingAssessmentStatus
		operations []string
	}{
		"57+38=":          {status: k12.GradingAssessmentCorrect, operations: []string{"solve", "grade"}},
		outOfScopeProblem: {status: k12.GradingAssessmentOutOfScope, operations: []string{"solve"}},
		"26×3=":           {status: k12.GradingAssessmentBlankSolved, operations: []string{"solve"}},
		"48÷6=":           {status: k12.GradingAssessmentAnswerUnclear, operations: nil},
	}
	for _, question := range projection.Questions {
		want, exists := expected[question.Question]
		if !exists {
			t.Fatalf("unexpected public question %q", question.Question)
		}
		assessment, exists := assessmentByProblem[question.ProblemID]
		if !exists || assessment.Status != want.status {
			t.Fatalf("question %q assessment=%+v want status %s", question.Question, assessment, want.status)
		}
		if got := operationsByProblem[question.ProblemID]; !reflect.DeepEqual(got, want.operations) {
			t.Fatalf("question %q operations=%v want %v", question.Question, got, want.operations)
		}
		if len(want.operations) == 0 &&
			(assessment.SolveInvocationID != "" || assessment.GradeInvocationID != "") {
			t.Fatalf("question %q unexpectedly reached Provider: %+v", question.Question, assessment)
		}
		if question.AnswerState == AnswerStateBlank &&
			(question.StudentAnswer != "" || assessment.GradeInvocationID != "" || assessment.ProjectionCreated) {
			t.Fatalf("blank question %q must not acquire a student grade or mistake: %+v", question.Question, assessment)
		}
	}
}

// 空白卷中的 blank_solved 会调用 solve 产生家长讲解，但不得
// 伪造 grade operation 或 grade receipt。
func TestK12ProblemGroundingProjectionBlankSolvedUsesSolveOnly(t *testing.T) {
	grounding := &gradingItemPinnedGrounding{active: "revision-a"}
	solver := &gradingItemGroundedPhysicalSolver{grounding: grounding, solution: "2"}
	grader := &gradingItemGroundedPhysicalGrader{}
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学",
		AnswerState: AnswerStateBlank, KnowledgePoints: []string{"两位数加法"},
	}}}, nil, WithGradingRunDir(t.TempDir()))
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	o.deps.Grounding = grounding
	o.deps.TextbookOwnerID = "desktop-user"
	profile := o.deps.Profiles.(*memProfiles).m["mingming"]
	profile.TextbookEdition = "人教版"
	o.deps.Profiles.(*memProfiles).m["mingming"] = profile
	seedGradingItemActiveTextbookBinding(t, o)

	request := orchestratorPhotoRequest()
	request.TaskIntent = PhotoTaskBlankWorksheet
	started, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: request, SourceKind: "im", SourceKey: "problem-grounding-blank-solved",
	})
	if err != nil || !created {
		t.Fatalf("start blank worksheet: created=%v err=%v", created, err)
	}
	jobID := started.Record.RecordID
	freezeItemResumeBudget(t, o, jobID)
	view, err := o.RunGradingJob(context.Background(), jobID)
	if err != nil || view.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("run blank worksheet to confirmation: stage=%s err=%v", view.Record.Status, err)
	}
	waitGradingView(t, o, jobID, func(candidate GradingJobView) bool {
		return candidate.Fields.AnchorState == k12.GradingAnchorLocated ||
			candidate.Fields.AnchorState == k12.GradingAnchorDegraded
	})
	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil {
		t.Fatalf("complete blank worksheet: %v", err)
	}
	if completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("blank worksheet stage=%s want completed", completed.Record.Status)
	}
	if solver.callCount() != 1 || grader.callCount() != 0 {
		t.Fatalf("blank Provider calls solve/grade=%d/%d want 1/0", solver.callCount(), grader.callCount())
	}
	projection, err := o.ImageTaskHomeworkProjection(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Questions) != 1 {
		t.Fatalf("blank public questions=%d want 1", len(projection.Questions))
	}
	receipts := decodeProblemGroundingReceipts(t, projection)
	if len(receipts) != 1 || receipts[0].ProblemID != projection.Questions[0].ProblemID ||
		receipts[0].Operation != "solve" {
		t.Fatalf("blank_solved receipts=%+v want solve only", receipts)
	}
	assessments, err := o.deps.Records.ListGradingAssessmentItems(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(assessments) != 1 || assessments[0].Status != k12.GradingAssessmentBlankSolved ||
		assessments[0].SolveInvocationID == "" || assessments[0].GradeInvocationID != "" {
		t.Fatalf("blank_solved assessment=%+v", assessments)
	}
	artifact, err := o.deps.Records.GetGradingFinalArtifactByJob(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"共 1 题 · 1 题已解答", "## 已解答", "**正确答案：** 2"} {
		if !strings.Contains(artifact.CanonicalMarkdown, want) {
			t.Fatalf("blank final artifact lacks %q: %s", want, artifact.CanonicalMarkdown)
		}
	}
	for _, forbidden := range []string{"0 题正确", "1 题需关注", "## 需关注的题", "## 已答对的题"} {
		if strings.Contains(artifact.CanonicalMarkdown, forbidden) {
			t.Fatalf("blank final artifact invents a grading result %q: %s", forbidden, artifact.CanonicalMarkdown)
		}
	}
}

// 关闭并重开真实 SQLite 后，逐题 receipt 必须逐值相同，且 mutable
// active revision 的变化不能触发再次检索。
func TestK12ProblemGroundingProjectionSurvivesSQLiteRestartWithoutRetrieval(t *testing.T) {
	o, jobID, grounding, solver, grader := completedProblemGroundingFixture(t)
	before, err := o.ImageTaskHomeworkProjection(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	want := decodeProblemGroundingReceipts(t, before)
	freezesBefore, legacyBefore, queriesBefore := grounding.snapshot()
	solveCallsBefore, gradeCallsBefore := solver.callCount(), grader.callCount()
	grounding.switchActive("revision-after-restart")

	var databasePath string
	if err := o.deps.Records.DB().QueryRowContext(
		context.Background(), `SELECT file FROM pragma_database_list WHERE name='main'`,
	).Scan(&databasePath); err != nil {
		t.Fatal(err)
	}
	if err := o.deps.Records.DB().Close(); err != nil {
		t.Fatal(err)
	}
	reopenedDB, err := sql.Open("sqlite", databasePath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	reopenedDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = reopenedDB.Close() })
	if err := reopenedDB.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	constraint := k12.NewCurriculumStub()
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(constraint)); err != nil {
		t.Fatal(err)
	}
	restartedDeps := o.deps
	restartedDeps.Records = k12storage.NewStore(reopenedDB, registry.Records)
	restartedDeps.Constraint = constraint
	restarted := &GradingOrchestrator{deps: restartedDeps}
	after, err := restarted.ImageTaskHomeworkProjection(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeProblemGroundingReceipts(t, after)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("problem grounding projection drifted after restart:\n before=%+v\n after=%+v", want, got)
	}
	freezesAfter, legacyAfter, queriesAfter := grounding.snapshot()
	if freezesAfter != freezesBefore || legacyAfter != legacyBefore || len(queriesAfter) != len(queriesBefore) ||
		solver.callCount() != solveCallsBefore || grader.callCount() != gradeCallsBefore {
		t.Fatalf(
			"restart projection caused external work: freeze=%d->%d legacy=%d->%d query=%d->%d solve=%d->%d grade=%d->%d",
			freezesBefore, freezesAfter, legacyBefore, legacyAfter, len(queriesBefore), len(queriesAfter),
			solveCallsBefore, solver.callCount(), gradeCallsBefore, grader.callCount(),
		)
	}
}

func rewriteProblemGroundingInvocation(
	t *testing.T,
	o *GradingOrchestrator,
	invocationID, resultJSON string,
) {
	t.Helper()
	if _, err := o.deps.Records.DB().ExecContext(
		context.Background(),
		`UPDATE k12_grading_item_invocations SET result_json=?,result_digest=?
		 WHERE agent_name='mingming' AND item_invocation_id=?`,
		resultJSON, modelInvocationDigest([]byte(resultJSON)), invocationID,
	); err != nil {
		t.Fatal(err)
	}
}

// 缺失、重复、畸形或跨 operation 身份漂移的持久证据都必须 fail closed，
// 公开读取不能补检索或再次调用模型。
func TestK12ProblemGroundingProjectionRejectsInvalidDurableLineage(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *GradingOrchestrator, string)
	}{
		{
			name: "missing grounding envelope",
			mutate: func(t *testing.T, o *GradingOrchestrator, jobID string) {
				items, err := o.deps.Records.ListGradingAssessmentItems(context.Background(), "mingming", jobID)
				if err != nil || len(items) == 0 {
					t.Fatalf("list assessments: len=%d err=%v", len(items), err)
				}
				invocation, err := o.deps.Records.GetGradingItemInvocation(
					context.Background(), "mingming", items[0].GradeInvocationID,
				)
				if err != nil {
					t.Fatal(err)
				}
				var envelope gradingGroundedPhysicalEnvelope
				if err := json.Unmarshal([]byte(invocation.ResultJSON), &envelope); err != nil {
					t.Fatal(err)
				}
				rewriteProblemGroundingInvocation(t, o, invocation.InvocationID, string(envelope.Payload))
			},
		},
		{
			name: "duplicate receipt",
			mutate: func(t *testing.T, o *GradingOrchestrator, jobID string) {
				items, err := o.deps.Records.ListGradingAssessmentItems(context.Background(), "mingming", jobID)
				if err != nil || len(items) == 0 {
					t.Fatalf("list assessments: len=%d err=%v", len(items), err)
				}
				invocation, err := o.deps.Records.GetGradingItemInvocation(
					context.Background(), "mingming", items[0].SolveInvocationID,
				)
				if err != nil {
					t.Fatal(err)
				}
				var envelope gradingGroundedPhysicalEnvelope
				if err := json.Unmarshal([]byte(invocation.ResultJSON), &envelope); err != nil {
					t.Fatal(err)
				}
				envelope.Grounding.Receipts = append(
					envelope.Grounding.Receipts,
					envelope.Grounding.Receipts[0],
				)
				raw, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				rewriteProblemGroundingInvocation(t, o, invocation.InvocationID, string(raw))
			},
		},
		{
			name: "malformed envelope",
			mutate: func(t *testing.T, o *GradingOrchestrator, jobID string) {
				items, err := o.deps.Records.ListGradingAssessmentItems(context.Background(), "mingming", jobID)
				if err != nil || len(items) == 0 {
					t.Fatalf("list assessments: len=%d err=%v", len(items), err)
				}
				rewriteProblemGroundingInvocation(t, o, items[0].SolveInvocationID, "{")
			},
		},
		{
			name: "solve grade identity drift",
			mutate: func(t *testing.T, o *GradingOrchestrator, jobID string) {
				items, err := o.deps.Records.ListGradingAssessmentItems(context.Background(), "mingming", jobID)
				if err != nil || len(items) != 2 {
					t.Fatalf("list assessments: len=%d err=%v", len(items), err)
				}
				donor, err := o.deps.Records.GetGradingItemInvocation(
					context.Background(), "mingming", items[1].GradeInvocationID,
				)
				if err != nil {
					t.Fatal(err)
				}
				rewriteProblemGroundingInvocation(t, o, items[0].GradeInvocationID, donor.ResultJSON)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			o, jobID, grounding, solver, grader := completedProblemGroundingFixture(t)
			test.mutate(t, o, jobID)
			freezesBefore, legacyBefore, queriesBefore := grounding.snapshot()
			solveBefore, gradeBefore := solver.callCount(), grader.callCount()
			if _, err := o.ImageTaskHomeworkProjection(context.Background(), "mingming", jobID); err == nil {
				t.Fatal("invalid durable problem grounding lineage was projected")
			}
			freezesAfter, legacyAfter, queriesAfter := grounding.snapshot()
			if freezesAfter != freezesBefore || legacyAfter != legacyBefore || len(queriesAfter) != len(queriesBefore) ||
				solver.callCount() != solveBefore || grader.callCount() != gradeBefore {
				t.Fatal("fail-closed projection performed external work")
			}
		})
	}
}

// 没有冻结教材命中时，公开逐题加法投影必须是空数组，不能把普通模型
// 结果伪装成教材来源。
func TestK12ProblemGroundingProjectionNoHitIsEmpty(t *testing.T) {
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "57+38=", Subject: "数学", StudentAnswer: "95",
		AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数加法"},
	}}}, nil, WithGradingRunDir(t.TempDir()))
	jobID := runItemResumeJobToAssessing(t, o, "problem-grounding-no-hit")
	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("grading stage=%s want completed", completed.Record.Status)
	}
	projection, err := o.ImageTaskHomeworkProjection(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeProblemGroundingReceipts(t, projection); len(got) != 0 {
		t.Fatalf("no-hit problem grounding receipts=%+v want []", got)
	}
}

// ImageTask result 必须保留逐题加法投影，不能只在 target projection 中存在。
func TestK12ImageTaskResultCarriesProblemGroundingReceipts(t *testing.T) {
	var projection ImageTaskHomeworkProjection
	if err := json.Unmarshal([]byte(`{
		"problem_grounding_receipts":[{
			"problem_id":"problem-result","operation":"solve",
			"identity_digest":"sha256:problem-result",
			"textbook_binding_id":"binding-result","textbook_manifest_id":"manifest-result",
			"document_id":"document-result","document_generation":1,
			"vector_revision_id":"revision-result","query_digest":"sha256:query-result",
			"chunk_id":"chunk-result","logical_page":1,"pdf_page":2,
			"source_digest":"source-result","citation_digest":"citation-result"
		}]
	}`), &projection); err != nil {
		t.Fatal(err)
	}
	projection.Stage = k12.GradingStageCompleted
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	coordinator.Grading = &blockingImageTaskPhotoResultGrading{
		imageTaskGradingStub: imageTaskGradingStub{jobID: "problem-grounding-result-job"},
		projection:           projection,
		release:              make(chan struct{}),
	}
	view, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !created {
		t.Fatalf("create/run image task: created=%v err=%v", created, err)
	}
	result, err := coordinator.Result(context.Background(), "mingming", view.Dispatch.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeProblemGroundingReceipts(t, result)
	if len(got) != 1 || got[0].ProblemID != "problem-result" || got[0].Operation != "solve" {
		t.Fatalf("ImageTask result omitted problem grounding receipts: %+v", got)
	}
}
