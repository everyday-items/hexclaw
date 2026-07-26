package usecase

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type bug20260726031TipsSpy struct {
	calls int
}

func (s *bug20260726031TipsSpy) GenerateTutoringTipsReview(
	context.Context,
	string,
	string,
	string,
) (string, error) {
	s.calls++
	return "按当前整页结果生成的辅导要点。", nil
}

type bug20260726031ProfileStore struct {
	profile k12.ChildProfile
}

func (s *bug20260726031ProfileStore) GetProfile(
	context.Context,
	string,
) (k12.ChildProfile, error) {
	return s.profile, nil
}

func (s *bug20260726031ProfileStore) SaveProfile(
	_ context.Context,
	_ string,
	profile k12.ChildProfile,
) error {
	s.profile = profile
	return nil
}

type bug20260726031FinalArtifact struct {
	ArtifactID            string
	StructureVersion      int
	CoverageStatus        string
	TotalCount            int
	PublishedCount        int
	SkippedCount          int
	OrderedCurrentDigests string
	CanonicalMarkdown     string
	ArtifactDigest        string
	SummaryInvocationID   string
}

func loadBUG20260726031FinalArtifact(
	t *testing.T,
	o *GradingOrchestrator,
	jobID string,
) bug20260726031FinalArtifact {
	t.Helper()
	var artifact bug20260726031FinalArtifact
	err := o.deps.Records.DB().QueryRow(`
		SELECT artifact_id,structure_version,coverage_status,total_count,
		       published_count,skipped_count,ordered_current_digests_json,
		       canonical_markdown,artifact_digest,summary_invocation_id
		FROM k12_grading_final_artifacts
		WHERE agent_name='mingming' AND job_id=?`,
		jobID,
	).Scan(
		&artifact.ArtifactID,
		&artifact.StructureVersion,
		&artifact.CoverageStatus,
		&artifact.TotalCount,
		&artifact.PublishedCount,
		&artifact.SkippedCount,
		&artifact.OrderedCurrentDigests,
		&artifact.CanonicalMarkdown,
		&artifact.ArtifactDigest,
		&artifact.SummaryInvocationID,
	)
	if err != nil {
		t.Fatalf("BUG_20260726_031 canonical final artifact is not durable: %v", err)
	}
	return artifact
}

func forceBUG20260726031Projecting(
	t *testing.T,
	o *GradingOrchestrator,
	jobID string,
	result PhotoGradeResult,
) *gradingRun {
	t.Helper()
	run := o.lookup(jobID)
	if run == nil {
		t.Fatal("missing grading runtime")
	}
	run.result = &result
	if _, err := o.deps.Records.DB().Exec(`
		UPDATE k12_grading_jobs
		SET status=?,version=version+1,updated_at=updated_at+1
		WHERE agent_name='mingming' AND record_id=?`,
		k12.GradingStageProjecting,
		jobID,
	); err != nil {
		t.Fatalf("move fixture to projecting: %v", err)
	}
	return run
}

func TestBUG_20260726_031_WithSkipsFinalizesHonestArtifactWithoutFullTutoringTips(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "1+1=", Subject: "数学", StudentAnswer: "2",
			AnswerState:      AnswerStatePresent,
			SourceNumberPath: []string{"1"}, DisplayLabel: "1.",
			KnowledgePoints: []string{"加法"},
		},
		{
			Question: "来源看不清", Subject: "数学", StudentAnswer: "3",
			AnswerState:      AnswerStatePresent,
			SourceNumberPath: []string{"2"}, DisplayLabel: "2.",
			KnowledgePoints: []string{"加法"},
		},
	}, solver, grader)
	tips := &bug20260726031TipsSpy{}
	o.deps.TutoringTipsReview = tips

	jobID := runItemResumeJobToAssessing(t, o, "bug-031-final-with-skips")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	first, err := o.assessDurablePhotoItem(
		context.Background(), o.deps, job, run.req, PhotoModeGrade, run.questions[0],
	)
	if err != nil {
		t.Fatalf("commit clear result: %v", err)
	}
	seedBUG20260726031SkipReceipt(t, o, jobID, run.questions[1], "final-with-skips")
	forceBUG20260726031Projecting(t, o, jobID, PhotoGradeResult{
		Items: []PhotoGradeItem{first},
	})

	view, err := o.runProject(context.Background(), run, jobID)
	if err != nil {
		t.Fatalf("with-skips exact-set must finalize: %v", err)
	}
	if view.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("with-skips final stage=%s, want completed", view.Record.Status)
	}
	if tips.calls != 0 {
		t.Fatalf("with-skips invoked full TutoringTips %d times, want zero", tips.calls)
	}

	artifact := loadBUG20260726031FinalArtifact(t, o, jobID)
	if artifact.CoverageStatus != "with_skips" ||
		artifact.TotalCount != 2 ||
		artifact.PublishedCount != 1 ||
		artifact.SkippedCount != 1 {
		t.Fatalf("dishonest with-skips coverage: %+v", artifact)
	}
	if !strings.Contains(artifact.CanonicalMarkdown, "2.") ||
		!strings.Contains(artifact.CanonicalMarkdown, "已跳过") ||
		!strings.Contains(artifact.CanonicalMarkdown, "未判断对错") {
		t.Fatalf("final artifact lost stable skip placeholder: %q", artifact.CanonicalMarkdown)
	}
}

func TestBUG_20260726_031_FullFinalizerKeepsTipsAndBindsOneSummaryToAllCurrentDigests(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "1+1=", Subject: "数学", StudentAnswer: "2",
			AnswerState:      AnswerStatePresent,
			SourceNumberPath: []string{"1"}, DisplayLabel: "1.",
			KnowledgePoints: []string{"加法"},
		},
		{
			Question: "2+2=", Subject: "数学", StudentAnswer: "4",
			AnswerState:      AnswerStatePresent,
			SourceNumberPath: []string{"2"}, DisplayLabel: "2.",
			KnowledgePoints: []string{"加法"},
		},
	}, solver, grader)
	tips := &bug20260726031TipsSpy{}
	o.deps.TutoringTipsReview = tips
	o.deps.Profiles = &bug20260726031ProfileStore{
		profile: k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级下"},
	}

	jobID := runItemResumeJobToAssessing(t, o, "bug-031-final-full")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	items := make([]PhotoGradeItem, 0, len(run.questions))
	for _, question := range run.questions {
		item, err := o.assessDurablePhotoItem(
			context.Background(), o.deps, job, run.req, PhotoModeGrade, question,
		)
		if err != nil {
			t.Fatalf("commit full item %s: %v", question.ProblemID, err)
		}
		items = append(items, item)
	}
	forceBUG20260726031Projecting(t, o, jobID, PhotoGradeResult{Items: items})
	view, err := o.runProject(context.Background(), run, jobID)
	if err != nil {
		t.Fatalf("full exact-set finalization: %v", err)
	}
	if view.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("full final stage=%s, want completed", view.Record.Status)
	}
	if tips.calls != 1 {
		t.Errorf("full coverage TutoringTips calls=%d, want one canonical page call", tips.calls)
	}

	invocations, err := o.deps.Records.ListModelInvocations(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	summaryInvocations := 0
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageProjecting {
			summaryInvocations++
		}
	}
	if summaryInvocations != 1 {
		t.Errorf("page summary invocations=%d, want exactly one", summaryInvocations)
	}

	receipts, err := o.deps.Records.ListGradingAssessmentItems(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantDigests := make([]string, 0, len(receipts))
	for _, receipt := range receipts {
		if receipt.CurrentDisposition == k12.GradingAssessmentDispositionCurrent {
			wantDigests = append(wantDigests, receipt.ResultDigest)
		}
	}
	sort.Strings(wantDigests)
	artifact := loadBUG20260726031FinalArtifact(t, o, jobID)
	var gotDigests []string
	if err := json.Unmarshal([]byte(artifact.OrderedCurrentDigests), &gotDigests); err != nil {
		t.Fatalf("decode final current digests: %v", err)
	}
	sort.Strings(gotDigests)
	if strings.Join(gotDigests, "\x00") != strings.Join(wantDigests, "\x00") {
		t.Fatalf("summary/final artifact digests=%v, want all current %v", gotDigests, wantDigests)
	}
	if artifact.SummaryInvocationID == "" || artifact.ArtifactDigest == "" {
		t.Fatalf("full final artifact is not bound to one summary receipt: %+v", artifact)
	}
}

func TestBUG_20260726_031_UnresolvedDispositionOrOperationBlocksFinalization(t *testing.T) {
	for _, operationStatus := range []string{
		"partial",
		string(k12.ModelInvocationPrepared),
		string(k12.ModelInvocationSent),
		string(k12.ModelInvocationFailed),
		string(k12.ModelInvocationOutcomeUnknown),
	} {
		t.Run(operationStatus, func(t *testing.T) {
			solver := &itemResumeSolver{calls: map[string]int{}}
			grader := &itemResumeGrader{calls: map[string]int{}}
			o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
				{
					Question: "done", Subject: "数学", StudentAnswer: "1",
					AnswerState: AnswerStatePresent,
				},
				{
					Question: "not-terminal", Subject: "数学", StudentAnswer: "2",
					AnswerState: AnswerStatePresent,
				},
			}, solver, grader)
			jobID := runItemResumeJobToAssessing(
				t, o, "bug-031-final-block-"+operationStatus,
			)
			run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
			first, err := o.assessDurablePhotoItem(
				context.Background(), o.deps, job, run.req, PhotoModeGrade, run.questions[0],
			)
			if err != nil {
				t.Fatal(err)
			}
			if operationStatus != "partial" {
				now := o.deps.now()
				invocation, _, prepareErr := o.deps.Records.PrepareGradingItemInvocation(
					context.Background(),
					k12.GradingItemInvocation{
						InvocationID:     "bug-031-unresolved-" + operationStatus,
						AgentName:        "mingming",
						JobID:            jobID,
						ProblemID:        run.questions[1].ProblemID,
						AttemptID:        run.questions[1].AttemptID,
						Operation:        k12.GradingItemOperationSolveGenerate,
						OperationAttempt: 1,
						RequestDigest:    "sha256:unresolved-" + operationStatus,
						RouteSnapshot:    job.Fields.ModelSnapshot,
						CreatedAt:        now,
						UpdatedAt:        now,
					},
				)
				if prepareErr != nil {
					t.Fatalf("prepare unresolved invocation: %v", prepareErr)
				}
				failureClass, failureCode := "", ""
				if operationStatus == string(k12.ModelInvocationFailed) ||
					operationStatus == string(k12.ModelInvocationOutcomeUnknown) {
					failureClass = "provider"
					failureCode = "wave3_unresolved"
				}
				if _, updateErr := o.deps.Records.DB().Exec(`
					UPDATE k12_grading_item_invocations
					SET status=?,failure_class=?,failure_code=?,updated_at=updated_at+1
					WHERE item_invocation_id=?`,
					operationStatus, failureClass, failureCode, invocation.InvocationID,
				); updateErr != nil {
					t.Fatalf("set unresolved status: %v", updateErr)
				}
			}

			forceBUG20260726031Projecting(t, o, jobID, PhotoGradeResult{
				Items: []PhotoGradeItem{first},
			})
			view, projectErr := o.runProject(context.Background(), run, jobID)
			if projectErr == nil && view.Record.Status == k12.GradingStageCompleted {
				t.Fatalf("%s unresolved state incorrectly froze a final artifact", operationStatus)
			}
			var artifacts int
			countErr := o.deps.Records.DB().QueryRow(`
				SELECT COUNT(*) FROM k12_grading_final_artifacts
				WHERE agent_name='mingming' AND job_id=?`,
				jobID,
			).Scan(&artifacts)
			if countErr != nil && !strings.Contains(countErr.Error(), "no such table") {
				t.Fatal(countErr)
			}
			if artifacts != 0 {
				t.Fatalf("%s unresolved state wrote %d final artifacts", operationStatus, artifacts)
			}
		})
	}
}

func TestBUG_20260726_031_OneFinalizerAndArtifactOnlyPrintPDFIMBoundary(t *testing.T) {
	orchestratorRaw, err := os.ReadFile("gradingjob_orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	orchestrator := string(orchestratorRaw)
	if strings.Count(orchestrator, "func (o *GradingOrchestrator) runProject(") != 1 {
		t.Fatal("production must expose exactly one page finalization entry")
	}
	projectStart := strings.Index(orchestrator, "func (o *GradingOrchestrator) runProject(")
	projectEnd := strings.Index(orchestrator[projectStart:], "\n}\n")
	if projectStart < 0 || projectEnd < 0 {
		t.Fatal("cannot inspect canonical finalizer boundary")
	}
	projectBody := orchestrator[projectStart : projectStart+projectEnd+3]
	if strings.Contains(projectBody, "return o.advanceOK(") ||
		!strings.Contains(strings.ToLower(projectBody), "final") {
		t.Errorf("legacy page writer still bypasses the one canonical finalizer:\n%s", projectBody)
	}

	imageTaskRaw, err := os.ReadFile("image_task.go")
	if err != nil {
		t.Fatal(err)
	}
	printRaw, err := os.ReadFile("generic_print.go")
	if err != nil {
		t.Fatal(err)
	}
	deliveryRaw, err := os.ReadFile("../apihttp/delivery_receipt_handler.go")
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{
		"result":    string(imageTaskRaw),
		"print/PDF": string(printRaw),
		"formal IM": string(deliveryRaw),
	} {
		if !strings.Contains(strings.ToLower(source), "final_artifact") {
			t.Errorf("%s has no canonical final_artifact read boundary", name)
		}
	}
	if strings.Contains(string(deliveryRaw), `json:"content"`) {
		t.Error("formal IM still accepts page-writable content instead of final_artifact identity")
	}
}
