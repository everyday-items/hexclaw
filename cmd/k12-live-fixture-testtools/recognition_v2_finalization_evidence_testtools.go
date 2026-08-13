//go:build testtools

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/livetestfixture"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"golang.org/x/sys/unix"
)

const (
	recognitionV2EvidenceSchemaVersion = 1
	recognitionV2EvidenceHashDomain    = "hexclaw:k12:recognition-v2-finalization-evidence:v1"
)

type recognitionV2EvidenceOptions struct {
	commonOptions
	manifest string
	claim    string
}

type recognitionV2TargetClaim struct {
	SchemaVersion   int    `json:"schema_version"`
	TargetAgent     string `json:"target_agent"`
	DispatchID      string `json:"dispatch_id"`
	SourceSessionID string `json:"source_session_id"`
	SourceDigest    string `json:"source_digest"`
}

var recognitionV2PrivateSnapshotOpenedHook func(string)

type recognitionV2EvidenceBatch struct {
	Ordinal                 int    `json:"ordinal"`
	PhysicalUnit            string `json:"physical_unit"`
	CandidateCount          int    `json:"candidate_count"`
	CandidateExactSetSHA256 string `json:"candidate_exact_set_sha256"`
}

type recognitionV2EvidenceRepair struct {
	PhysicalUnit            string `json:"physical_unit"`
	CandidateOrdinal        int    `json:"candidate_ordinal"`
	CandidateRefSHA256      string `json:"candidate_ref_sha256"`
	RepairRound             int    `json:"repair_round"`
	AuthorizationSHA256     string `json:"authorization_sha256"`
	SettlementSHA256        string `json:"settlement_sha256"`
	CandidateExactSetSHA256 string `json:"candidate_exact_set_sha256"`
}

type recognitionV2EvidencePhysicalReceipt struct {
	Ordinal                   int    `json:"ordinal"`
	InvocationSHA256          string `json:"invocation_sha256"`
	ParentInvocationSHA256    string `json:"parent_invocation_sha256"`
	PhysicalUnit              string `json:"physical_unit"`
	Operation                 string `json:"operation"`
	Provider                  string `json:"provider"`
	Model                     string `json:"model"`
	Status                    string `json:"status"`
	Attempt                   int    `json:"attempt"`
	ResultSHA256              string `json:"result_sha256"`
	RecognitionPlanVersion    int    `json:"recognition_plan_version"`
	PlanSHA256                string `json:"plan_sha256"`
	CandidateExactSetSHA256   string `json:"candidate_exact_set_sha256"`
	StageDeadlineAtUnixMillis int64  `json:"stage_deadline_at_unix_millis"`
}

type recognitionV2FinalizationEvidence struct {
	SchemaVersion                  int                                    `json:"schema_version"`
	EvidenceClass                  string                                 `json:"evidence_class"`
	Complete                       bool                                   `json:"complete"`
	EligibleForPass                bool                                   `json:"eligible_for_pass"`
	ExternalBoundaryAttested       bool                                   `json:"external_boundary_attested"`
	ManifestSHA256                 string                                 `json:"manifest_sha256"`
	ClaimSHA256                    string                                 `json:"claim_sha256"`
	RunSHA256                      string                                 `json:"run_sha256"`
	OwnershipSHA256                string                                 `json:"ownership_sha256"`
	FixtureAgentSHA256             string                                 `json:"fixture_agent_sha256"`
	TargetAgentSHA256              string                                 `json:"target_agent_sha256"`
	DispatchSHA256                 string                                 `json:"dispatch_sha256"`
	SourceSessionSHA256            string                                 `json:"source_session_sha256"`
	SourceDigestSHA256             string                                 `json:"source_digest_sha256"`
	SubmissionSHA256               string                                 `json:"submission_sha256"`
	JobSHA256                      string                                 `json:"job_sha256"`
	ParentInvocationSHA256         string                                 `json:"parent_invocation_sha256"`
	PlanIDSHA256                   string                                 `json:"plan_id_sha256"`
	RecognitionPlanVersion         int                                    `json:"recognition_plan_version"`
	Status                         string                                 `json:"status"`
	ParentStatus                   string                                 `json:"parent_status"`
	ParentAttempt                  int                                    `json:"parent_attempt"`
	Provider                       string                                 `json:"provider"`
	Model                          string                                 `json:"model"`
	HeaderSHA256                   string                                 `json:"header_sha256"`
	AuthorizedPlanSHA256           string                                 `json:"authorized_plan_sha256"`
	CandidateExactSetSHA256        string                                 `json:"candidate_exact_set_sha256"`
	CandidateResultsExactSetSHA256 string                                 `json:"candidate_results_exact_set_sha256"`
	PhysicalResultsExactSetSHA256  string                                 `json:"physical_results_exact_set_sha256"`
	FinalizationSHA256             string                                 `json:"finalization_sha256"`
	StageStartedAtUnixMillis       int64                                  `json:"stage_started_at_unix_millis"`
	StageDeadlineAtUnixMillis      int64                                  `json:"stage_deadline_at_unix_millis"`
	SelectedBucketMaxProblems      int                                    `json:"selected_bucket_max_problems"`
	BudgetBucketsMillis            k12.RecognitionLayoutBudgetBucketsV2   `json:"budget_buckets_millis"`
	PhysicalCallCapMillis          int64                                  `json:"physical_call_cap_millis"`
	AdapterWorkerHardCap           int                                    `json:"adapter_worker_hard_cap"`
	EffectiveConcurrency           int                                    `json:"effective_concurrency"`
	CandidateResultCount           int                                    `json:"candidate_result_count"`
	QuestionCount                  int                                    `json:"question_count"`
	NonQuestionCount               int                                    `json:"non_question_count"`
	PhysicalResultCount            int                                    `json:"physical_result_count"`
	AuthorizedBatches              []recognitionV2EvidenceBatch           `json:"authorized_batches"`
	AuthorizedRepairs              []recognitionV2EvidenceRepair          `json:"authorized_repairs"`
	PhysicalReceipts               []recognitionV2EvidencePhysicalReceipt `json:"physical_receipts"`
}

func parseRecognitionV2EvidenceOptions(
	args []string,
	stderr io.Writer,
) (recognitionV2EvidenceOptions, error) {
	var options recognitionV2EvidenceOptions
	flags := flag.NewFlagSet("recognition-v2-finalization-evidence", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.profile, "profile", "", "isolated /tmp profile")
	flags.StringVar(&options.store, "store", "", "existing isolated SQLite store")
	flags.StringVar(&options.manifest, "manifest", "", "current 0600 fixture manifest")
	flags.StringVar(&options.claim, "claim", "", "current 0600 target claim")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return recognitionV2EvidenceOptions{}, errors.New(
			"invalid recognition-v2-finalization-evidence arguments",
		)
	}
	if strings.TrimSpace(options.manifest) == "" || strings.TrimSpace(options.claim) == "" {
		return recognitionV2EvidenceOptions{}, errors.New(
			"recognition-v2-finalization-evidence requires manifest and claim",
		)
	}
	return options, nil
}

func executeRecognitionV2FinalizationEvidence(
	ctx context.Context,
	options recognitionV2EvidenceOptions,
	stdout io.Writer,
) error {
	resolved, err := resolveCommon(options.commonOptions)
	if err != nil {
		return err
	}
	return withProfileLock(resolved.profile, func() error {
		manifestPath, manifestBytes, err := readRecognitionV2PrivateSnapshot(
			resolved.profile,
			options.manifest,
			"fixture manifest",
		)
		if err != nil {
			return err
		}
		claimPath, claimBytes, err := readRecognitionV2PrivateSnapshot(
			resolved.profile,
			options.claim,
			"target claim",
		)
		if err != nil {
			return err
		}
		if manifestPath == claimPath {
			return errors.New("fixture manifest and target claim must be distinct")
		}
		if err := rejectRecognitionV2DuplicateObjectFields(
			manifestBytes,
			"fixture manifest",
		); err != nil {
			return err
		}
		manifest, err := decodeManifest(manifestBytes)
		if err != nil {
			return err
		}
		claim, err := decodeRecognitionV2TargetClaim(claimBytes)
		if err != nil {
			return err
		}

		db, err := openPartialLedgerDiagnosticDatabase(ctx, resolved.store)
		if err != nil {
			return errors.New("recognition V2 finalization evidence failed")
		}
		defer db.Close()
		registry := scenario.NewRegistry()
		if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
			return errors.New("recognition V2 finalization evidence failed")
		}
		store := k12storage.NewStore(db, registry.Records)
		snapshot, err := store.LoadRecognitionV2FinalizationEvidenceSnapshotForTesttools(
			ctx,
			manifest.AgentName,
			k12storage.RecognitionV2FinalizationEvidenceClaim{
				TargetAgent:     claim.TargetAgent,
				DispatchID:      claim.DispatchID,
				SourceSessionID: claim.SourceSessionID,
				SourceDigest:    claim.SourceDigest,
			},
		)
		if err != nil {
			return errors.New("recognition V2 finalization evidence failed")
		}
		runID, matches := livetestfixture.VerifiedManifestRunID(
			fixtureFromManifest(manifest),
			router.AgentConfig{
				Name:     manifest.AgentName,
				Metadata: snapshot.FixtureAgentMetadata,
			},
		)
		if !matches {
			return errors.New("recognition V2 finalization evidence failed")
		}
		receipt, err := buildRecognitionV2FinalizationEvidence(
			manifestBytes,
			claimBytes,
			manifest,
			runID,
			snapshot,
		)
		if err != nil {
			return errors.New("recognition V2 finalization evidence failed")
		}
		return writeJSON(stdout, receipt)
	})
}

func buildRecognitionV2FinalizationEvidence(
	manifestBytes []byte,
	claimBytes []byte,
	manifest manifestFile,
	runID string,
	snapshot k12storage.RecognitionV2FinalizationEvidenceSnapshot,
) (recognitionV2FinalizationEvidence, error) {
	header, err := recognitionV2BareSHA256(snapshot.HeaderDigest)
	if err != nil {
		return recognitionV2FinalizationEvidence{}, err
	}
	authorizedPlan, err := recognitionV2BareSHA256(snapshot.AuthorizedPlanDigest)
	if err != nil {
		return recognitionV2FinalizationEvidence{}, err
	}
	candidateExactSet, err := recognitionV2BareSHA256(snapshot.CandidateExactSetDigest)
	if err != nil {
		return recognitionV2FinalizationEvidence{}, err
	}
	candidateResults, err := recognitionV2BareSHA256(snapshot.CandidateResultsExactSetDigest)
	if err != nil {
		return recognitionV2FinalizationEvidence{}, err
	}
	physicalResults, err := recognitionV2BareSHA256(snapshot.PhysicalResultsExactSetDigest)
	if err != nil {
		return recognitionV2FinalizationEvidence{}, err
	}
	finalization, err := recognitionV2BareSHA256(snapshot.FinalizationDigest)
	if err != nil {
		return recognitionV2FinalizationEvidence{}, err
	}
	sourceDigest, err := recognitionV2BareSHA256(snapshot.SourceDigest)
	if err != nil {
		return recognitionV2FinalizationEvidence{}, err
	}

	batches := make([]recognitionV2EvidenceBatch, 0, len(snapshot.AuthorizedBatches))
	for _, batch := range snapshot.AuthorizedBatches {
		digest, err := recognitionV2BareSHA256(batch.CandidateExactSetDigest)
		if err != nil {
			return recognitionV2FinalizationEvidence{}, err
		}
		batches = append(batches, recognitionV2EvidenceBatch{
			Ordinal:                 batch.Ordinal,
			PhysicalUnit:            string(batch.PhysicalUnit),
			CandidateCount:          batch.CandidateCount,
			CandidateExactSetSHA256: digest,
		})
	}
	repairs := make([]recognitionV2EvidenceRepair, 0, len(snapshot.AuthorizedRepairs))
	for _, repair := range snapshot.AuthorizedRepairs {
		authorization, err := recognitionV2BareSHA256(repair.AuthorizationDigest)
		if err != nil {
			return recognitionV2FinalizationEvidence{}, err
		}
		settlement, err := recognitionV2BareSHA256(repair.SettlementDigest)
		if err != nil {
			return recognitionV2FinalizationEvidence{}, err
		}
		exactSet, err := recognitionV2BareSHA256(repair.CandidateExactSetDigest)
		if err != nil {
			return recognitionV2FinalizationEvidence{}, err
		}
		repairs = append(repairs, recognitionV2EvidenceRepair{
			PhysicalUnit:            string(repair.PhysicalUnit),
			CandidateOrdinal:        repair.CandidateOrdinal,
			CandidateRefSHA256:      recognitionV2EvidenceDigest("candidate", repair.CandidateID),
			RepairRound:             repair.RepairRound,
			AuthorizationSHA256:     authorization,
			SettlementSHA256:        settlement,
			CandidateExactSetSHA256: exactSet,
		})
	}
	parentDigest := recognitionV2EvidenceDigest(
		"parent_invocation",
		snapshot.ParentInvocationID,
	)
	physical := make(
		[]recognitionV2EvidencePhysicalReceipt,
		0,
		len(snapshot.PhysicalReceipts),
	)
	for _, value := range snapshot.PhysicalReceipts {
		resultDigest, err := recognitionV2BareSHA256(value.ResultDigest)
		if err != nil {
			return recognitionV2FinalizationEvidence{}, err
		}
		planDigest, err := recognitionV2BareSHA256(value.PlanDigest)
		if err != nil {
			return recognitionV2FinalizationEvidence{}, err
		}
		physicalExactSet := ""
		if value.CandidateExactSetDigest != "" {
			physicalExactSet, err = recognitionV2BareSHA256(value.CandidateExactSetDigest)
			if err != nil {
				return recognitionV2FinalizationEvidence{}, err
			}
		}
		physical = append(physical, recognitionV2EvidencePhysicalReceipt{
			Ordinal:                   value.Ordinal,
			InvocationSHA256:          recognitionV2EvidenceDigest("physical_invocation", value.PhysicalInvocationID),
			ParentInvocationSHA256:    parentDigest,
			PhysicalUnit:              string(value.PhysicalUnit),
			Operation:                 k12.GradingStageRecognizing,
			Provider:                  value.Provider,
			Model:                     value.Model,
			Status:                    string(value.Status),
			Attempt:                   value.Attempt,
			ResultSHA256:              resultDigest,
			RecognitionPlanVersion:    value.RecognitionPlanVersion,
			PlanSHA256:                planDigest,
			CandidateExactSetSHA256:   physicalExactSet,
			StageDeadlineAtUnixMillis: snapshot.StageDeadlineAtUnixMillis,
		})
	}

	return recognitionV2FinalizationEvidence{
		SchemaVersion:                  recognitionV2EvidenceSchemaVersion,
		EvidenceClass:                  "recognition_v2_finalization",
		Complete:                       true,
		EligibleForPass:                true,
		ExternalBoundaryAttested:       false,
		ManifestSHA256:                 recognitionV2BytesSHA256(manifestBytes),
		ClaimSHA256:                    recognitionV2BytesSHA256(claimBytes),
		RunSHA256:                      recognitionV2EvidenceDigest("run", runID),
		OwnershipSHA256:                recognitionV2EvidenceDigest("ownership", manifest.Ownership),
		FixtureAgentSHA256:             recognitionV2EvidenceDigest("fixture_agent", manifest.AgentName),
		TargetAgentSHA256:              recognitionV2EvidenceDigest("target_agent", snapshot.TargetAgent),
		DispatchSHA256:                 recognitionV2EvidenceDigest("dispatch", snapshot.DispatchID),
		SourceSessionSHA256:            recognitionV2EvidenceDigest("source_session", snapshot.SourceSessionID),
		SourceDigestSHA256:             sourceDigest,
		SubmissionSHA256:               recognitionV2EvidenceDigest("submission", snapshot.SubmissionID),
		JobSHA256:                      recognitionV2EvidenceDigest("job", snapshot.JobID),
		ParentInvocationSHA256:         parentDigest,
		PlanIDSHA256:                   recognitionV2EvidenceDigest("plan_id", snapshot.PlanID),
		RecognitionPlanVersion:         snapshot.RecognitionPlanVersion,
		Status:                         snapshot.PlanStatus,
		ParentStatus:                   string(snapshot.ParentStatus),
		ParentAttempt:                  snapshot.ParentAttempt,
		Provider:                       snapshot.Provider,
		Model:                          snapshot.Model,
		HeaderSHA256:                   header,
		AuthorizedPlanSHA256:           authorizedPlan,
		CandidateExactSetSHA256:        candidateExactSet,
		CandidateResultsExactSetSHA256: candidateResults,
		PhysicalResultsExactSetSHA256:  physicalResults,
		FinalizationSHA256:             finalization,
		StageStartedAtUnixMillis:       snapshot.StageStartedAtUnixMillis,
		StageDeadlineAtUnixMillis:      snapshot.StageDeadlineAtUnixMillis,
		SelectedBucketMaxProblems:      snapshot.SelectedBucketMaxProblems,
		BudgetBucketsMillis:            snapshot.BudgetBuckets,
		PhysicalCallCapMillis:          snapshot.PhysicalCallCapMillis,
		AdapterWorkerHardCap:           snapshot.AdapterWorkerHardCap,
		EffectiveConcurrency:           snapshot.EffectiveConcurrency,
		CandidateResultCount:           snapshot.CandidateResultCount,
		QuestionCount:                  snapshot.QuestionCount,
		NonQuestionCount:               snapshot.NonQuestionCount,
		PhysicalResultCount:            snapshot.PhysicalResultCount,
		AuthorizedBatches:              batches,
		AuthorizedRepairs:              repairs,
		PhysicalReceipts:               physical,
	}, nil
}

func readRecognitionV2PrivateSnapshot(
	profile string,
	requested string,
	label string,
) (string, []byte, error) {
	if !filepath.IsAbs(requested) || strings.TrimSpace(requested) == "" {
		return "", nil, fmt.Errorf("%s path must be absolute", label)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(requested)))
	if err != nil || parent != profile && !strictDescendant(profile, parent) {
		return "", nil, fmt.Errorf("%s must be inside the isolated profile", label)
	}
	path := filepath.Join(parent, filepath.Base(filepath.Clean(requested)))
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", nil, fmt.Errorf("%s must be a non-symlink 0600 regular file", label)
	}
	file := os.NewFile(uintptr(fd), filepath.Base(path))
	if file == nil {
		_ = unix.Close(fd)
		return "", nil, fmt.Errorf("cannot open %s snapshot", label)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", nil, fmt.Errorf("%s must be a non-symlink 0600 regular file", label)
	}
	if recognitionV2PrivateSnapshotOpenedHook != nil {
		recognitionV2PrivateSnapshotOpenedHook(path)
	}
	const maximumPrivateSnapshotBytes = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(file, maximumPrivateSnapshotBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maximumPrivateSnapshotBytes {
		return "", nil, fmt.Errorf("%s snapshot is empty, oversized, or unreadable", label)
	}
	return path, raw, nil
}

func decodeRecognitionV2TargetClaim(raw []byte) (recognitionV2TargetClaim, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return recognitionV2TargetClaim{}, errors.New("target claim JSON is invalid")
	}
	fields := make(map[string]json.RawMessage, 5)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return recognitionV2TargetClaim{}, errors.New("target claim JSON is invalid")
		}
		if _, duplicate := fields[key]; duplicate {
			return recognitionV2TargetClaim{}, errors.New("target claim fields are duplicated")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return recognitionV2TargetClaim{}, errors.New("target claim JSON is invalid")
		}
		fields[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return recognitionV2TargetClaim{}, errors.New("target claim JSON is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return recognitionV2TargetClaim{}, errors.New("target claim has trailing data")
	}
	expected := []string{
		"schema_version",
		"target_agent",
		"dispatch_id",
		"source_session_id",
		"source_digest",
	}
	if len(fields) != len(expected) {
		return recognitionV2TargetClaim{}, errors.New("target claim fields are invalid")
	}
	for _, key := range expected {
		if _, ok := fields[key]; !ok {
			return recognitionV2TargetClaim{}, errors.New("target claim fields are invalid")
		}
	}
	var claim recognitionV2TargetClaim
	if err := json.Unmarshal(fields["schema_version"], &claim.SchemaVersion); err != nil ||
		claim.SchemaVersion != recognitionV2EvidenceSchemaVersion ||
		json.Unmarshal(fields["target_agent"], &claim.TargetAgent) != nil ||
		json.Unmarshal(fields["dispatch_id"], &claim.DispatchID) != nil ||
		json.Unmarshal(fields["source_session_id"], &claim.SourceSessionID) != nil ||
		json.Unmarshal(fields["source_digest"], &claim.SourceDigest) != nil ||
		!safeEvidenceLabel(claim.TargetAgent, 256) ||
		!safeEvidenceLabel(claim.DispatchID, 512) ||
		!safeEvidenceLabel(claim.SourceSessionID, 512) ||
		!recognitionV2PrefixedSHA256(claim.SourceDigest) {
		return recognitionV2TargetClaim{}, errors.New("target claim values are invalid")
	}
	return claim, nil
}

func rejectRecognitionV2DuplicateObjectFields(raw []byte, label string) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("%s JSON is invalid", label)
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return fmt.Errorf("%s JSON is invalid", label)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%s fields are duplicated", label)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("%s JSON is invalid", label)
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return fmt.Errorf("%s JSON is invalid", label)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s has trailing data", label)
	}
	return nil
}

func recognitionV2BareSHA256(value string) (string, error) {
	if !recognitionV2PrefixedSHA256(value) {
		return "", errors.New("recognition V2 evidence digest is invalid")
	}
	return strings.TrimPrefix(value, "sha256:"), nil
}

func recognitionV2PrefixedSHA256(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	digest := strings.TrimPrefix(value, "sha256:")
	return strings.Trim(digest, "0123456789abcdef") == ""
}

func recognitionV2EvidenceDigest(namespace string, values ...string) string {
	hash := sha256.New()
	writeEvidenceDigestPart(hash, recognitionV2EvidenceHashDomain)
	writeEvidenceDigestPart(hash, namespace)
	for _, value := range values {
		writeEvidenceDigestPart(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func recognitionV2BytesSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
