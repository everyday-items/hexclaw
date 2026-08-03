package k12storage_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/viewcontract"
)

func TestProblemSourceArchiveV6RoundTripPreservesCurrentSourceTypedFactsAndRecoversOnlyFromCommittedResult(t *testing.T) {
	ctx := context.Background()
	sourceStore, sourceDB := setup(t)
	lease := seedProblemSourceRecognitionFixture(t, sourceStore, sourceDB, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, sourceDB)
	seedProblemSourceArchiveExtraReferencedAsset(t, sourceDB)
	committed, created, err := sourceStore.CommitProblemSourceRecognitionResult(
		ctx,
		lease,
		validProblemSourceRecognitionResult(),
	)
	if err != nil || !created {
		t.Fatalf("commit V73 source result: created=%v err=%v", created, err)
	}
	freezeProblemSourceArchiveReceipt(t, sourceDB, committed)
	if _, err := sourceDB.Exec(`UPDATE k12_problem_source_reprocess_jobs
		SET reconciliation_epoch=7,reconciliation_attempt_count=2
		WHERE work_id=?`, recognitionWork); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceDB.Exec(`UPDATE k12_grading_jobs
		SET finalization_generation=4 WHERE agent_name=? AND record_id=?`,
		"mingming", recognitionJob); err != nil {
		t.Fatal(err)
	}
	finalArtifact, replay, err := sourceStore.CommitGradingFinalArtifact(
		ctx,
		k12.GradingFinalArtifact{
			AgentName: "mingming", JobID: recognitionJob,
			StructureVersion: 1,
			CoverageStatus:   k12.GradingFinalArtifactCoverageWithSkips,
			TotalCount:       1, PublishedCount: 0, SkippedCount: 1,
			OrderedCurrentDigestsJSON: `["skip-v6-generation-4"]`,
			CanonicalMarkdown:         "# restored typed final artifact",
			ArtifactDigest:            repeatHex("f", 64),
		},
		4,
	)
	if err != nil || replay {
		t.Fatalf("seed final artifact generation: replay=%v err=%v", replay, err)
	}

	archive, err := sourceStore.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatalf("export source archive: %v", err)
	}
	if len(archive.PageAssets) != 2 || len(archive.InputRevisions) != 6 ||
		len(archive.ActionReceipts) != 1 || len(archive.ReprocessJobs) != 1 ||
		len(archive.FinalizationGenerations) != 1 ||
		len(archive.RecognitionResults) != 1 || len(archive.RecognitionItems) != 2 ||
		len(archive.RecognitionPhysicalResults) != 2 {
		t.Fatalf("v6 source exact-set incomplete: %+v", archive)
	}
	if generation := archive.FinalizationGenerations[0]; generation.AgentName != "mingming" || generation.JobID != recognitionJob ||
		generation.Generation != 4 || generation.Artifact == nil ||
		!reflect.DeepEqual(*generation.Artifact, finalArtifact) {
		t.Fatalf("v6 source finalization generation drifted: %+v", generation)
	}
	if archive.ReprocessJobs[0].Status != k12storage.ProblemSourceReprocessRunning {
		t.Fatalf("export must preserve original queue evidence: %+v", archive.ReprocessJobs[0])
	}
	for _, invocation := range archive.ModelInvocations {
		if invocation.ProviderIdempotencyKey != "" || invocation.ExternalRequestID != "" {
			t.Fatalf("archive leaked provider control identifiers: %+v", invocation)
		}
	}
	for _, invocation := range archive.ModelPhysicalInvocations {
		if invocation.ExternalRequestID != "" {
			t.Fatalf("archive leaked physical provider request identifier: %+v", invocation)
		}
	}
	if archive.ReprocessJobs[0].ReconciliationEpoch != 7 ||
		archive.ReprocessJobs[0].ReconciliationAttemptCount != 2 ||
		archive.ReprocessJobs[0].NextReconcileAtMilli != 0 {
		t.Fatalf("export lost reconciliation audit fields: %+v", archive.ReprocessJobs[0])
	}

	targetStore, targetDB := setup(t)
	seedProblemSourceArchiveTargetParents(t, targetDB)
	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := targetStore.ImportProblemSourceArchiveV6Tx(ctx, tx, "mingming", archive); err != nil {
		t.Fatalf("import v6 source chain: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var restoredGeneration int64
	if err := targetDB.QueryRowContext(ctx, `SELECT finalization_generation
		FROM k12_grading_jobs WHERE agent_name=? AND record_id=?`,
		"mingming", recognitionJob).Scan(&restoredGeneration); err != nil {
		t.Fatal(err)
	}
	if restoredGeneration != 4 {
		t.Fatalf("restored finalization generation=%d want 4", restoredGeneration)
	}
	restoredArtifact, err := targetStore.GetGradingFinalArtifactByJob(
		ctx, "mingming", recognitionJob,
	)
	if err != nil || !reflect.DeepEqual(restoredArtifact, finalArtifact) {
		t.Fatalf("restored final artifact drifted: got=%+v want=%+v err=%v", restoredArtifact, finalArtifact, err)
	}

	restoredHeads, err := targetStore.ListCurrentProblemInputRevisions(
		ctx,
		"mingming",
		recognitionSubmission,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, problemID := range []string{recognitionChildOne, recognitionChildTwo} {
		head := restoredHeads[problemID]
		if head.InputRevision != 3 || head.SourceRegion == nil ||
			head.SourceRegion.X != 10 || head.SourceRegion.Y != 20 ||
			head.SourceRegion.Width != 100 || head.SourceRegion.Height != 80 ||
			head.SourceWidth != 200 || head.SourceHeight != 120 {
			t.Fatalf("restored current source head %s drifted: %+v", problemID, head)
		}
	}
	restoredFacts, err := targetStore.ListCurrentProblemSourceRecognitionFacts(
		ctx,
		"mingming",
		recognitionSubmission,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantFact := committed.Items[0]
	gotFact := restoredFacts[recognitionChildOne]
	if gotFact.AnswerState != wantFact.AnswerState ||
		gotFact.AnswerCanonicalMarkdown != wantFact.AnswerCanonicalMarkdown ||
		gotFact.ConfirmationRequired != wantFact.ConfirmationRequired ||
		!reflect.DeepEqual(gotFact.ConfirmationReasons, wantFact.ConfirmationReasons) {
		t.Fatalf("restored V73 answer/risk drifted: got=%+v want=%+v", gotFact, wantFact)
	}
	restoredWork, err := targetStore.GetProblemSourceReprocessJob(
		ctx,
		recognitionOwner,
		recognitionWork,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restoredWork.Status != k12storage.ProblemSourceReprocessQueued ||
		restoredWork.LeaseOwner != "" || restoredWork.LeaseExpiresAtMilli != 0 ||
		restoredWork.ReconciliationOwner != "" ||
		restoredWork.ReconciliationExpiresAtMilli != 0 ||
		restoredWork.ReconciliationEpoch != 7 ||
		restoredWork.ReconciliationAttemptCount != 2 ||
		restoredWork.NextReconcileAtMilli != 0 {
		t.Fatalf("committed-result work was not safely re-queued for local finalization: %+v", restoredWork)
	}
	recoverable, err := targetStore.ListRecoverableProblemSourceReprocessJobs(
		ctx,
		time.Now().Add(24*time.Hour),
		32,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 1 || recoverable[0].WorkID != recognitionWork {
		t.Fatalf("only work backed by a committed V73 result may be recoverable: %+v", recoverable)
	}
	if _, err := targetStore.GetProblemSourceRecognitionResultByWork(
		ctx,
		recognitionOwner,
		recoverable[0].WorkID,
	); err != nil {
		t.Fatalf("recoverable restored work has no provider-free V73 result: %v", err)
	}
	var rawProviderResult sql.NullString
	if err := targetDB.QueryRowContext(ctx, `SELECT result_content
		FROM k12_model_physical_invocations
		WHERE physical_invocation_id='recognition-physical-1'`).Scan(&rawProviderResult); err != nil {
		t.Fatal(err)
	}
	if rawProviderResult.Valid {
		t.Fatalf("archive restore copied raw provider payload: %q", rawProviderResult.String)
	}
	var frozenResponse string
	if err := targetDB.QueryRowContext(ctx, `SELECT response_json
		FROM k12_problem_source_action_receipts
		WHERE command_receipt_id=?`, recognitionReceipt).Scan(&frozenResponse); err != nil {
		t.Fatal(err)
	}
	if string(archive.ActionReceipts[0].ResponseJSON) != frozenResponse {
		t.Fatalf("frozen receipt replay bytes drifted: got=%s want=%s", frozenResponse, archive.ActionReceipts[0].ResponseJSON)
	}
}

func TestProblemSourceArchiveV6RejectsUnboundV73DigestLineage(t *testing.T) {
	ctx := context.Background()
	store, db := setup(t)
	lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, db)
	commit, created, err := store.CommitProblemSourceRecognitionResult(
		ctx, lease, validProblemSourceRecognitionResult(),
	)
	if err != nil || !created {
		t.Fatalf("commit V73 source result: created=%v err=%v", created, err)
	}
	freezeProblemSourceArchiveReceipt(t, db, commit)
	archive, err := store.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if err := k12storage.ValidateProblemSourceArchiveV6("mingming", archive); err != nil {
		t.Fatalf("valid archive rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*k12storage.ProblemSourceArchiveV6)
		want   string
	}{
		{
			name: "parent typed result digest",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ModelInvocations[0].ResultDigest = "sha256:" + repeatHex("c", 64)
			},
			want: "typed result digest",
		},
		{
			name: "parent request digest",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				fake := "sha256:" + repeatHex("d", 64)
				candidate.ModelInvocations[0].RequestDigest = fake
				candidate.RecognitionResults[0].ParentRequestDigest = fake
			},
			want: "parent request digest",
		},
		{
			name: "aggregate result digest",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.RecognitionResults[0].ResultDigest = repeatHex("e", 64)
			},
			want: "aggregate result digest",
		},
		{
			name: "typed item mutation",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.RecognitionItems[0].StemRaw = "archive-only mutation"
			},
			want: "aggregate result digest",
		},
		{
			name: "physical result lineage",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ModelPhysicalInvocations[0].ResultDigest = repeatHex("f", 64)
			},
			want: "physical invocation lineage",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneProblemSourceArchiveV6ForTest(t, archive)
			test.mutate(&candidate)
			err := k12storage.ValidateProblemSourceArchiveV6("mingming", candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("tampered V73 archive accepted: err=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestProblemSourceArchiveV6RejectsSourceActionSemanticTampering(t *testing.T) {
	ctx := context.Background()
	store, db := setup(t)
	lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, db)
	commit, created, err := store.CommitProblemSourceRecognitionResult(
		ctx, lease, validProblemSourceRecognitionResult(),
	)
	if err != nil || !created {
		t.Fatalf("commit V73 source result: created=%v err=%v", created, err)
	}
	freezeProblemSourceArchiveReceipt(t, db, commit)
	archive, err := store.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if err := k12storage.ValidateProblemSourceArchiveV6("mingming", archive); err != nil {
		t.Fatalf("valid archive rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*k12storage.ProblemSourceArchiveV6)
		want   string
	}{
		{
			name: "duplicate dispatch owner",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.DispatchOwners = append(candidate.DispatchOwners, candidate.DispatchOwners[0])
			},
			want: "duplicate problem-source dispatch owner",
		},
		{
			name: "unreferenced dispatch owner",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				extra := candidate.DispatchOwners[0]
				extra.DispatchID = "unreferenced-dispatch"
				candidate.DispatchOwners = append(candidate.DispatchOwners, extra)
			},
			want: "unreferenced problem-source dispatch owner",
		},
		{
			name: "duplicate homework",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.HomeworkSubmissions = append(candidate.HomeworkSubmissions, candidate.HomeworkSubmissions[0])
			},
			want: "duplicate problem-source homework",
		},
		{
			name: "duplicate receipt",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ActionReceipts = append(candidate.ActionReceipts, candidate.ActionReceipts[0])
			},
			want: "duplicate problem-source receipt",
		},
		{
			name: "duplicate work",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ReprocessJobs = append(candidate.ReprocessJobs, candidate.ReprocessJobs[0])
			},
			want: "duplicate source work",
		},
		{
			name: "duplicate recognition result",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.RecognitionResults = append(candidate.RecognitionResults, candidate.RecognitionResults[0])
			},
			want: "duplicate source recognition aggregate",
		},
		{
			name: "structure digest",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.StructureSnapshots[0].StructureDigest = repeatHex("0", 64)
			},
			want: "problem-source structure digest mismatch",
		},
		{
			name: "duplicate structure member",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.StructureMembers = append(candidate.StructureMembers, candidate.StructureMembers[0])
			},
			want: "duplicate problem-source structure member",
		},
		{
			name: "duplicate dependency group",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.DependencyGroups = append(candidate.DependencyGroups, candidate.DependencyGroups[0])
			},
			want: "duplicate problem-source dependency group",
		},
		{
			name: "missing dependency group",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.DependencyGroups = nil
			},
			want: "structure member dependency group missing",
		},
		{
			name: "duplicate input revision",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.InputRevisions = append(candidate.InputRevisions, candidate.InputRevisions[0])
			},
			want: "duplicate problem input revision",
		},
		{
			name: "receipt request identity",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ActionReceipts[0].RequestJSON = json.RawMessage(`{"action":"resume","structure_version":1,"expected_input_revision":1,"payload":{}}`)
			},
			want: "problem-source receipt request identity mismatch",
		},
		{
			name: "receipt request payload digest",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ActionReceipts[0].RequestJSON = json.RawMessage(
					`{"action":"select_region","structure_version":1,"expected_input_revision":1,"payload":{"page_asset_id":"` +
						candidate.PageAssets[0].PageAssetID +
						`","region":{"x":11,"y":20,"width":100,"height":80}}}`,
				)
			},
			want: "problem-source receipt request digest mismatch",
		},
		{
			name: "receipt request digest",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ActionReceipts[0].RequestDigest = repeatHex("0", 64)
			},
			want: "problem-source receipt request digest mismatch",
		},
		{
			name: "receipt affected order",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ActionReceipts[0].AffectedProblemIDsJSON = json.RawMessage(
					`["` + recognitionChildTwo + `","` + recognitionChildOne + `"]`,
				)
			},
			want: "problem-source receipt affected exact-set mismatch",
		},
		{
			name: "work request identity",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ReprocessJobs[0].RequestJSON = json.RawMessage(`{"action":"resume","structure_version":1,"expected_input_revision":1,"payload":{}}`)
			},
			want: "source work request identity mismatch",
		},
		{
			name: "work affected order",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ReprocessJobs[0].AffectedProblemIDs = []string{
					recognitionChildTwo, recognitionChildOne,
				}
			},
			want: "source work affected exact-set mismatch",
		},
		{
			name: "work input digest",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.ReprocessJobs[0].InputDigest = "sha256:" + repeatHex("0", 64)
			},
			want: "source work input digest mismatch",
		},
		{
			name: "recognition structure digest",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.RecognitionResults[0].StructureDigest = repeatHex("1", 64)
			},
			want: "source recognition structure digest mismatch",
		},
		{
			name: "recognition affected order",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.RecognitionResults[0].AffectedProblemIDsJSON = json.RawMessage(
					`["` + recognitionChildTwo + `","` + recognitionChildOne + `"]`,
				)
			},
			want: "source recognition affected exact-set mismatch",
		},
		{
			name: "duplicate recognition item",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.RecognitionItems = append(candidate.RecognitionItems, candidate.RecognitionItems[0])
			},
			want: "duplicate source recognition item",
		},
		{
			name: "recognition result input revision",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				candidate.RecognitionItems[0].ResultInputRevision++
			},
			want: "source recognition item revision mismatch",
		},
		{
			name: "current input head",
			mutate: func(candidate *k12storage.ProblemSourceArchiveV6) {
				for index := range candidate.InputRevisions {
					input := &candidate.InputRevisions[index]
					if input.ProblemID == recognitionChildOne && input.CurrentDisposition == "current" {
						input.CurrentDisposition = "superseded"
					}
				}
			},
			want: "problem-source structure current input head mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneProblemSourceArchiveV6ForTest(t, archive)
			test.mutate(&candidate)
			err := k12storage.ValidateProblemSourceArchiveV6("mingming", candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("tampered source archive accepted: err=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestProblemSourceArchiveV6RoundTripPreservesTypedSucceededSummaryPayload(t *testing.T) {
	ctx := context.Background()
	sourceStore, sourceDB := setup(t)
	lease := seedProblemSourceRecognitionFixture(t, sourceStore, sourceDB, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, sourceDB)
	commit, created, err := sourceStore.CommitProblemSourceRecognitionResult(
		ctx, lease, validProblemSourceRecognitionResult(),
	)
	if err != nil || !created {
		t.Fatalf("commit V73 source result: created=%v err=%v", created, err)
	}
	freezeProblemSourceArchiveReceipt(t, sourceDB, commit)
	if _, err := sourceDB.Exec(`UPDATE k12_problem_source_reprocess_jobs
		SET status='succeeded',lease_owner='',lease_expires_at=0,updated_at=101
		WHERE work_id=?`, recognitionWork); err != nil {
		t.Fatal(err)
	}

	const summaryInvocationID = "summary-result-before-artifact"
	resultJSON := problemSourceArchiveSummaryResultJSON(
		recognitionJob,
		recognitionSubmission,
	)
	resultDigest := modelResultPayloadDigest(resultJSON)
	if _, err := sourceDB.Exec(`INSERT INTO k12_model_invocations (
		invocation_id,agent_name,job_id,stage,request_digest,provider,model,
		route_snapshot_json,request_policy_snapshot_json,provider_idempotency_key,
		status,attempt,result_digest,result_json,external_request_id,failure_kind,
		created_at,updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		summaryInvocationID, "mingming", recognitionJob, k12.GradingStageProjecting,
		"sha256:summary-request-v6", "openai", "gpt-5.6-sol",
		`{"provider":"openai","model":"gpt-5.6-sol","route":"cloud"}`, `{}`,
		"summary-provider-key", k12.ModelInvocationSucceeded, 1,
		resultDigest, resultJSON, "summary-provider-request", "", 101, 101,
	); err != nil {
		t.Fatal(err)
	}

	archive, err := sourceStore.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	summary, found := problemSourceArchiveModelInvocation(
		archive.ModelInvocations,
		summaryInvocationID,
	)
	if !found {
		t.Fatal("v6 archive omitted succeeded summary payload before artifact commit")
	}
	if summary.ResultJSON != resultJSON || summary.ResultDigest != resultDigest ||
		summary.ProviderIdempotencyKey != "" || summary.ExternalRequestID != "" {
		t.Fatalf("archived typed summary drifted or leaked provider ids: %+v", summary)
	}

	for _, test := range []struct {
		name   string
		mutate func(*k12.ModelInvocation)
		want   string
	}{
		{
			name: "digest mismatch",
			mutate: func(invocation *k12.ModelInvocation) {
				invocation.ResultDigest = "sha256:" + repeatHex("a", 64)
			},
			want: "result payload digest",
		},
		{
			name: "raw provider shape",
			mutate: func(invocation *k12.ModelInvocation) {
				invocation.ResultJSON = `{"choices":[{"message":{"content":"raw"}}]}`
				invocation.ResultDigest = modelResultPayloadDigest(invocation.ResultJSON)
			},
			want: "typed summary payload",
		},
		{
			name: "wrong durable submission identity",
			mutate: func(invocation *k12.ModelInvocation) {
				invocation.ResultJSON = strings.Replace(
					invocation.ResultJSON,
					recognitionSubmission,
					"other-submission",
					1,
				)
				invocation.ResultDigest = modelResultPayloadDigest(invocation.ResultJSON)
			},
			want: "typed summary payload",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneProblemSourceArchiveV6ForTest(t, archive)
			for index := range candidate.ModelInvocations {
				if candidate.ModelInvocations[index].InvocationID == summaryInvocationID {
					test.mutate(&candidate.ModelInvocations[index])
				}
			}
			err := k12storage.ValidateProblemSourceArchiveV6("mingming", candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe summary payload accepted: err=%v want substring %q", err, test.want)
			}
		})
	}

	targetStore, targetDB := setup(t)
	seedProblemSourceArchiveTargetParents(t, targetDB)
	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := targetStore.ImportProblemSourceArchiveV6Tx(
		ctx, tx, "mingming", archive,
	); err != nil {
		t.Fatalf("restore typed summary payload: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	restarted := k12storage.NewStore(targetDB, nil)
	restored, err := restarted.GetModelInvocation(
		ctx, "mingming", summaryInvocationID,
	)
	if err != nil || restored.Status != k12.ModelInvocationSucceeded ||
		restored.ResultJSON != resultJSON || restored.ResultDigest != resultDigest {
		t.Fatalf("restart lost safe summary recovery payload: restored=%+v err=%v", restored, err)
	}
}

func problemSourceArchiveSummaryResultJSON(jobID, submissionID string) string {
	return `{"GradingJobID":"` + jobID + `","SubmissionID":"` + submissionID +
		`","Grade":"五年级下","Subject":"数学","knowledge_points":["整数除法"],` +
		`"sections":[` +
		`{"title":"这页在练什么","content":"练习整数除法。","source_label":"🤖 AI 归纳·供参考"},` +
		`{"title":"小明要留意","content":"留意计算步骤。","source_label":"🧠 学情信号"},` +
		`{"title":"每道题怎么带（不直接给答案）","content":"先说清题意。","source_label":"🤖 AI 归纳·供参考"}` +
		`]}`
}

func problemSourceArchiveModelInvocation(
	invocations []k12.ModelInvocation,
	invocationID string,
) (k12.ModelInvocation, bool) {
	for _, invocation := range invocations {
		if invocation.InvocationID == invocationID {
			return invocation, true
		}
	}
	return k12.ModelInvocation{}, false
}

func cloneProblemSourceArchiveV6ForTest(
	t *testing.T,
	source k12storage.ProblemSourceArchiveV6,
) k12storage.ProblemSourceArchiveV6 {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var out k12storage.ProblemSourceArchiveV6
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func seedProblemSourceArchiveExtraReferencedAsset(t *testing.T, db *sql.DB) {
	t.Helper()
	extraAssetID := "asset://mingming/" + repeatHex("e", 64) + ".png"
	if _, err := db.Exec(`INSERT INTO k12_page_assets (
		owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,
		pixel_width,pixel_height,orientation_policy,orientation_policy_version,
		transform_chain_json,storage_state,ready_at,last_error,created_at,updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		recognitionOwner,
		extraAssetID,
		"mingming",
		repeatHex("e", 64),
		"image/png",
		2048,
		100,
		80,
		"verified",
		"exif-v1",
		`[{"operation":"exif_normalize","orientation":1}]`,
		"ready",
		100,
		"",
		100,
		100,
	); err != nil {
		t.Fatalf("seed dispatch-only PageAsset: %v", err)
	}
	refs := `["asset://mingming/` + repeatHex("b", 64) + `.png","` + extraAssetID + `"]`
	if _, err := db.Exec(`UPDATE k12_homework_submissions
		SET source_asset_refs_json=? WHERE submission_id=?`, refs, recognitionSubmission); err != nil {
		t.Fatalf("attach homework-only PageAsset: %v", err)
	}
}

func TestProblemSourceArchiveV6RestoreAsRewritesTerminalV73DigestLineage(t *testing.T) {
	ctx := context.Background()
	store, db := setup(t)
	lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, db)
	commit, created, err := store.CommitProblemSourceRecognitionResult(
		ctx, lease, validProblemSourceRecognitionResult(),
	)
	if err != nil || !created {
		t.Fatalf("commit V73 source result: created=%v err=%v", created, err)
	}
	freezeProblemSourceArchiveReceipt(t, db, commit)
	if _, err := db.Exec(`UPDATE k12_problem_source_reprocess_jobs
		SET status='succeeded',lease_owner='',lease_expires_at=0,updated_at=101
		WHERE work_id=?`, recognitionWork); err != nil {
		t.Fatal(err)
	}
	source, err := store.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	sourceResultDigest := source.RecognitionResults[0].ResultDigest
	sourceParentDigest := source.RecognitionResults[0].ParentRequestDigest
	sourceParentTypedDigest := source.ModelInvocations[0].ResultDigest
	sourceWorkID := source.ReprocessJobs[0].WorkID
	sourceReceiptID := source.ActionReceipts[0].CommandReceiptID
	sourceAssetID := source.PageAssets[0].PageAssetID
	const sourceSummaryID = "summary-invocation-before-restore-as"
	canonicalTargetSummaryJSON := problemSourceArchiveSummaryResultJSON(
		recognitionJob,
		recognitionSubmission,
	)
	// Same-owner restore preserves these exact bytes. Restore-as must instead
	// typed-decode against the stable target GradingJob/Submission identity and
	// canonicalize, even when the stable IDs currently have equal string values.
	sourceSummaryJSON := "\n  " + canonicalTargetSummaryJSON + "\n"
	source.ModelInvocations = append(source.ModelInvocations, k12.ModelInvocation{
		InvocationID: sourceSummaryID, AgentName: "mingming", JobID: recognitionJob,
		Stage: k12.GradingStageProjecting, RequestDigest: "sha256:summary-restore-as-request",
		RouteSnapshot: k12.GradingModelSnapshot{
			Provider: "openai", Model: "gpt-5.6-sol", Route: "cloud",
		},
		Status: k12.ModelInvocationSucceeded, Attempt: 1,
		ResultDigest: modelResultPayloadDigest(sourceSummaryJSON),
		ResultJSON:   sourceSummaryJSON,
		CreatedAt:    100,
		UpdatedAt:    100,
	})
	sourceArtifact := k12.GradingFinalArtifact{
		ArtifactID: "grading-final-source", AgentName: "mingming",
		JobID: recognitionJob, StructureVersion: 1,
		CoverageStatus: k12.GradingFinalArtifactCoverageComplete,
		TotalCount:     1, PublishedCount: 1, SkippedCount: 0,
		OrderedCurrentDigestsJSON: `["assessment-source"]`,
		CanonicalMarkdown:         "# canonical source final",
		SummaryInvocationID:       sourceSummaryID,
		CreatedAt:                 100,
		UpdatedAt:                 100,
	}
	sourceArtifact.ArtifactDigest = problemSourceArchiveTestFinalArtifactDigest(sourceArtifact)
	source.FinalizationGenerations[0].Artifact = &sourceArtifact
	request := json.RawMessage(`{"action":"select_region","structure_version":1,"expected_input_revision":1,"payload":{"page_asset_id":"` + sourceAssetID + `","region":{"x":10,"y":20,"width":100,"height":80}}}`)
	source.ActionReceipts[0].RequestJSON = append(json.RawMessage(nil), request...)
	source.ReprocessJobs[0].RequestJSON = append(json.RawMessage(nil), request...)
	work := source.ReprocessJobs[0]
	updatedParentRequestDigest, err := k12storage.ProblemSourceRecognitionParentRequestDigest(
		k12storage.ProblemSourceReprocessJob{
			WorkID: work.WorkID, CommandReceiptID: work.CommandReceiptID,
			OwnerScope: work.OwnerScope, AgentName: work.AgentName,
			DispatchID: work.DispatchID, JobID: work.JobID, ProblemID: work.ProblemID,
			Action: work.Action, StructureVersion: work.StructureVersion,
			InputRevision: work.InputRevision, InputDigest: work.InputDigest,
			AffectedProblemIDs: append([]string(nil), work.AffectedProblemIDs...),
			RequestJSON:        append(json.RawMessage(nil), work.RequestJSON...),
		},
		source.ModelInvocations[0].RouteSnapshot,
		source.ModelInvocations[0].RequestPolicySnapshot,
	)
	if err != nil {
		t.Fatal(err)
	}
	source.ModelInvocations[0].RequestDigest = updatedParentRequestDigest
	source.RecognitionResults[0].ParentRequestDigest = updatedParentRequestDigest
	targetAssetID := "asset://target-tutor/" + repeatHex("b", 64) + ".png"

	migrated, err := k12storage.MigrateProblemSourceArchiveV6Owner(
		"mingming", "target-tutor", source,
		map[string]string{sourceAssetID: targetAssetID},
	)
	if err != nil {
		t.Fatalf("migrate terminal V73 archive: %v", err)
	}
	if err := k12storage.ValidateProblemSourceArchiveV6("target-tutor", migrated); err != nil {
		t.Fatalf("migrated V73 closure invalid: %v", err)
	}
	if migrated.ReprocessJobs[0].WorkID == sourceWorkID ||
		migrated.ActionReceipts[0].CommandReceiptID == sourceReceiptID ||
		migrated.RecognitionResults[0].ResultDigest == sourceResultDigest ||
		migrated.RecognitionResults[0].ParentRequestDigest == sourceParentDigest {
		t.Fatalf("global/digest lineage was not rewritten: %+v", migrated)
	}
	if migrated.ModelInvocations[0].RequestDigest !=
		migrated.RecognitionResults[0].ParentRequestDigest {
		t.Fatalf("parent request digest split: parent=%s result=%s",
			migrated.ModelInvocations[0].RequestDigest,
			migrated.RecognitionResults[0].ParentRequestDigest)
	}
	typedDigest, err := k12storage.ProblemSourceRecognitionTypedResultDigest(
		validProblemSourceRecognitionResult(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.ModelInvocations[0].ResultDigest != sourceParentTypedDigest ||
		migrated.ModelInvocations[0].ResultDigest != typedDigest {
		t.Fatalf("owner-only ID rewrite changed V73 typed result binding: source=%s migrated=%s recomputed=%s",
			sourceParentTypedDigest,
			migrated.ModelInvocations[0].ResultDigest,
			typedDigest,
		)
	}
	migratedArtifact := migrated.FinalizationGenerations[0].Artifact
	if migratedArtifact == nil || migratedArtifact.AgentName != "target-tutor" ||
		migratedArtifact.ArtifactID == sourceArtifact.ArtifactID ||
		migratedArtifact.SummaryInvocationID == sourceSummaryID ||
		migratedArtifact.ArtifactDigest == sourceArtifact.ArtifactDigest {
		t.Fatalf("restore-as final artifact lineage was not rewritten: %+v", migratedArtifact)
	}
	for _, item := range migrated.RecognitionItems {
		if item.InputDigest == "" || item.PageAssetID != targetAssetID {
			t.Fatalf("migrated V73 typed fact drifted: %+v", item)
		}
	}
	for _, invocation := range migrated.ModelInvocations {
		wantStatus := k12.ModelInvocationReconciled
		if invocation.Stage == k12.GradingStageProjecting {
			wantStatus = k12.ModelInvocationSucceeded
			if invocation.ResultJSON == sourceSummaryJSON ||
				invocation.ResultJSON != canonicalTargetSummaryJSON ||
				invocation.ResultDigest != modelResultPayloadDigest(canonicalTargetSummaryJSON) {
				t.Fatalf("typed summary payload drifted during restore-as: %+v", invocation)
			}
		}
		if invocation.Status != wantStatus || invocation.ExternalRequestID != "" ||
			invocation.ProviderIdempotencyKey != "" {
			t.Fatalf("provider control state crossed restore-as: %+v", invocation)
		}
	}
	targetStore, targetDB := setup(t)
	seedProblemSourceArchiveTargetParentsForAgent(
		t, targetDB, "target-tutor", targetAssetID,
	)
	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := targetStore.ImportProblemSourceArchiveV6Tx(
		ctx, tx, "target-tutor", migrated,
	); err != nil {
		t.Fatalf("import migrated terminal V73 archive: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	facts, err := targetStore.ListCurrentProblemSourceRecognitionFacts(
		ctx, "target-tutor", recognitionSubmission,
	)
	if err != nil || len(facts) != 2 {
		t.Fatalf("migrated V73 typed facts are not restart-readable: facts=%+v err=%v", facts, err)
	}
	if facts[recognitionChildOne].AnswerCanonicalMarkdown !=
		commit.Items[0].AnswerCanonicalMarkdown {
		t.Fatalf("migrated target answer fact drifted: %+v", facts[recognitionChildOne])
	}
	restoredArtifact, err := targetStore.GetGradingFinalArtifactByJob(
		ctx, "target-tutor", recognitionJob,
	)
	if err != nil || migratedArtifact == nil ||
		!reflect.DeepEqual(restoredArtifact, *migratedArtifact) {
		t.Fatalf("migrated final artifact is not restart-readable: got=%+v want=%+v err=%v", restoredArtifact, migratedArtifact, err)
	}
	restoredSummary, err := targetStore.GetModelInvocation(
		ctx,
		"target-tutor",
		migratedArtifact.SummaryInvocationID,
	)
	if err != nil || restoredSummary.Status != k12.ModelInvocationSucceeded ||
		restoredSummary.ResultJSON != canonicalTargetSummaryJSON ||
		restoredSummary.ResultDigest != modelResultPayloadDigest(canonicalTargetSummaryJSON) {
		t.Fatalf("migrated typed summary is not restart-readable: invocation=%+v err=%v", restoredSummary, err)
	}
}

func problemSourceArchiveTestFinalArtifactDigest(
	artifact k12.GradingFinalArtifact,
) string {
	raw, _ := json.Marshal(struct {
		StructureVersion          int
		CoverageStatus            k12.GradingFinalArtifactCoverageStatus
		TotalCount                int
		PublishedCount            int
		SkippedCount              int
		OrderedCurrentDigestsJSON string
		CanonicalMarkdown         string
		SummaryInvocationID       string
	}{
		artifact.StructureVersion,
		artifact.CoverageStatus,
		artifact.TotalCount,
		artifact.PublishedCount,
		artifact.SkippedCount,
		artifact.OrderedCurrentDigestsJSON,
		artifact.CanonicalMarkdown,
		artifact.SummaryInvocationID,
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestProblemSourceArchiveV6OutcomeUnknownPreservesReconciliationAuditAndNeverBecomesRetryable(t *testing.T) {
	ctx := context.Background()
	sourceStore, sourceDB := setup(t)
	lease := seedProblemSourceRecognitionFixture(t, sourceStore, sourceDB, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, sourceDB)
	commit, created, err := sourceStore.CommitProblemSourceRecognitionResult(
		ctx, lease, validProblemSourceRecognitionResult(),
	)
	if err != nil || !created {
		t.Fatalf("commit V73 source result: created=%v err=%v", created, err)
	}
	freezeProblemSourceArchiveReceipt(t, sourceDB, commit)
	if _, err := sourceDB.Exec(`UPDATE k12_problem_source_reprocess_jobs SET
		status='outcome_unknown',lease_owner='',lease_expires_at=0,next_attempt_at=0,
		reconciliation_owner='reconciler-v6',reconciliation_epoch=5,
		reconciliation_expires_at=900,reconciliation_attempt_count=3,
		next_reconcile_at=0,failure_code='provider_outcome_unknown',
		failure_detail='bounded evidence',updated_at=101 WHERE work_id=?`,
		recognitionWork,
	); err != nil {
		t.Fatal(err)
	}
	leased, err := sourceStore.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	leasedWork := leased.ReprocessJobs[0]
	if leasedWork.ReconciliationOwner != "reconciler-v6" ||
		leasedWork.ReconciliationEpoch != 5 ||
		leasedWork.ReconciliationExpiresAtMilli != 900 ||
		leasedWork.ReconciliationAttemptCount != 3 ||
		leasedWork.NextReconcileAtMilli != 0 {
		t.Fatalf("active reconciliation lease was not archived: %+v", leasedWork)
	}
	if _, err := k12storage.MigrateProblemSourceArchiveV6Owner(
		"mingming", "target-tutor", leased, map[string]string{},
	); !errors.Is(err, k12storage.ErrProblemSourceArchiveLiveWork) {
		t.Fatalf("restore-as accepted outcome_unknown work: %v", err)
	}
	if _, err := sourceDB.Exec(`UPDATE k12_problem_source_reprocess_jobs SET
		reconciliation_owner='',reconciliation_expires_at=0,next_reconcile_at=888,
		updated_at=102 WHERE work_id=?`, recognitionWork); err != nil {
		t.Fatal(err)
	}
	scheduled, err := sourceStore.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}

	targetStore, targetDB := setup(t)
	seedProblemSourceArchiveTargetParents(t, targetDB)
	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := targetStore.ImportProblemSourceArchiveV6Tx(
		ctx, tx, "mingming", scheduled,
	); err != nil {
		t.Fatalf("import outcome_unknown source closure: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	restored, err := targetStore.GetProblemSourceReprocessJob(
		ctx, recognitionOwner, recognitionWork,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != k12storage.ProblemSourceReprocessOutcomeUnknown ||
		restored.LeaseOwner != "" || restored.NextAttemptAtMilli != 0 ||
		restored.ReconciliationOwner != "" ||
		restored.ReconciliationExpiresAtMilli != 0 ||
		restored.ReconciliationEpoch != 5 ||
		restored.ReconciliationAttemptCount != 3 ||
		restored.NextReconcileAtMilli != 888 {
		t.Fatalf("restored outcome_unknown audit drifted: %+v", restored)
	}
	retryable, err := targetStore.ListRecoverableProblemSourceReprocessJobs(
		ctx, time.UnixMilli(1_000), 32,
	)
	if err != nil || len(retryable) != 0 {
		t.Fatalf("outcome_unknown entered ordinary retry queue: jobs=%+v err=%v", retryable, err)
	}
	due, err := targetStore.ListProblemSourceReprocessOutcomeUnknownDue(
		ctx, time.UnixMilli(1_000), 32,
	)
	if err != nil || len(due) != 1 || due[0].WorkID != recognitionWork {
		t.Fatalf("restored outcome_unknown is not reconciliation-recoverable: jobs=%+v err=%v", due, err)
	}
}

func TestProblemSourceArchiveV6RejectsFinalArtifactGenerationDrift(t *testing.T) {
	ctx := context.Background()
	store, db := setup(t)
	_ = seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, db)
	if _, _, err := store.CommitGradingFinalArtifact(
		ctx,
		k12.GradingFinalArtifact{
			AgentName: "mingming", JobID: recognitionJob,
			StructureVersion: 1,
			CoverageStatus:   k12.GradingFinalArtifactCoverageWithSkips,
			TotalCount:       1, PublishedCount: 0, SkippedCount: 1,
			OrderedCurrentDigestsJSON: `["skip-before-drift"]`,
			CanonicalMarkdown:         "# frozen before drift",
			ArtifactDigest:            repeatHex("a", 64),
		},
		0,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_grading_jobs
		SET finalization_generation=1 WHERE agent_name=? AND record_id=?`,
		"mingming", recognitionJob); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExportProblemSourceArchiveV6(ctx, "mingming"); err == nil || !strings.Contains(err.Error(), "generation mismatch") {
		t.Fatalf("archive accepted job/artifact generation drift: %v", err)
	}
}

func seedProblemSourceArchiveHomeworkParent(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO k12_homework_submissions (
		submission_id,dispatch_id,agent_name,learner_id,source_kind,source_ref,
		source_asset_refs_json,task_intent,status,grading_job_id,idempotency_key,
		version,created_at,updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		recognitionSubmission,
		recognitionDispatch,
		"mingming",
		"recognition-learner",
		"desktop",
		"recognition-message",
		`["asset://mingming/`+repeatHex("b", 64)+`.png"]`,
		"completed_homework",
		"processing",
		recognitionJob,
		"recognition-submission-key",
		1,
		100,
		100,
	); err != nil {
		t.Fatalf("seed homework parent: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO k12_image_task_owner_scopes
		(dispatch_id,owner_scope,agent_name,created_at) VALUES(?,?,?,?)`,
		recognitionDispatch,
		recognitionOwner,
		"mingming",
		100,
	); err != nil {
		t.Fatalf("seed dispatch owner: %v", err)
	}
}

func seedProblemSourceArchiveTargetParents(t *testing.T, db *sql.DB) {
	t.Helper()
	seedProblemSourceArchiveTargetParentsForAgent(
		t, db, "mingming", "asset://mingming/"+repeatHex("b", 64)+".png",
	)
}

func seedProblemSourceArchiveTargetParentsForAgent(
	t *testing.T,
	db *sql.DB,
	agentName string,
	assetID string,
) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO agents(name) VALUES(?)`, agentName); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_grading_jobs (
		record_id,agent_name,status,submission_id,source_kind,idempotency_key,
		dedupe_key,created_at,updated_at
	) VALUES (?,?,?,?,?,?,?,?,?)`,
		recognitionJob,
		agentName,
		"active",
		recognitionSubmission,
		"desktop",
		"recognition-job-key",
		"recognition-job-dedupe",
		100,
		100,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_problems (
		problem_id,agent_name,submission_id,page_asset_id,ordinal,problem_kind,
		parent_problem_id,subproblem_no,subject,stem_raw,stem_markdown,concept_ids_json,
		transcription_confidence,confirmation_required,confirmation_reasons_json,
		canonical_version,created_at,updated_at
	) VALUES
		(?,?,?,?,?,'compound_parent',NULL,'','数学','旧公共题干','旧公共规范题干','[]',0.7,1,'["old"]',2,100,100),
		(?,?,?,?,?,'subproblem',?,'1','数学','旧题干一','旧规范题干一','["old-kp-1"]',0.7,1,'["old"]',2,100,100),
		(?,?,?,?,?,'subproblem',?,'2','数学','旧题干二','旧规范题干二','["old-kp-2"]',0.7,1,'["old"]',2,100,100)`,
		recognitionParent, agentName, recognitionSubmission, assetID, 0,
		recognitionChildOne, agentName, recognitionSubmission, assetID, 1, recognitionParent,
		recognitionChildTwo, agentName, recognitionSubmission, assetID, 2, recognitionParent,
	); err != nil {
		t.Fatal(err)
	}
}

func freezeProblemSourceArchiveReceipt(
	t *testing.T,
	db *sql.DB,
	commit k12storage.ProblemSourceRecognitionCommit,
) {
	t.Helper()
	response, err := viewcontract.FreezeProblemSourceActionResponse(
		viewcontract.ProblemSourceActionResponse{
			CommandReceiptID: commit.CommandReceiptID,
			DispatchID:       commit.DispatchID,
			ProblemID:        commit.PathProblemID,
			Action:           commit.Action,
			StructureVersion: commit.StructureVersion,
			InputRevision:    commit.SourceInputRevision,
			ProgressiveSnapshot: viewcontract.ProblemSourceProgressiveSnapshot{
				StructureVersion: commit.StructureVersion,
				SnapshotRevision: commit.ResultInputRevision,
				ProblemProgress: []viewcontract.ProblemSourceProgress{
					{ProblemID: recognitionParent, Status: "processing", InputRevision: 1, CurrentDisposition: "current"},
					{ProblemID: recognitionChildOne, Status: "processing", InputRevision: 3, CurrentDisposition: "current"},
					{ProblemID: recognitionChildTwo, Status: "processing", InputRevision: 3, CurrentDisposition: "current"},
				},
				Coverage: viewcontract.ProblemSourceProgressiveCoverage{
					Total: 3, Awaiting: 3, Status: "in_progress", ProjectionRevision: 3,
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(response.JSON) {
		t.Fatal("frozen response is not JSON")
	}
	if _, err := db.Exec(`UPDATE k12_problem_source_action_receipts
		SET response_json=? WHERE command_receipt_id=?`, response.JSON, recognitionReceipt); err != nil {
		t.Fatal(err)
	}
}

func repeatHex(value string, count int) string {
	out := ""
	for len(out) < count {
		out += value
	}
	return out[:count]
}
