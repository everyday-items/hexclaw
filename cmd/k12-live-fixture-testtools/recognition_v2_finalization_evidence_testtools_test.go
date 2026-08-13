//go:build testtools

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type decodedRecognitionV2EvidenceBatch struct {
	Ordinal                 int    `json:"ordinal"`
	PhysicalUnit            string `json:"physical_unit"`
	CandidateCount          int    `json:"candidate_count"`
	CandidateExactSetSHA256 string `json:"candidate_exact_set_sha256"`
}

type decodedRecognitionV2EvidenceRepair struct {
	CandidateOrdinal        int    `json:"candidate_ordinal"`
	CandidateRefSHA256      string `json:"candidate_ref_sha256"`
	PhysicalUnit            string `json:"physical_unit"`
	RepairRound             int    `json:"repair_round"`
	AuthorizationSHA256     string `json:"authorization_sha256"`
	SettlementSHA256        string `json:"settlement_sha256"`
	CandidateExactSetSHA256 string `json:"candidate_exact_set_sha256"`
}

type decodedRecognitionV2PhysicalReceipt struct {
	Ordinal                 int    `json:"ordinal"`
	InvocationSHA256        string `json:"invocation_sha256"`
	ParentInvocationSHA256  string `json:"parent_invocation_sha256"`
	PhysicalUnit            string `json:"physical_unit"`
	Operation               string `json:"operation"`
	Provider                string `json:"provider"`
	Model                   string `json:"model"`
	Status                  string `json:"status"`
	Attempt                 int    `json:"attempt"`
	ResultSHA256            string `json:"result_sha256"`
	RecognitionPlanVersion  int    `json:"recognition_plan_version"`
	PlanSHA256              string `json:"plan_sha256"`
	CandidateExactSetSHA256 string `json:"candidate_exact_set_sha256"`
	StageDeadlineAtMillis   int64  `json:"stage_deadline_at_unix_millis"`
}

type decodedRecognitionV2FinalizationEvidence struct {
	SchemaVersion                  int                                   `json:"schema_version"`
	EvidenceClass                  string                                `json:"evidence_class"`
	Complete                       bool                                  `json:"complete"`
	EligibleForPass                bool                                  `json:"eligible_for_pass"`
	ExternalBoundaryAttested       bool                                  `json:"external_boundary_attested"`
	ManifestSHA256                 string                                `json:"manifest_sha256"`
	ClaimSHA256                    string                                `json:"claim_sha256"`
	RunSHA256                      string                                `json:"run_sha256"`
	OwnershipSHA256                string                                `json:"ownership_sha256"`
	FixtureAgentSHA256             string                                `json:"fixture_agent_sha256"`
	TargetAgentSHA256              string                                `json:"target_agent_sha256"`
	DispatchSHA256                 string                                `json:"dispatch_sha256"`
	SourceSessionSHA256            string                                `json:"source_session_sha256"`
	SourceDigestSHA256             string                                `json:"source_digest_sha256"`
	SubmissionSHA256               string                                `json:"submission_sha256"`
	JobSHA256                      string                                `json:"job_sha256"`
	ParentInvocationSHA256         string                                `json:"parent_invocation_sha256"`
	PlanIDSHA256                   string                                `json:"plan_id_sha256"`
	RecognitionPlanVersion         int                                   `json:"recognition_plan_version"`
	Status                         string                                `json:"status"`
	ParentStatus                   string                                `json:"parent_status"`
	ParentAttempt                  int                                   `json:"parent_attempt"`
	Provider                       string                                `json:"provider"`
	Model                          string                                `json:"model"`
	HeaderSHA256                   string                                `json:"header_sha256"`
	AuthorizedPlanSHA256           string                                `json:"authorized_plan_sha256"`
	CandidateExactSetSHA256        string                                `json:"candidate_exact_set_sha256"`
	CandidateResultsExactSetSHA256 string                                `json:"candidate_results_exact_set_sha256"`
	PhysicalResultsExactSetSHA256  string                                `json:"physical_results_exact_set_sha256"`
	FinalizationSHA256             string                                `json:"finalization_sha256"`
	StageStartedAtUnixMillis       int64                                 `json:"stage_started_at_unix_millis"`
	StageDeadlineAtUnixMillis      int64                                 `json:"stage_deadline_at_unix_millis"`
	SelectedBucketMaxProblems      int                                   `json:"selected_bucket_max_problems"`
	BudgetBucketsMillis            k12.RecognitionLayoutBudgetBucketsV2  `json:"budget_buckets_millis"`
	PhysicalCallCapMillis          int64                                 `json:"physical_call_cap_millis"`
	AdapterWorkerHardCap           int                                   `json:"adapter_worker_hard_cap"`
	EffectiveConcurrency           int                                   `json:"effective_concurrency"`
	CandidateResultCount           int                                   `json:"candidate_result_count"`
	QuestionCount                  int                                   `json:"question_count"`
	NonQuestionCount               int                                   `json:"non_question_count"`
	PhysicalResultCount            int                                   `json:"physical_result_count"`
	AuthorizedBatches              []decodedRecognitionV2EvidenceBatch   `json:"authorized_batches"`
	AuthorizedRepairs              []decodedRecognitionV2EvidenceRepair  `json:"authorized_repairs"`
	PhysicalReceipts               []decodedRecognitionV2PhysicalReceipt `json:"physical_receipts"`
}

type recognitionV2EvidenceFixture struct {
	profile       string
	storePath     string
	manifestPath  string
	claimPath     string
	manifest      decodedManifest
	runID         string
	targetAgent   string
	dispatchID    string
	sessionID     string
	sourceDigest  string
	submissionID  string
	jobID         string
	parentID      string
	headerDigest  string
	stageStarted  int64
	stageDeadline int64
	buckets       k12.RecognitionLayoutBudgetBucketsV2
	plan          k12.RecognitionLayoutPlanV2
	finalization  k12.RecognitionLayoutPlanFinalizationResultV2
	physicalIDs   []string
	candidateIDs  []string
}

// K12-LIVE-RECOGNITION-PLAN-V2-EVIDENCE-20260809-001：私有导出器在不暴露任何
// 原始业务内容、Provider 载荷或文件系统身份的前提下，证明一条目标声明谱系和
// 一个已完成最终化的 V2 精确集合。
func TestK12LiveRecognitionPlanV2EvidenceExportsFinalizedClaimLineage(t *testing.T) {
	fixture := newRecognitionV2EvidenceFixture(t, true)
	beforeStore := fileSHA256(t, fixture.storePath)

	stdout, stderr, err := executeCLI([]string{
		"recognition-v2-finalization-evidence",
		"--profile", fixture.profile,
		"--store", fixture.storePath,
		"--manifest", fixture.manifestPath,
		"--claim", fixture.claimPath,
	})
	if err != nil {
		t.Fatalf("export finalized V2 evidence: %v\nstderr=%s", err, stderr)
	}
	if got := fileSHA256(t, fixture.storePath); got != beforeStore {
		t.Fatalf("query-only evidence export changed store: before=%s after=%s", beforeStore, got)
	}
	if _, err := os.Stat(filepath.Join(fixture.profile, ".hexclaw", ".sidecar.lock")); !os.IsNotExist(err) {
		t.Fatalf("profile lock survived evidence export: %v", err)
	}

	var evidence decodedRecognitionV2FinalizationEvidence
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		t.Fatalf("decode evidence: %v\n%s", err, stdout)
	}
	if evidence.SchemaVersion != 1 ||
		evidence.EvidenceClass != "recognition_v2_finalization" ||
		!evidence.Complete || !evidence.EligibleForPass ||
		evidence.ExternalBoundaryAttested ||
		evidence.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		evidence.Status != "succeeded" || evidence.Provider != "hexclaw-gpt" ||
		evidence.ParentStatus != string(k12.ModelInvocationSucceeded) ||
		evidence.ParentAttempt != 1 ||
		evidence.Model != k12.RecognizingPolicyModel {
		t.Fatalf("wrong evidence identity/status: %+v", evidence)
	}
	for name, digest := range map[string]string{
		"manifest":                    evidence.ManifestSHA256,
		"claim":                       evidence.ClaimSHA256,
		"run":                         evidence.RunSHA256,
		"ownership":                   evidence.OwnershipSHA256,
		"fixture_agent":               evidence.FixtureAgentSHA256,
		"target_agent":                evidence.TargetAgentSHA256,
		"dispatch":                    evidence.DispatchSHA256,
		"source_session":              evidence.SourceSessionSHA256,
		"source_digest":               evidence.SourceDigestSHA256,
		"submission":                  evidence.SubmissionSHA256,
		"job":                         evidence.JobSHA256,
		"parent":                      evidence.ParentInvocationSHA256,
		"plan_id":                     evidence.PlanIDSHA256,
		"header":                      evidence.HeaderSHA256,
		"authorized_plan":             evidence.AuthorizedPlanSHA256,
		"candidate_exact_set":         evidence.CandidateExactSetSHA256,
		"candidate_results_exact_set": evidence.CandidateResultsExactSetSHA256,
		"physical_results_exact_set":  evidence.PhysicalResultsExactSetSHA256,
		"finalization":                evidence.FinalizationSHA256,
	} {
		assertBareSHA256(t, name, digest)
	}
	if evidence.HeaderSHA256 == evidence.AuthorizedPlanSHA256 {
		t.Fatal("manifest header digest was collapsed into the authorized-plan digest")
	}
	if evidence.StageStartedAtUnixMillis != fixture.stageStarted ||
		evidence.StageDeadlineAtUnixMillis != fixture.stageDeadline ||
		evidence.SelectedBucketMaxProblems != 8 ||
		evidence.BudgetBucketsMillis != fixture.buckets ||
		evidence.PhysicalCallCapMillis != 120_000 ||
		evidence.AdapterWorkerHardCap != 2 || evidence.EffectiveConcurrency != 1 {
		t.Fatalf("wrong frozen budget projection: %+v", evidence)
	}
	if evidence.CandidateResultCount != 4 || evidence.QuestionCount != 3 ||
		evidence.NonQuestionCount != 1 ||
		evidence.CandidateResultCount != evidence.QuestionCount+evidence.NonQuestionCount ||
		evidence.PhysicalResultCount != 3 {
		t.Fatalf("wrong final exact-set counts: %+v", evidence)
	}
	if len(evidence.AuthorizedBatches) != 1 ||
		evidence.AuthorizedBatches[0].Ordinal != 1 ||
		evidence.AuthorizedBatches[0].PhysicalUnit != "layout_batch_0001" ||
		evidence.AuthorizedBatches[0].CandidateCount != 4 {
		t.Fatalf("wrong ordered batches: %+v", evidence.AuthorizedBatches)
	}
	assertBareSHA256(t, "batch candidate exact-set", evidence.AuthorizedBatches[0].CandidateExactSetSHA256)
	if len(evidence.AuthorizedRepairs) != 1 ||
		evidence.AuthorizedRepairs[0].PhysicalUnit != "layout_repair_0003" ||
		evidence.AuthorizedRepairs[0].CandidateOrdinal != 3 ||
		evidence.AuthorizedRepairs[0].RepairRound != 1 {
		t.Fatalf("wrong sparse repair evidence: %+v", evidence.AuthorizedRepairs)
	}
	assertBareSHA256(t, "repair candidate reference", evidence.AuthorizedRepairs[0].CandidateRefSHA256)
	assertBareSHA256(t, "repair authorization", evidence.AuthorizedRepairs[0].AuthorizationSHA256)
	assertBareSHA256(t, "repair settlement", evidence.AuthorizedRepairs[0].SettlementSHA256)
	assertBareSHA256(t, "repair candidate exact-set", evidence.AuthorizedRepairs[0].CandidateExactSetSHA256)

	wantUnits := []string{"whole_page", "layout_batch_0001", "layout_repair_0003"}
	if len(evidence.PhysicalReceipts) != len(wantUnits) {
		t.Fatalf("physical receipts=%d, want %d", len(evidence.PhysicalReceipts), len(wantUnits))
	}
	for index, receipt := range evidence.PhysicalReceipts {
		if receipt.Ordinal != index+1 || receipt.PhysicalUnit != wantUnits[index] ||
			receipt.ParentInvocationSHA256 != evidence.ParentInvocationSHA256 ||
			receipt.Operation != k12.GradingStageRecognizing ||
			receipt.Provider != evidence.Provider || receipt.Model != evidence.Model ||
			receipt.Status != string(k12.ModelInvocationSucceeded) || receipt.Attempt != 1 ||
			receipt.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
			receipt.StageDeadlineAtMillis != evidence.StageDeadlineAtUnixMillis {
			t.Fatalf("physical receipt %d drifted: %+v", index+1, receipt)
		}
		assertBareSHA256(t, "physical invocation", receipt.InvocationSHA256)
		assertBareSHA256(t, "physical result", receipt.ResultSHA256)
		assertBareSHA256(t, "physical plan", receipt.PlanSHA256)
		if index == 0 {
			if receipt.PlanSHA256 != evidence.HeaderSHA256 || receipt.CandidateExactSetSHA256 != "" {
				t.Fatalf("manifest receipt is not header-bound: %+v", receipt)
			}
		} else {
			if receipt.PlanSHA256 != evidence.AuthorizedPlanSHA256 {
				t.Fatalf("layout receipt is not authorized-plan-bound: %+v", receipt)
			}
			assertBareSHA256(t, "physical candidate exact-set", receipt.CandidateExactSetSHA256)
		}
	}

	for _, forbidden := range append([]string{
		fixture.profile,
		fixture.storePath,
		fixture.manifestPath,
		fixture.claimPath,
		fixture.manifest.AgentName,
		fixture.manifest.Ownership,
		fixture.runID,
		fixture.targetAgent,
		fixture.dispatchID,
		fixture.sessionID,
		fixture.submissionID,
		fixture.jobID,
		fixture.parentID,
		"private question text",
		"private non-question reason",
		"private repair text",
		"provider-private-payload",
	}, append(fixture.physicalIDs, fixture.candidateIDs...)...) {
		if forbidden != "" && strings.Contains(stdout+stderr, forbidden) {
			t.Fatalf("evidence output leaked raw private value %q", forbidden)
		}
	}
}

func TestK12LiveRecognitionPlanV2EvidenceFailsClosedOnClaimAndLedgerDrift(t *testing.T) {
	tests := []struct {
		name     string
		finalize bool
		mutate   func(*testing.T, recognitionV2EvidenceFixture)
	}{
		{
			name:     "manifest ownership drift",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				manifest := fixture.manifest
				manifest.Ownership = "different-ownership"
				raw, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixture.manifestPath, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "fixture run metadata drift",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`UPDATE agents
					 SET metadata=json_set(metadata,'$."hexclaw.test.fixture_run_id"',?)
					 WHERE name=?`,
					"different-run",
					fixture.manifest.AgentName,
				)
			},
		},
		{
			name:     "claim owner drift",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				writeRecognitionV2EvidenceClaim(
					t,
					fixture,
					"different-target-agent",
					fixture.sessionID,
					fixture.sourceDigest,
				)
			},
		},
		{
			name:     "claim session drift",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				writeRecognitionV2EvidenceClaim(
					t,
					fixture,
					fixture.targetAgent,
					"different-session",
					fixture.sourceDigest,
				)
			},
		},
		{
			name:     "claim source drift",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				writeRecognitionV2EvidenceClaim(
					t,
					fixture,
					fixture.targetAgent,
					fixture.sessionID,
					"sha256:"+strings.Repeat("b", 64),
				)
			},
		},
		{
			name:     "non finalized plan",
			finalize: false,
			mutate:   func(*testing.T, recognitionV2EvidenceFixture) {},
		},
		{
			name:     "zero recognizing parent",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`DELETE FROM k12_model_invocations WHERE invocation_id=?`,
					fixture.parentID,
				)
			},
		},
		{
			name:     "multiple recognizing parents",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`INSERT INTO k12_model_invocations (
						invocation_id,agent_name,job_id,stage,request_digest,provider,model,
						route_snapshot_json,request_policy_snapshot_json,provider_idempotency_key,
						status,attempt,result_digest,result_json,external_request_id,failure_kind,
						created_at,updated_at
					) SELECT ?,agent_name,job_id,stage,request_digest,provider,model,
						route_snapshot_json,request_policy_snapshot_json,provider_idempotency_key,
						status,2,result_digest,result_json,external_request_id,failure_kind,
						created_at,updated_at
					FROM k12_model_invocations WHERE invocation_id=?`,
					"second-parent-private",
					fixture.parentID,
				)
			},
		},
		{
			name:     "v1 physical child mixed into v2 parent",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`INSERT INTO k12_model_physical_invocations (
						physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
						physical_unit,request_digest,route_snapshot_json,request_policy_snapshot_json,
						status,attempt,result_digest,result_content,external_request_id,failure_kind,
						created_at,updated_at,recognition_plan_version,plan_digest,
						candidate_exact_set_digest
					) SELECT ?,parent_invocation_id,agent_name,job_id,stage,
						'segment_1','sha256:v1-private',route_snapshot_json,
						request_policy_snapshot_json,status,attempt,result_digest,result_content,
						external_request_id,failure_kind,created_at,updated_at,'v1','',''
					FROM k12_model_physical_invocations WHERE physical_invocation_id=?`,
					"v1-child-private",
					fixture.physicalIDs[0],
				)
			},
		},
		{
			name:     "header digest drift",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`DROP TRIGGER k12_recognition_layout_plan_identity_immutable`,
				)
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`UPDATE k12_recognition_layout_plans SET header_digest=? WHERE plan_id=?`,
					"sha256:"+strings.Repeat("b", 64),
					fixture.planID(),
				)
			},
		},
		{
			name:     "authorized plan digest drift",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`DROP TRIGGER k12_recognition_layout_plan_authorization_once`,
				)
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`UPDATE k12_recognition_layout_plans SET authorized_plan_digest=? WHERE plan_id=?`,
					"sha256:"+strings.Repeat("c", 64),
					fixture.planID(),
				)
			},
		},
		{
			name:     "physical plan digest drift",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`DROP TRIGGER k12_model_physical_invocation_identity_immutable`,
				)
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`UPDATE k12_model_physical_invocations SET plan_digest=?
					 WHERE physical_invocation_id=?`,
					fixture.headerDigest,
					fixture.physicalIDs[1],
				)
			},
		},
		{
			name:     "finalization digest drift",
			finalize: true,
			mutate: func(t *testing.T, fixture recognitionV2EvidenceFixture) {
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`DROP TRIGGER k12_recognition_layout_finalization_immutable`,
				)
				mutateRecognitionV2EvidenceStore(
					t,
					fixture.storePath,
					`UPDATE k12_recognition_layout_finalizations SET finalization_digest=?
					 WHERE plan_id=?`,
					"sha256:"+strings.Repeat("d", 64),
					fixture.planID(),
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRecognitionV2EvidenceFixture(t, test.finalize)
			test.mutate(t, fixture)
			assertRecognitionV2EvidenceFailsClosed(t, fixture)
		})
	}
}

func TestK12LiveRecognitionPlanV2EvidenceRejectsNonExactClaimSchema(t *testing.T) {
	fixture := newRecognitionV2EvidenceFixture(t, true)
	claim := []byte(`{"schema_version":1,"target_agent":"` + fixture.targetAgent +
		`","dispatch_id":"` + fixture.dispatchID + `","source_session_id":"` +
		fixture.sessionID + `","source_digest":"` + fixture.sourceDigest +
		`","target_agent":"` + fixture.targetAgent + `"}`)
	if err := os.WriteFile(fixture.claimPath, claim, 0o600); err != nil {
		t.Fatal(err)
	}
	assertRecognitionV2EvidenceFailsClosed(t, fixture)
}

func TestK12LiveRecognitionPlanV2EvidenceRejectsClaimSymlink(t *testing.T) {
	fixture := newRecognitionV2EvidenceFixture(t, true)
	if err := os.Remove(fixture.claimPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fixture.manifestPath, fixture.claimPath); err != nil {
		t.Fatal(err)
	}
	assertRecognitionV2EvidenceFailsClosed(t, fixture)
}

func TestK12LiveRecognitionPlanV2EvidenceRejectsDuplicateManifestFields(t *testing.T) {
	tests := []struct {
		name        string
		fieldPrefix string
		replacement string
	}{
		{
			name:        "ordinary duplicate",
			fieldPrefix: `"ownership":`,
			replacement: `"ownership":"shadowed","ownership":`,
		},
		{
			name:        "unicode-equivalent duplicate",
			fieldPrefix: `"agent_name":`,
			replacement: `"agent_name":"shadowed","\u0061gent_name":`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRecognitionV2EvidenceFixture(t, true)
			raw, err := os.ReadFile(fixture.manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			mutated := bytes.Replace(
				raw,
				[]byte(test.fieldPrefix),
				[]byte(test.replacement),
				1,
			)
			if bytes.Equal(mutated, raw) {
				t.Fatal("manifest field mutation did not apply")
			}
			if err := os.WriteFile(fixture.manifestPath, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			assertRecognitionV2EvidenceFailsClosed(t, fixture)
		})
	}
}

func TestK12LiveRecognitionPlanV2EvidenceReadsOneOpenedFDWhenClaimPathIsSwapped(t *testing.T) {
	fixture := newRecognitionV2EvidenceFixture(t, true)
	originalClaim, err := os.ReadFile(fixture.claimPath)
	if err != nil {
		t.Fatal(err)
	}
	maliciousPath := filepath.Join(fixture.profile, "replacement-claim.json")
	if err := os.WriteFile(maliciousPath, []byte(`{"schema_version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	openedPath := filepath.Join(fixture.profile, "opened-original-claim.json")
	previousHook := recognitionV2PrivateSnapshotOpenedHook
	recognitionV2PrivateSnapshotOpenedHook = func(path string) {
		if path != fixture.claimPath {
			return
		}
		if err := os.Rename(path, openedPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(maliciousPath, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { recognitionV2PrivateSnapshotOpenedHook = previousHook })

	stdout, stderr, err := executeCLI([]string{
		"recognition-v2-finalization-evidence",
		"--profile", fixture.profile,
		"--store", fixture.storePath,
		"--manifest", fixture.manifestPath,
		"--claim", fixture.claimPath,
	})
	if err != nil {
		t.Fatalf("opened-fd snapshot followed replaced claim path: %v\nstderr=%s", err, stderr)
	}
	var evidence decodedRecognitionV2FinalizationEvidence
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.ClaimSHA256 != recognitionV2BytesSHA256(originalClaim) {
		t.Fatalf("claim digest did not bind the opened fd snapshot: %s", evidence.ClaimSHA256)
	}
}

func (fixture recognitionV2EvidenceFixture) planID() string {
	return "layout-plan-private"
}

func writeRecognitionV2EvidenceClaim(
	t *testing.T,
	fixture recognitionV2EvidenceFixture,
	targetAgent string,
	sessionID string,
	sourceDigest string,
) {
	t.Helper()
	claim := []byte(`{"schema_version":1,"target_agent":"` + targetAgent +
		`","dispatch_id":"` + fixture.dispatchID + `","source_session_id":"` +
		sessionID + `","source_digest":"` + sourceDigest + `"}`)
	if err := os.WriteFile(fixture.claimPath, claim, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mutateRecognitionV2EvidenceStore(
	t *testing.T,
	storePath string,
	statement string,
	args ...any,
) {
	t.Helper()
	store, err := sqlitestore.New(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(context.Background(), statement, args...); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(storePath, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRecognitionV2EvidenceFailsClosed(
	t *testing.T,
	fixture recognitionV2EvidenceFixture,
) {
	t.Helper()
	stdout, stderr, err := executeCLI([]string{
		"recognition-v2-finalization-evidence",
		"--profile", fixture.profile,
		"--store", fixture.storePath,
		"--manifest", fixture.manifestPath,
		"--claim", fixture.claimPath,
	})
	if err == nil {
		t.Fatalf("drifted recognition evidence unexpectedly passed: %s", stdout)
	}
	if stdout != "" {
		t.Fatalf("failed evidence command emitted a receipt: %s", stdout)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.profile, ".hexclaw", ".sidecar.lock")); !os.IsNotExist(statErr) {
		t.Fatalf("profile lock survived failed evidence export: %v", statErr)
	}
	combined := stdout + stderr + err.Error()
	for _, forbidden := range []string{
		fixture.targetAgent,
		fixture.dispatchID,
		fixture.sessionID,
		fixture.submissionID,
		fixture.jobID,
		fixture.parentID,
		"private question text",
		"provider-private-payload",
	} {
		if forbidden != "" && strings.Contains(combined, forbidden) {
			t.Fatalf("failed evidence output leaked %q: %s", forbidden, combined)
		}
	}
}

func newRecognitionV2EvidenceFixture(
	t *testing.T,
	finalize bool,
) recognitionV2EvidenceFixture {
	t.Helper()
	profile, storePath, manifestPath := newIsolatedCLIStore(t)
	runID := "recognition-v2-evidence-" + strings.ReplaceAll(t.Name(), "/", "-")
	if _, stderr, err := executeCLI(startArguments(
		profile,
		storePath,
		manifestPath,
		runID,
		30*time.Minute,
	)); err != nil {
		t.Fatalf("start fixture: %v\nstderr=%s", err, stderr)
	}
	manifest, _ := readDecodedManifest(t, manifestPath)
	targetAgent := "target-agent-private"
	dispatchID := "dispatch-private"
	sessionID := "session-private"
	sourceDigest := "sha256:" + strings.Repeat("a", 64)
	submissionID := "submission-private"
	claimPath := filepath.Join(profile, "recognition-v2-target-claim.json")
	claim := []byte(`{"schema_version":1,"target_agent":"` + targetAgent +
		`","dispatch_id":"` + dispatchID + `","source_session_id":"` + sessionID +
		`","source_digest":"` + sourceDigest + `"}`)
	if err := os.WriteFile(claimPath, claim, 0o600); err != nil {
		t.Fatal(err)
	}

	sqliteStore, err := sqlitestore.New(storePath)
	if err != nil {
		t.Fatal(err)
	}
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	recordsStore := k12storage.NewStore(sqliteStore.DB(), registry.Records)
	ctx := context.Background()
	if _, err := sqliteStore.DB().ExecContext(
		ctx,
		`INSERT INTO agents(name,metadata) VALUES(?,?)`,
		targetAgent,
		`{"scenario":"k12-tutor"}`,
	); err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}

	policy := k12.ApprovedRecognizingRequestPolicy()
	route := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		RecognizingRequestPolicy: policy,
	}
	buckets := k12.RecognitionLayoutBudgetBucketsV2{
		UpTo1ProblemMillis:   60_000,
		UpTo8ProblemsMillis:  120_000,
		UpTo16ProblemsMillis: 180_000,
		UpTo32ProblemsMillis: 300_000,
	}
	budget := k12.GradingBudgetSnapshot{
		PolicyVersion: 1,
		StageSeconds: k12.GradingStageBudgets{
			Queued: 60, Normalizing: 60, Recognizing: 300,
			Locating: 60, Rendering: 60, Projecting: 60,
		},
		AssessingBuckets: []k12.GradingAssessingBudgetBucket{
			{MaxProblems: 1, Seconds: 60},
			{MaxProblems: 8, Seconds: 120},
			{MaxProblems: 16, Seconds: 180},
			{MaxProblems: 32, Seconds: 300},
		},
		ItemConcurrency:        1,
		RecognitionPlanVersion: k12.RecognitionPlanVersionV2,
		RecognizingBuckets:     buckets,
		PhysicalCallCapMillis:  120_000,
		WorkerHardCap:          2,
		EffectiveConcurrency:   1,
	}
	jobRecord, err := k12.NewGradingJobRecord(targetAgent, sessionID, k12.GradingJobFields{
		SubmissionID:      submissionID,
		SourceKind:        "image_task",
		IdempotencyKey:    k12.BuildGradingIdempotencyKey("image_task", dispatchID, 0),
		ConfirmationState: k12.GradingConfirmationPending,
		AnchorState:       k12.GradingAnchorPending,
		ModelSnapshot:     route,
		BudgetSnapshot:    budget,
	})
	if err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	if _, err := recordsStore.Put(ctx, jobRecord); err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	jobID := jobRecord.RecordID
	if _, err := sqliteStore.DB().ExecContext(ctx, `
		INSERT INTO k12_image_task_dispatches (
			dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,
			source_asset_refs_json,source_digest,message_intent,task_intent,
			intent_evidence_json,intent_confidence,confirmation_candidates_json,status,
			target_object_type,target_object_id,classification_route_snapshot_json,
			classification_invocation_id,route_policy_snapshot_json,idempotency_key,
			request_digest,attempt_generation,retry_safe,failure_kind,version,created_at,updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		dispatchID,
		targetAgent,
		"learner-private",
		"desktop",
		"source-private",
		sessionID,
		`["asset-private"]`,
		sourceDigest,
		"grade",
		"completed_homework",
		`[]`,
		1.0,
		`[]`,
		"routed",
		"homework_submission",
		submissionID,
		`{}`,
		"classification-private",
		`{}`,
		"dispatch-key-private",
		"sha256:dispatch-request-private",
		1,
		0,
		"",
		1,
		100,
		100,
	); err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	if _, err := sqliteStore.DB().ExecContext(ctx, `
		INSERT INTO k12_homework_submissions (
			submission_id,dispatch_id,agent_name,learner_id,source_kind,source_ref,
			source_asset_refs_json,task_intent,status,grading_job_id,idempotency_key,
			version,created_at,updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		submissionID,
		dispatchID,
		targetAgent,
		"learner-private",
		"desktop",
		"source-private",
		`["asset-private"]`,
		"completed_homework",
		"awaiting_confirmation",
		jobID,
		"submission-key-private",
		1,
		100,
		100,
	); err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}

	page := recognitionV2EvidencePagePNG(t)
	manifestID := "physical-manifest-private"
	manifestContent := `{"targets":["manifest_0001","manifest_0002","manifest_0003","manifest_0004"]}`
	manifestDigest := recognitionV2EvidenceStoredDigest(manifestContent)
	manifestTargets := []k12.RecognitionLayoutManifestTargetV2{
		{ManifestRef: "manifest_0001", ManifestOrder: 1, SourceNumberPath: []string{"1"}, DisplayLabel: "1", Region: k12.SourcePixelRegion{X: 0, Y: 0, Width: 40, Height: 10}},
		{ManifestRef: "manifest_0002", ManifestOrder: 2, SourceNumberPath: []string{"2"}, DisplayLabel: "2", Region: k12.SourcePixelRegion{X: 0, Y: 10, Width: 40, Height: 10}},
		{ManifestRef: "manifest_0003", ManifestOrder: 3, SourceNumberPath: []string{"3"}, DisplayLabel: "3", Region: k12.SourcePixelRegion{X: 0, Y: 20, Width: 40, Height: 10}},
		{ManifestRef: "manifest_0004", ManifestOrder: 4, SourceNumberPath: []string{"4"}, DisplayLabel: "4", Region: k12.SourcePixelRegion{X: 0, Y: 30, Width: 40, Height: 10}},
	}
	plan, err := k12.BuildRecognitionLayoutPlanV2(k12.RecognitionLayoutPlanInputV2{
		PagePNG: page,
		Manifest: k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: manifestID,
			ResultDigest: manifestDigest,
		},
		Targets: manifestTargets,
	})
	if err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	parent := k12.ModelInvocation{
		InvocationID:          "parent-private",
		AgentName:             targetAgent,
		JobID:                 jobID,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         "sha256:parent-request-private",
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               1,
		CreatedAt:             100,
	}
	stageStarted := time.Now().UnixMilli()
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-private",
		ParentInvocationID:       parent.InvocationID,
		AgentName:                targetAgent,
		JobID:                    jobID,
		PageDigest:               plan.PageDigest,
		ParentRequestDigest:      parent.RequestDigest,
		RouteSnapshot:            route,
		RequestPolicySnapshot:    policy,
		StageStartedAtUnixMillis: stageStarted,
		PhysicalCallCapMillis:    120_000,
		BudgetBuckets:            buckets,
		AdapterWorkerHardCap:     2,
		EffectiveConcurrency:     1,
	}
	headerDigest, err := k12.RecognitionLayoutPlanHeaderDigestV2(header)
	if err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	manifestChild := recognitionV2EvidencePhysical(
		parent,
		manifestID,
		k12.RecognitionPhysicalUnitWholePage,
		headerDigest,
		"",
	)
	storedParent, storedManifest, created, err :=
		recordsStore.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
			ctx,
			parent,
			manifestChild,
			header,
		)
	if err != nil || !created {
		_ = sqliteStore.Close()
		t.Fatalf("publish V2 evidence plan: created=%v err=%v", created, err)
	}
	if _, claimed, err := recordsStore.ClaimModelPhysicalInvocationSent(
		ctx,
		targetAgent,
		storedManifest.PhysicalInvocationID,
	); err != nil || !claimed {
		_ = sqliteStore.Close()
		t.Fatalf("claim manifest: claimed=%v err=%v", claimed, err)
	}
	if _, err := recordsStore.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		targetAgent,
		storedManifest.PhysicalInvocationID,
		manifestContent,
		"provider-manifest-private",
	); err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	if err := recordsStore.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		targetAgent,
		storedParent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: storedManifest.PhysicalInvocationID,
			ResultDigest: manifestDigest,
		},
		plan,
	); err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	batch := plan.Batches[0]
	batchExactSet, err := k12.RecognitionLayoutTargetExactSetDigestV2(batch.TargetIDs)
	if err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	batchChild := recognitionV2EvidencePhysical(
		storedParent,
		"physical-batch-private",
		batch.Unit,
		plan.AuthorizedPlanDigest,
		batchExactSet,
	)
	storedBatch, created, err := recordsStore.PrepareModelPhysicalInvocation(ctx, batchChild)
	if err != nil || !created {
		_ = sqliteStore.Close()
		t.Fatalf("prepare batch: created=%v err=%v", created, err)
	}
	if _, claimed, err := recordsStore.ClaimModelPhysicalInvocationSent(
		ctx,
		targetAgent,
		storedBatch.PhysicalInvocationID,
	); err != nil || !claimed {
		_ = sqliteStore.Close()
		t.Fatalf("claim batch: claimed=%v err=%v", claimed, err)
	}
	storedBatch, err = recordsStore.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		targetAgent,
		storedBatch.PhysicalInvocationID,
		`{"provider-private-payload":"batch"}`,
		"provider-batch-private",
	)
	if err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	settled, created, err := recordsStore.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		targetAgent,
		storedParent.InvocationID,
		k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			SourcePhysicalInvocationID: storedBatch.PhysicalInvocationID,
			SourcePhysicalUnit:         storedBatch.PhysicalUnit,
			SourcePhysicalResultDigest: storedBatch.ResultDigest,
			Classification:             k12.RecognitionLayoutBatchClassifiedV2,
			Candidates: []k12.RecognitionLayoutCandidateSettlementV2{
				{CandidateID: plan.Targets[0].TargetID, Classification: k12.RecognitionLayoutCandidateValidV2, ResultKind: k12.RecognitionLayoutCandidateQuestionV2, ResultJSON: json.RawMessage(`{"text":"private question text"}`)},
				{CandidateID: plan.Targets[1].TargetID, Classification: k12.RecognitionLayoutCandidateValidV2, ResultKind: k12.RecognitionLayoutCandidateNonQuestionV2, ResultJSON: json.RawMessage(`{"reason":"private non-question reason"}`)},
				{CandidateID: plan.Targets[2].TargetID, Classification: k12.RecognitionLayoutCandidateMissingV2},
				{CandidateID: plan.Targets[3].TargetID, Classification: k12.RecognitionLayoutCandidateValidV2, ResultKind: k12.RecognitionLayoutCandidateQuestionV2, ResultJSON: json.RawMessage(`{"text":"private question text 4"}`)},
			},
		},
	)
	if err != nil || !created || len(settled.RepairAuthorizations) != 1 {
		_ = sqliteStore.Close()
		t.Fatalf("settle primary: created=%v result=%+v err=%v", created, settled, err)
	}
	repairAuthorization := settled.RepairAuthorizations[0]
	repairExactSet, err := k12.RecognitionLayoutTargetExactSetDigestV2(
		[]string{repairAuthorization.CandidateID},
	)
	if err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	repairChild := recognitionV2EvidencePhysical(
		storedParent,
		"physical-repair-private",
		repairAuthorization.PhysicalUnit,
		plan.AuthorizedPlanDigest,
		repairExactSet,
	)
	storedRepair, created, err := recordsStore.PrepareModelPhysicalInvocation(ctx, repairChild)
	if err != nil || !created {
		_ = sqliteStore.Close()
		t.Fatalf("prepare repair: created=%v err=%v", created, err)
	}
	if _, claimed, err := recordsStore.ClaimModelPhysicalInvocationSent(
		ctx,
		targetAgent,
		storedRepair.PhysicalInvocationID,
	); err != nil || !claimed {
		_ = sqliteStore.Close()
		t.Fatalf("claim repair: claimed=%v err=%v", claimed, err)
	}
	storedRepair, err = recordsStore.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		targetAgent,
		storedRepair.PhysicalInvocationID,
		`{"provider-private-payload":"repair"}`,
		"provider-repair-private",
	)
	if err != nil {
		_ = sqliteStore.Close()
		t.Fatal(err)
	}
	if _, created, err := recordsStore.SettleRecognitionLayoutRepairV2(
		ctx,
		targetAgent,
		storedParent.InvocationID,
		k12.RecognitionLayoutRepairSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			AuthorizationID:            repairAuthorization.AuthorizationID,
			AuthorizationDigest:        repairAuthorization.AuthorizationDigest,
			CandidateID:                repairAuthorization.CandidateID,
			SourcePhysicalInvocationID: storedRepair.PhysicalInvocationID,
			SourcePhysicalUnit:         storedRepair.PhysicalUnit,
			SourcePhysicalResultDigest: storedRepair.ResultDigest,
			Classification:             k12.RecognitionLayoutCandidateValidV2,
			ResultKind:                 k12.RecognitionLayoutCandidateQuestionV2,
			ResultJSON:                 json.RawMessage(`{"text":"private repair text"}`),
		},
	); err != nil || !created {
		_ = sqliteStore.Close()
		t.Fatalf("settle repair: created=%v err=%v", created, err)
	}

	var finalization k12.RecognitionLayoutPlanFinalizationResultV2
	if finalize {
		finalization, created, err = recordsStore.FinalizeRecognitionLayoutPlanV2(
			ctx,
			targetAgent,
			storedParent.InvocationID,
		)
		if err != nil || !created {
			_ = sqliteStore.Close()
			t.Fatalf("finalize V2 plan: created=%v err=%v", created, err)
		}
		if _, err := recordsStore.MarkModelInvocationSucceeded(
			ctx,
			targetAgent,
			storedParent.InvocationID,
			finalization.FinalizationDigest,
			"",
		); err != nil {
			_ = sqliteStore.Close()
			t.Fatal(err)
		}
	}
	if err := sqliteStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(storePath, 0o600); err != nil {
		t.Fatal(err)
	}
	candidateIDs := make([]string, len(plan.Targets))
	for index := range plan.Targets {
		candidateIDs[index] = plan.Targets[index].TargetID
	}
	return recognitionV2EvidenceFixture{
		profile: profile, storePath: storePath, manifestPath: manifestPath,
		claimPath: claimPath, manifest: manifest, runID: runID,
		targetAgent: targetAgent, dispatchID: dispatchID, sessionID: sessionID,
		sourceDigest: sourceDigest, submissionID: submissionID, jobID: jobID,
		parentID: storedParent.InvocationID, headerDigest: headerDigest,
		stageStarted: stageStarted, stageDeadline: stageStarted + 120_000,
		buckets: buckets,
		plan:    plan, finalization: finalization,
		physicalIDs: []string{
			storedManifest.PhysicalInvocationID,
			storedBatch.PhysicalInvocationID,
			storedRepair.PhysicalInvocationID,
		},
		candidateIDs: candidateIDs,
	}
}

func recognitionV2EvidencePhysical(
	parent k12.ModelInvocation,
	id string,
	unit k12.RecognitionPhysicalUnit,
	planDigest string,
	candidateExactSetDigest string,
) k12.ModelPhysicalInvocation {
	return k12.ModelPhysicalInvocation{
		PhysicalInvocationID:    id,
		ParentInvocationID:      parent.InvocationID,
		AgentName:               parent.AgentName,
		JobID:                   parent.JobID,
		Stage:                   parent.Stage,
		PhysicalUnit:            unit,
		RecognitionPlanVersion:  k12.RecognitionPlanVersionV2,
		PlanDigest:              planDigest,
		CandidateExactSetDigest: candidateExactSetDigest,
		RequestDigest:           "sha256:request-" + string(unit),
		RouteSnapshot:           parent.RouteSnapshot,
		RequestPolicySnapshot:   parent.RequestPolicySnapshot,
		Attempt:                 1,
		CreatedAt:               200,
	}
}

func recognitionV2EvidencePagePNG(t *testing.T) []byte {
	t.Helper()
	page := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			page.Set(x, y, color.RGBA{R: uint8(x + y), G: 180, B: 220, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, page); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func recognitionV2EvidenceStoredDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func assertBareSHA256(t *testing.T, name, value string) {
	t.Helper()
	if len(value) != 64 || strings.Trim(value, "0123456789abcdef") != "" ||
		strings.HasPrefix(value, "sha256:") {
		t.Fatalf("%s=%q, want bare lowercase SHA-256", name, value)
	}
}
