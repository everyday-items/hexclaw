package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type finalSummaryCrashFixture struct {
	orchestrator *GradingOrchestrator
	run          *gradingRun
	job          GradingJobView
	invocation   k12.ModelInvocation
	tips         TutoringTips
	resultJSON   string
	provider     *bug20260726031TipsSpy
}

func prepareFinalSummaryCrashFixture(t *testing.T) finalSummaryCrashFixture {
	t.Helper()
	deps, store := newPipeline(
		t,
		fakeSolver{
			solution: "2",
			ev: SolveEvidence{
				Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec,
			},
		},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		nil,
	)
	provider := &bug20260726031TipsSpy{}
	deps.TutoringTipsReview = provider
	deps.Profiles = &bug20260726031ProfileStore{
		profile: k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级下"},
	}
	orchestrator := &GradingOrchestrator{deps: deps}

	const (
		submissionID = "summary-result-crash-submission"
		problemID    = "summary-result-crash-problem"
		attemptID    = "summary-result-crash-attempt"
		inputDigest  = "sha256:summary-result-crash-input-v1"
	)
	if err := store.PutProblemAttemptSnapshot(
		context.Background(),
		k12.ProblemAttemptSnapshot{
			Problems: []k12.Problem{{
				ProblemID: problemID, AgentName: "mingming",
				SubmissionID: submissionID, PageAssetID: "page-summary-result-crash",
				Ordinal: 0, ProblemKind: k12.ProblemKindStandalone,
				SourceNumberPath: []string{"1"}, DisplayLabel: "1.", Subject: "数学",
				StemRaw: "1+1=", StemMarkdown: "1+1=", ConceptIDs: []string{"加法"},
				CanonicalVersion: 1, CreatedAt: 100, UpdatedAt: 100,
			}},
			Attempts: []k12.Attempt{{
				AttemptID: attemptID, AgentName: "mingming", SubmissionID: submissionID,
				ProblemID: problemID, AnswerState: "present", AnswerRaw: "2",
				AnswerMarkdown: "2", ConfirmedVersion: 1, InputDigest: inputDigest,
				CreatedAt: 100, UpdatedAt: 100,
			}},
		},
	); err != nil {
		t.Fatalf("seed durable problem/attempt: %v", err)
	}
	jobRecord, err := k12.NewGradingJobRecord(
		"mingming",
		"session-summary-result-crash",
		k12.GradingJobFields{
			SubmissionID: submissionID, SourceKind: "test",
			IdempotencyKey: k12.BuildGradingIdempotencyKey(
				"test", "summary-result-crash", 1,
			),
			ConfirmedVersion: 1, ConfirmationState: k12.GradingConfirmationConfirmed,
			AnchorState: k12.GradingAnchorLocated,
			ModelSnapshot: k12.GradingModelSnapshot{
				Provider: "provider-a", Model: "model-a", Route: "provider-a/model-a",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	jobRecord.Status = k12.GradingStageProjecting
	if created, putErr := store.Put(context.Background(), jobRecord); putErr != nil || !created {
		t.Fatalf("seed projecting grading job: created=%v err=%v", created, putErr)
	}
	assessment, created, err := store.CommitGradingAssessmentItem(
		context.Background(),
		k12.GradingAssessmentItem{
			AgentName: "mingming", JobID: jobRecord.RecordID,
			ProblemID: problemID, AttemptID: attemptID,
			ConfirmedVersion: 1, InputRevision: 1,
			StructureVersion: 1, InputDigest: inputDigest,
			Status:           k12.GradingAssessmentUnanswered,
			ResultJSON:       `{"status":"unanswered"}`,
			ResultDigest:     "sha256:summary-result-crash-assessment",
			ProjectionStatus: k12.GradingProjectionCommitted,
			CreatedAt:        200,
		},
		k12storage.GradingAssessmentEffects{},
	)
	if err != nil || !created {
		t.Fatalf("seed current assessment: assessment=%+v created=%v err=%v", assessment, created, err)
	}
	job, err := deps.GetGradingJob(
		context.Background(), "mingming", jobRecord.RecordID,
	)
	if err != nil {
		t.Fatalf("reload projecting job: %v", err)
	}
	run := &gradingRun{
		agentName: "mingming",
		questions: []RecognizedQuestion{{
			ProblemID: problemID, ProblemKind: ProblemKindStandalone,
			SourceNumberPath: []string{"1"}, DisplayLabel: "1.",
			PageAssetID: "page-summary-result-crash", AttemptID: attemptID,
			RawTranscription: "1+1=", CanonicalMarkdown: "1+1=",
			AnswerRawTranscription: "2", AnswerCanonicalMarkdown: "2",
			CanonicalVersion: 1, ConfirmedVersion: 1, InputDigest: inputDigest,
			Question: "1+1=", KnowledgePoints: []string{"加法"},
			AnswerState: AnswerStatePresent, StudentAnswer: "2", Subject: "数学",
		}},
	}

	orderedDigestsJSON, err := json.Marshal([]string{assessment.ResultDigest})
	if err != nil {
		t.Fatal(err)
	}
	attempt := job.Fields.AttemptCount + 1
	if attempt < 1 {
		attempt = 1
	}
	invocation, created, err := orchestrator.deps.Records.PrepareModelInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID: "summary-result-crash-invocation",
			AgentName:    run.agentName,
			JobID:        jobRecord.RecordID,
			Stage:        k12.GradingStageProjecting,
			RequestDigest: modelInvocationDigest(
				[]byte("structure:1"),
				orderedDigestsJSON,
			),
			RouteSnapshot: job.Fields.ModelSnapshot,
			Attempt:       attempt,
			CreatedAt:     orchestrator.deps.now(),
			UpdatedAt:     orchestrator.deps.now(),
		},
	)
	if err != nil || !created {
		t.Fatalf("prepare summary invocation: created=%v err=%v", created, err)
	}
	invocation, err = orchestrator.deps.Records.MarkModelInvocationSent(
		context.Background(), invocation.AgentName, invocation.InvocationID, "",
	)
	if err != nil {
		t.Fatalf("mark summary sent: %v", err)
	}
	tips := TutoringTips{
		GradingJobID:    jobRecord.RecordID,
		SubmissionID:    job.Fields.SubmissionID,
		Grade:           "五年级下",
		Subject:         "数学",
		KnowledgePoints: []string{"加法"},
		Sections: []TutoringTipsSection{
			{
				Title: "这页在练什么", Content: "练习加法。",
				SourceLabel: TutoringTipsSourceAI,
			},
			{
				Title: "小明要留意", Content: "留意计算步骤。",
				SourceLabel: TutoringTipsSourceLearningEvidence,
			},
			{
				Title: "每道题怎么带（不直接给答案）", Content: "先请孩子说清题意。",
				SourceLabel: TutoringTipsSourceAI,
			},
		},
	}
	resultJSON, err := json.Marshal(tips)
	if err != nil {
		t.Fatal(err)
	}
	return finalSummaryCrashFixture{
		orchestrator: orchestrator,
		run:          run,
		job:          job,
		invocation:   invocation,
		tips:         tips,
		resultJSON:   string(resultJSON),
		provider:     provider,
	}
}

func (f finalSummaryCrashFixture) restartedFinalizer() *GradingOrchestrator {
	restarted := &GradingOrchestrator{deps: f.orchestrator.deps}
	// A new Store proves recovery reads only the durable invocation ledger; no
	// process-local summary cache is carried across the simulated restart.
	restarted.deps.Records = k12storage.NewStore(
		f.orchestrator.deps.Records.DB(),
		nil,
	)
	return restarted
}

// K12-FINAL-SUMMARY-RECOVERY-001: crash after the provider result and atomic
// invocation success, but before final-artifact commit, must replay the typed
// durable payload and never send the summary request again.
func TestGradingFinalizerRecoversSucceededSummaryPayloadWithoutProviderResend(t *testing.T) {
	fixture := prepareFinalSummaryCrashFixture(t)
	stored, err := fixture.orchestrator.deps.Records.MarkModelInvocationSucceededWithResult(
		context.Background(),
		fixture.invocation.AgentName,
		fixture.invocation.InvocationID,
		modelInvocationResultDigest(fixture.tips),
		fixture.resultJSON,
		"",
	)
	if err != nil || stored.ResultJSON == "" {
		t.Fatalf("persist successful summary payload: stored=%+v err=%v", stored, err)
	}
	if _, err := fixture.orchestrator.deps.Records.GetGradingFinalArtifactByJob(
		context.Background(), fixture.invocation.AgentName, fixture.invocation.JobID,
	); err == nil {
		t.Fatal("crash fixture unexpectedly has a final artifact")
	}

	artifact, err := fixture.restartedFinalizer().finalizeGradingPage(
		context.Background(), fixture.run, fixture.job,
	)
	if err != nil {
		t.Fatalf("restart finalization from durable summary payload: %v", err)
	}
	if fixture.provider.calls != 0 {
		t.Fatalf("restart resent page summary provider %d times, want 0", fixture.provider.calls)
	}
	if artifact.SummaryInvocationID != fixture.invocation.InvocationID {
		t.Fatalf("artifact summary invocation=%q want %q",
			artifact.SummaryInvocationID, fixture.invocation.InvocationID)
	}
	if artifact.ArtifactDigest == "" {
		t.Fatal("restart did not commit the final artifact")
	}
}

// K12-FINAL-SUMMARY-RECOVERY-002: reconciliation-only source recovery may
// consume a conclusive, already-durable page-summary result. The restriction
// applies to creating/sending external work, not to committing local effects
// from an exact succeeded ledger payload.
func TestGradingFinalizerReconciliationOnlyRecoversSucceededSummaryPayload(t *testing.T) {
	fixture := prepareFinalSummaryCrashFixture(t)
	if _, err := fixture.orchestrator.deps.Records.MarkModelInvocationSucceededWithResult(
		context.Background(),
		fixture.invocation.AgentName,
		fixture.invocation.InvocationID,
		modelInvocationResultDigest(fixture.tips),
		fixture.resultJSON,
		"",
	); err != nil {
		t.Fatalf("persist successful summary payload: %v", err)
	}

	artifact, err := fixture.restartedFinalizer().finalizeGradingPage(
		withProblemSourceReconciliationOnly(context.Background()),
		fixture.run,
		fixture.job,
	)
	if err != nil {
		t.Fatalf("reconciliation-only durable summary recovery: %v", err)
	}
	if fixture.provider.calls != 0 {
		t.Fatalf("reconciliation-only recovery resent provider %d times, want 0", fixture.provider.calls)
	}
	if artifact.SummaryInvocationID != fixture.invocation.InvocationID ||
		artifact.ArtifactDigest == "" {
		t.Fatalf("reconciliation-only recovery did not commit exact artifact: %+v", artifact)
	}
}

func TestGradingFinalizerReconciliationOnlyNeverCreatesOrSendsMissingOrPreparedSummary(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(t *testing.T, fixture finalSummaryCrashFixture)
		wantLedger int
		wantStatus k12.ModelInvocationStatus
	}{
		{
			name: "missing",
			mutate: func(t *testing.T, fixture finalSummaryCrashFixture) {
				t.Helper()
				if _, err := fixture.orchestrator.deps.Records.DB().Exec(`
					DELETE FROM k12_model_invocations WHERE invocation_id=?`,
					fixture.invocation.InvocationID,
				); err != nil {
					t.Fatal(err)
				}
			},
			wantLedger: 0,
		},
		{
			name: "prepared",
			mutate: func(t *testing.T, fixture finalSummaryCrashFixture) {
				t.Helper()
				if _, err := fixture.orchestrator.deps.Records.DB().Exec(`
					UPDATE k12_model_invocations SET status='prepared'
					WHERE invocation_id=?`, fixture.invocation.InvocationID); err != nil {
					t.Fatal(err)
				}
			},
			wantLedger: 1,
			wantStatus: k12.ModelInvocationPrepared,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := prepareFinalSummaryCrashFixture(t)
			test.mutate(t, fixture)
			_, err := fixture.restartedFinalizer().finalizeGradingPage(
				withProblemSourceReconciliationOnly(context.Background()),
				fixture.run,
				fixture.job,
			)
			if !errors.Is(err, ErrModelInvocationRequiresReconciliation) {
				t.Fatalf("reconciliation-only %s summary err=%v", test.name, err)
			}
			if fixture.provider.calls != 0 {
				t.Fatalf("reconciliation-only %s summary sent provider %d times",
					test.name, fixture.provider.calls)
			}
			var count int
			var status string
			row := fixture.orchestrator.deps.Records.DB().QueryRow(`
				SELECT COUNT(*),COALESCE(MAX(status),'')
				FROM k12_model_invocations
				WHERE agent_name=? AND job_id=? AND stage=?`,
				fixture.invocation.AgentName,
				fixture.invocation.JobID,
				k12.GradingStageProjecting,
			)
			if scanErr := row.Scan(&count, &status); scanErr != nil {
				t.Fatal(scanErr)
			}
			if count != test.wantLedger || status != string(test.wantStatus) {
				t.Fatalf("reconciliation-only %s mutated ledger: count=%d status=%q",
					test.name, count, status)
			}
		})
	}
}

func TestGradingFinalizerRejectsMissingOrCorruptSucceededSummaryPayloadWithoutResend(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, fixture finalSummaryCrashFixture)
	}{
		{
			name: "empty legacy payload",
			mutate: func(t *testing.T, fixture finalSummaryCrashFixture) {
				t.Helper()
				if _, err := fixture.orchestrator.deps.Records.DB().Exec(`
					UPDATE k12_model_invocations
					SET status='succeeded',result_digest=?,result_json=''
					WHERE invocation_id=?`,
					modelInvocationResultDigest(fixture.tips),
					fixture.invocation.InvocationID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "digest payload mismatch",
			mutate: func(t *testing.T, fixture finalSummaryCrashFixture) {
				t.Helper()
				if _, err := fixture.orchestrator.deps.Records.MarkModelInvocationSucceededWithResult(
					context.Background(),
					fixture.invocation.AgentName,
					fixture.invocation.InvocationID,
					modelInvocationResultDigest(fixture.tips),
					fixture.resultJSON,
					"",
				); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.orchestrator.deps.Records.DB().Exec(`
					UPDATE k12_model_invocations
					SET result_json='{"GradingJobID":"tampered","sections":[]}'
					WHERE invocation_id=?`, fixture.invocation.InvocationID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong typed job identity",
			mutate: func(t *testing.T, fixture finalSummaryCrashFixture) {
				t.Helper()
				wrong := fixture.tips
				wrong.GradingJobID = "different-job"
				raw, err := json.Marshal(wrong)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.orchestrator.deps.Records.MarkModelInvocationSucceededWithResult(
					context.Background(),
					fixture.invocation.AgentName,
					fixture.invocation.InvocationID,
					modelInvocationResultDigest(wrong),
					string(raw),
					"",
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := prepareFinalSummaryCrashFixture(t)
			test.mutate(t, fixture)
			_, err := fixture.restartedFinalizer().finalizeGradingPage(
				context.Background(), fixture.run, fixture.job,
			)
			if !errors.Is(err, ErrModelInvocationRequiresReconciliation) {
				t.Fatalf("corrupt summary err=%v, want reconciliation required", err)
			}
			if fixture.provider.calls != 0 {
				t.Fatalf("corrupt summary resent provider %d times, want 0", fixture.provider.calls)
			}
			var artifacts int
			if countErr := fixture.orchestrator.deps.Records.DB().QueryRow(`
				SELECT COUNT(*) FROM k12_grading_final_artifacts
				WHERE agent_name=? AND job_id=?`,
				fixture.invocation.AgentName,
				fixture.invocation.JobID,
			).Scan(&artifacts); countErr != nil {
				t.Fatal(countErr)
			}
			if artifacts != 0 {
				t.Fatalf("corrupt summary committed %d final artifacts", artifacts)
			}
		})
	}
}
