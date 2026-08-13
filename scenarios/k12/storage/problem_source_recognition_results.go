package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var (
	ErrProblemSourceRecognitionNotFound = errors.New(
		"problem source recognition result not found in owner scope",
	)
	ErrProblemSourceRecognitionInvalid = errors.New(
		"invalid problem source recognition result",
	)
	ErrProblemSourceRecognitionConflict = errors.New(
		"problem source recognition result conflict",
	)
	ErrProblemSourceRecognitionUnstableMapping = errors.New(
		"problem source recognition mapping is not a stable exact set",
	)
)

type ProblemSourceRecognitionMappingState string

const ProblemSourceRecognitionMappingStableExactSet ProblemSourceRecognitionMappingState = "stable_exact_set"

// ProblemSourceRecognitionPhysicalResultRef binds the normalized result to the
// exact durable provider responses that produced it. ResultDigest must equal
// the terminal child ledger; raw provider payload remains solely in
// k12_model_physical_invocations.result_content.
type ProblemSourceRecognitionPhysicalResultRef struct {
	PhysicalInvocationID    string `json:"physical_invocation_id"`
	PhysicalUnit            string `json:"physical_unit"`
	RecognitionPlanVersion  int    `json:"recognition_plan_version,omitempty"`
	PlanDigest              string `json:"plan_digest,omitempty"`
	CandidateExactSetDigest string `json:"candidate_exact_set_digest,omitempty"`
	ResultDigest            string `json:"result_digest"`
}

// 单个 V2 页面最多可证明一个 manifest、八个四目标 primary batch 和
// 三十二个单候选修复：1 + 8 + 32 = 41。
const problemSourceRecognitionPhysicalResultLimit = 41

// ProblemSourceRecognitionResult is the worker-supplied normalized result.
// Source image metadata is intentionally absent: Commit derives it from the
// current immutable input head and owner-scoped ready PageAsset in one tx.
type ProblemSourceRecognitionResult struct {
	MappingState       ProblemSourceRecognitionMappingState
	ParentInvocationID string
	PhysicalResults    []ProblemSourceRecognitionPhysicalResultRef
	Items              []ProblemSourceRecognitionItem
}

type ProblemSourceRecognitionItem struct {
	ProblemID                    string
	StemRaw                      string
	QuestionCanonicalMarkdown    string
	AnswerState                  string
	AnswerRaw                    string
	AnswerCanonicalMarkdown      string
	AnswerBBox                   *k12.AttemptBBox
	Subject                      string
	KnowledgePoints              []string
	RecognitionConfidence        *float64
	OCRSignals                   []string
	EvidenceTranscriptions       []string
	AnswerEvidenceTranscriptions []string
	ConfirmationRequired         bool
	ConfirmationReasons          []string
}

type ProblemSourceRecognitionSource struct {
	PageAssetID              string
	Region                   *k12.SourcePixelRegion
	ContentDigest            string
	MediaType                string
	SizeBytes                int64
	PixelWidth               int
	PixelHeight              int
	OrientationPolicy        PageAssetOrientationPolicy
	OrientationPolicyVersion string
	TransformChainJSON       json.RawMessage
}

type ProblemSourceRecognitionFact struct {
	ProblemSourceRecognitionItem
	Ordinal       int
	InputRevision int
	InputDigest   string
	Source        ProblemSourceRecognitionSource
}

// ProblemSourceRecognitionCommit is a restart-rebuildable immutable result
// projection. Created=false from Commit means an exact digest replay; it never
// means a second input revision was appended.
type ProblemSourceRecognitionCommit struct {
	WorkID                  string
	CommandReceiptID        string
	OwnerScope              string
	AgentName               string
	SubmissionID            string
	DispatchID              string
	JobID                   string
	PathProblemID           string
	ParentInvocationID      string
	ParentRequestDigest     string
	ParentInvocationAttempt int
	PhysicalResults         []ProblemSourceRecognitionPhysicalResultRef
	Action                  string
	StructureVersion        int
	SourceInputRevision     int
	ResultInputRevision     int
	ResultDigest            string
	MappingState            ProblemSourceRecognitionMappingState
	StructureDigest         string
	AffectedProblemIDs      []string
	Items                   []ProblemSourceRecognitionFact
	CreatedAt               int64
}

type normalizedProblemSourceRecognitionResult struct {
	MappingState       ProblemSourceRecognitionMappingState        `json:"mapping_state"`
	ParentInvocationID string                                      `json:"parent_invocation_id"`
	PhysicalResults    []ProblemSourceRecognitionPhysicalResultRef `json:"physical_results"`
	Items              []normalizedProblemSourceRecognitionItem    `json:"items"`
}

type normalizedProblemSourceRecognitionItem struct {
	ProblemID                    string           `json:"problem_id"`
	StemRaw                      string           `json:"stem_raw"`
	QuestionCanonicalMarkdown    string           `json:"question_canonical_markdown"`
	AnswerState                  string           `json:"answer_state"`
	AnswerRaw                    string           `json:"answer_raw"`
	AnswerCanonicalMarkdown      string           `json:"answer_canonical_markdown"`
	AnswerBBox                   *k12.AttemptBBox `json:"answer_bbox,omitempty"`
	Subject                      string           `json:"subject"`
	KnowledgePoints              []string         `json:"knowledge_points"`
	RecognitionConfidence        *float64         `json:"recognition_confidence,omitempty"`
	OCRSignals                   []string         `json:"ocr_signals"`
	EvidenceTranscriptions       []string         `json:"evidence_transcriptions"`
	AnswerEvidenceTranscriptions []string         `json:"answer_evidence_transcriptions"`
	ConfirmationRequired         bool             `json:"confirmation_required"`
	ConfirmationReasons          []string         `json:"confirmation_reasons"`
}

// ProblemSourceRecognitionParentRequestDigest is the deterministic binding
// between one immutable source-reprocess work identity and the recognizing
// parent model invocation prepared for it. The request JSON already contains
// the selected content-addressed PageAsset/region request; route and request
// policy are included so a parent prepared under another physical request
// contract cannot be rebound to this work merely because it shares a job.
func ProblemSourceRecognitionParentRequestDigest(
	job ProblemSourceReprocessJob,
	route k12.GradingModelSnapshot,
	policy k12.ModelRequestPolicySnapshot,
) (string, error) {
	job.WorkID = strings.TrimSpace(job.WorkID)
	job.CommandReceiptID = strings.TrimSpace(job.CommandReceiptID)
	job.OwnerScope = strings.TrimSpace(job.OwnerScope)
	job.AgentName = strings.TrimSpace(job.AgentName)
	job.DispatchID = strings.TrimSpace(job.DispatchID)
	job.JobID = strings.TrimSpace(job.JobID)
	job.ProblemID = strings.TrimSpace(job.ProblemID)
	job.Action = strings.TrimSpace(job.Action)
	job.InputDigest = strings.TrimSpace(job.InputDigest)
	if job.WorkID == "" || job.CommandReceiptID == "" || job.OwnerScope == "" ||
		job.AgentName == "" || job.DispatchID == "" || job.JobID == "" ||
		job.ProblemID == "" || (job.Action != "select_region" && job.Action != "retake") ||
		job.StructureVersion < 1 || job.InputRevision < 1 || job.InputDigest == "" ||
		len(job.AffectedProblemIDs) == 0 {
		return "", fmt.Errorf(
			"%w: incomplete source recognition work identity",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	affected := make([]string, len(job.AffectedProblemIDs))
	seen := make(map[string]struct{}, len(job.AffectedProblemIDs))
	for index := range job.AffectedProblemIDs {
		affected[index] = strings.TrimSpace(job.AffectedProblemIDs[index])
		if affected[index] == "" {
			return "", fmt.Errorf(
				"%w: source recognition work contains an empty affected problem",
				ErrProblemSourceRecognitionInvalid,
			)
		}
		if _, duplicate := seen[affected[index]]; duplicate {
			return "", fmt.Errorf(
				"%w: source recognition work contains duplicate affected problem %q",
				ErrProblemSourceRecognitionInvalid,
				affected[index],
			)
		}
		seen[affected[index]] = struct{}{}
	}
	var request any
	if err := json.Unmarshal(job.RequestJSON, &request); err != nil {
		return "", fmt.Errorf(
			"%w: source recognition work request is not JSON",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	if _, object := request.(map[string]any); !object {
		return "", fmt.Errorf(
			"%w: source recognition work request must be an object",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	route = k12.NormalizeGradingModelSnapshot(route)
	policy = k12.NormalizeModelRequestPolicySnapshot(policy)
	if route.Provider == "" || route.Model == "" || route.Route == "" {
		return "", fmt.Errorf(
			"%w: source recognition parent route is incomplete",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	payload := struct {
		Contract           string                         `json:"contract"`
		WorkID             string                         `json:"work_id"`
		CommandReceiptID   string                         `json:"command_receipt_id"`
		OwnerScope         string                         `json:"owner_scope"`
		AgentName          string                         `json:"agent_name"`
		DispatchID         string                         `json:"dispatch_id"`
		JobID              string                         `json:"job_id"`
		PathProblemID      string                         `json:"path_problem_id"`
		Action             string                         `json:"action"`
		StructureVersion   int                            `json:"structure_version"`
		InputRevision      int                            `json:"input_revision"`
		InputDigest        string                         `json:"input_digest"`
		AffectedProblemIDs []string                       `json:"affected_problem_ids"`
		Request            any                            `json:"request"`
		Route              k12.GradingModelSnapshot       `json:"route"`
		RequestPolicy      k12.ModelRequestPolicySnapshot `json:"request_policy"`
	}{
		Contract:           "k12.problem_source_recognition.parent_request.v1",
		WorkID:             job.WorkID,
		CommandReceiptID:   job.CommandReceiptID,
		OwnerScope:         job.OwnerScope,
		AgentName:          job.AgentName,
		DispatchID:         job.DispatchID,
		JobID:              job.JobID,
		PathProblemID:      job.ProblemID,
		Action:             job.Action,
		StructureVersion:   job.StructureVersion,
		InputRevision:      job.InputRevision,
		InputDigest:        job.InputDigest,
		AffectedProblemIDs: affected,
		Request:            request,
		Route:              route,
		RequestPolicy:      policy,
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf(
			"%w: canonicalize source recognition parent request: %v",
			ErrProblemSourceRecognitionInvalid,
			err,
		)
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func normalizeRecognitionStringSet(
	name string,
	values []string,
) ([]string, error) {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" || len(value) > 4096 {
			return nil, fmt.Errorf(
				"%w: %s contains empty or oversized evidence",
				ErrProblemSourceRecognitionInvalid,
				name,
			)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf(
				"%w: %s contains duplicate %q",
				ErrProblemSourceRecognitionInvalid,
				name,
				value,
			)
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func normalizeRecognitionBBox(
	bbox *k12.AttemptBBox,
) (*k12.AttemptBBox, error) {
	if bbox == nil {
		return nil, nil
	}
	copyBBox := *bbox
	values := []float64{copyBBox.X, copyBBox.Y, copyBBox.W, copyBBox.H}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf(
				"%w: answer bbox must contain finite coordinates",
				ErrProblemSourceRecognitionInvalid,
			)
		}
	}
	if copyBBox.X < 0 || copyBBox.Y < 0 || copyBBox.W <= 0 || copyBBox.H <= 0 ||
		copyBBox.X+copyBBox.W > 1 || copyBBox.Y+copyBBox.H > 1 {
		return nil, fmt.Errorf(
			"%w: answer bbox must fit normalized 0..1 coordinates",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	return &copyBBox, nil
}

func normalizeProblemSourceRecognitionResult(
	input ProblemSourceRecognitionResult,
) (normalizedProblemSourceRecognitionResult, string, error) {
	var normalized normalizedProblemSourceRecognitionResult
	normalized.MappingState = ProblemSourceRecognitionMappingState(
		strings.TrimSpace(string(input.MappingState)),
	)
	if normalized.MappingState != ProblemSourceRecognitionMappingStableExactSet {
		return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
			"%w: mapping_state=%q",
			ErrProblemSourceRecognitionUnstableMapping,
			normalized.MappingState,
		)
	}
	normalized.ParentInvocationID = strings.TrimSpace(input.ParentInvocationID)
	if normalized.ParentInvocationID == "" || len(normalized.ParentInvocationID) > 512 {
		return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
			"%w: a bounded parent invocation identity is required",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	if len(input.PhysicalResults) == 0 ||
		len(input.PhysicalResults) > problemSourceRecognitionPhysicalResultLimit {
		return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
			"%w: one to %d physical recognition results are required",
			ErrProblemSourceRecognitionInvalid,
			problemSourceRecognitionPhysicalResultLimit,
		)
	}
	normalized.PhysicalResults = make(
		[]ProblemSourceRecognitionPhysicalResultRef,
		len(input.PhysicalResults),
	)
	physicalIDs := make(map[string]struct{}, len(input.PhysicalResults))
	for index, raw := range input.PhysicalResults {
		ref := ProblemSourceRecognitionPhysicalResultRef{
			PhysicalInvocationID:    strings.TrimSpace(raw.PhysicalInvocationID),
			PhysicalUnit:            strings.ToLower(strings.TrimSpace(raw.PhysicalUnit)),
			RecognitionPlanVersion:  raw.RecognitionPlanVersion,
			PlanDigest:              strings.TrimSpace(raw.PlanDigest),
			CandidateExactSetDigest: strings.TrimSpace(raw.CandidateExactSetDigest),
			ResultDigest:            strings.TrimSpace(raw.ResultDigest),
		}
		if ref.PhysicalInvocationID == "" || len(ref.PhysicalInvocationID) > 512 ||
			ref.ResultDigest == "" || len(ref.ResultDigest) > 512 {
			return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
				"%w: bounded physical invocation identity and digest are required",
				ErrProblemSourceRecognitionInvalid,
			)
		}
		effectiveVersion := effectiveProblemSourceRecognitionPlanVersion(
			ref.RecognitionPlanVersion,
		)
		switch effectiveVersion {
		case k12.RecognitionPlanVersionV1:
			if !legacyRecognitionPhysicalUnit(k12.RecognitionPhysicalUnit(ref.PhysicalUnit)) ||
				ref.PlanDigest != "" || ref.CandidateExactSetDigest != "" {
				return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
					"%w: v1 physical invocation %s carries non-v1 plan facts",
					ErrProblemSourceRecognitionInvalid,
					ref.PhysicalInvocationID,
				)
			}
			// 逐字节保留历史 V1 聚合摘要。调用方传零值或显式传一均表示 V1，
			// 但规范 V1 JSON 会省略新引入的计划事实。
			ref.RecognitionPlanVersion = 0
		case k12.RecognitionPlanVersionV2:
			unit := k12.RecognitionPhysicalUnit(ref.PhysicalUnit)
			validManifest := unit == k12.RecognitionPhysicalUnitWholePage &&
				ref.CandidateExactSetDigest == ""
			validPlannedChild := layoutRecognitionPhysicalUnit(unit) &&
				ref.CandidateExactSetDigest != ""
			if ref.PlanDigest == "" || (!validManifest && !validPlannedChild) {
				return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
					"%w: v2 physical invocation %s has invalid plan/unit facts",
					ErrProblemSourceRecognitionInvalid,
					ref.PhysicalInvocationID,
				)
			}
		default:
			return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
				"%w: physical invocation %s has invalid plan version %d",
				ErrProblemSourceRecognitionInvalid,
				ref.PhysicalInvocationID,
				ref.RecognitionPlanVersion,
			)
		}
		if _, duplicate := physicalIDs[ref.PhysicalInvocationID]; duplicate {
			return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
				"%w: duplicate physical invocation %q",
				ErrProblemSourceRecognitionInvalid,
				ref.PhysicalInvocationID,
			)
		}
		physicalIDs[ref.PhysicalInvocationID] = struct{}{}
		normalized.PhysicalResults[index] = ref
	}
	sort.Slice(normalized.PhysicalResults, func(left, right int) bool {
		return normalized.PhysicalResults[left].PhysicalInvocationID <
			normalized.PhysicalResults[right].PhysicalInvocationID
	})

	if len(input.Items) == 0 || len(input.Items) > 512 {
		return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
			"%w: one to 512 recognition items are required",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	normalized.Items = make([]normalizedProblemSourceRecognitionItem, len(input.Items))
	problemIDs := make(map[string]struct{}, len(input.Items))
	for index, raw := range input.Items {
		item := normalizedProblemSourceRecognitionItem{
			ProblemID:                 strings.TrimSpace(raw.ProblemID),
			StemRaw:                   raw.StemRaw,
			QuestionCanonicalMarkdown: strings.TrimSpace(raw.QuestionCanonicalMarkdown),
			AnswerState:               strings.ToLower(strings.TrimSpace(raw.AnswerState)),
			AnswerRaw:                 raw.AnswerRaw,
			AnswerCanonicalMarkdown:   strings.TrimSpace(raw.AnswerCanonicalMarkdown),
			Subject:                   strings.TrimSpace(raw.Subject),
			ConfirmationRequired:      raw.ConfirmationRequired,
		}
		if item.ProblemID == "" || len(item.ProblemID) > 512 ||
			strings.TrimSpace(item.StemRaw) == "" || len(item.StemRaw) > 1<<20 ||
			item.QuestionCanonicalMarkdown == "" ||
			len(item.QuestionCanonicalMarkdown) > 1<<20 || len(item.AnswerRaw) > 1<<20 ||
			len(item.AnswerCanonicalMarkdown) > 1<<20 || len(item.Subject) > 512 {
			return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
				"%w: item %d has missing or oversized typed text",
				ErrProblemSourceRecognitionInvalid,
				index,
			)
		}
		if _, duplicate := problemIDs[item.ProblemID]; duplicate {
			return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
				"%w: duplicate problem %q",
				ErrProblemSourceRecognitionInvalid,
				item.ProblemID,
			)
		}
		problemIDs[item.ProblemID] = struct{}{}
		switch item.AnswerState {
		case "present":
			if item.AnswerCanonicalMarkdown == "" {
				return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
					"%w: present answer for %s needs canonical markdown",
					ErrProblemSourceRecognitionInvalid,
					item.ProblemID,
				)
			}
		case "blank":
			if strings.TrimSpace(item.AnswerRaw) != "" ||
				item.AnswerCanonicalMarkdown != "" || raw.AnswerBBox != nil {
				return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
					"%w: blank answer for %s cannot carry answer text or bbox",
					ErrProblemSourceRecognitionInvalid,
					item.ProblemID,
				)
			}
		case "unclear":
			if item.AnswerCanonicalMarkdown != "" {
				return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
					"%w: unclear answer for %s cannot carry canonical answer",
					ErrProblemSourceRecognitionInvalid,
					item.ProblemID,
				)
			}
		default:
			return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
				"%w: answer_state=%q for %s",
				ErrProblemSourceRecognitionInvalid,
				item.AnswerState,
				item.ProblemID,
			)
		}
		bbox, err := normalizeRecognitionBBox(raw.AnswerBBox)
		if err != nil {
			return normalizedProblemSourceRecognitionResult{}, "", err
		}
		item.AnswerBBox = bbox
		if raw.RecognitionConfidence != nil {
			confidence := *raw.RecognitionConfidence
			if math.IsNaN(confidence) || math.IsInf(confidence, 0) ||
				confidence < 0 || confidence > 1 {
				return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
					"%w: recognition confidence for %s must be within 0..1",
					ErrProblemSourceRecognitionInvalid,
					item.ProblemID,
				)
			}
			item.RecognitionConfidence = &confidence
		}
		if item.KnowledgePoints, err = normalizeRecognitionStringSet(
			"knowledge_points", raw.KnowledgePoints,
		); err != nil {
			return normalizedProblemSourceRecognitionResult{}, "", err
		}
		if item.OCRSignals, err = normalizeRecognitionStringSet(
			"ocr_signals", raw.OCRSignals,
		); err != nil {
			return normalizedProblemSourceRecognitionResult{}, "", err
		}
		if item.EvidenceTranscriptions, err = normalizeRecognitionStringSet(
			"evidence_transcriptions", raw.EvidenceTranscriptions,
		); err != nil {
			return normalizedProblemSourceRecognitionResult{}, "", err
		}
		if item.AnswerEvidenceTranscriptions, err = normalizeRecognitionStringSet(
			"answer_evidence_transcriptions", raw.AnswerEvidenceTranscriptions,
		); err != nil {
			return normalizedProblemSourceRecognitionResult{}, "", err
		}
		if item.ConfirmationReasons, err = normalizeRecognitionStringSet(
			"confirmation_reasons", raw.ConfirmationReasons,
		); err != nil {
			return normalizedProblemSourceRecognitionResult{}, "", err
		}
		if item.ConfirmationRequired != (len(item.ConfirmationReasons) > 0) {
			return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
				"%w: confirmation flag/reasons disagree for %s",
				ErrProblemSourceRecognitionInvalid,
				item.ProblemID,
			)
		}
		normalized.Items[index] = item
	}
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return normalizedProblemSourceRecognitionResult{}, "", fmt.Errorf(
			"%w: canonicalize result: %v",
			ErrProblemSourceRecognitionInvalid,
			err,
		)
	}
	digestBytes := sha256.Sum256(canonical)
	return normalized, hex.EncodeToString(digestBytes[:]), nil
}

// ProblemSourceRecognitionTypedResultDigest is the canonical content binding
// written to the recognizing parent invocation. Commit recomputes the same
// value from the typed exact-set, so callers cannot attach invented normalized
// items to otherwise valid physical-call lineage.
func ProblemSourceRecognitionTypedResultDigest(
	input ProblemSourceRecognitionResult,
) (string, error) {
	normalized, _, err := normalizeProblemSourceRecognitionResult(input)
	if err != nil {
		return "", err
	}
	return problemSourceRecognitionTypedDigestFromNormalized(normalized)
}

func problemSourceRecognitionTypedDigestFromNormalized(
	normalized normalizedProblemSourceRecognitionResult,
) (string, error) {
	canonical, err := json.Marshal(struct {
		Contract     string                                   `json:"contract"`
		MappingState ProblemSourceRecognitionMappingState     `json:"mapping_state"`
		Items        []normalizedProblemSourceRecognitionItem `json:"items"`
	}{
		Contract:     "k12.problem_source_recognition.typed_result.v1",
		MappingState: normalized.MappingState,
		Items:        normalized.Items,
	})
	if err != nil {
		return "", fmt.Errorf(
			"%w: canonicalize typed recognition result: %v",
			ErrProblemSourceRecognitionInvalid,
			err,
		)
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func recognitionJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal source recognition evidence: %w", err)
	}
	return string(encoded), nil
}

func decodeRecognitionStringArray(name string, raw string) ([]string, error) {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil || values == nil {
		return nil, fmt.Errorf(
			"%w: stored %s is not a string array",
			ErrProblemSourceRecognitionInvalid,
			name,
		)
	}
	return values, nil
}

func getProblemSourceRecognitionResultVia(
	ctx context.Context,
	queryer dbQueryer,
	ownerScope string,
	workID string,
) (ProblemSourceRecognitionCommit, error) {
	var commit ProblemSourceRecognitionCommit
	var mappingState, affectedJSON string
	rowErr := queryer.QueryRowContext(ctx, `
		SELECT work_id,command_receipt_id,owner_scope,agent_name,submission_id,
		       dispatch_id,job_id,path_problem_id,parent_invocation_id,
		       parent_request_digest,
		       parent_invocation_attempt,action,structure_version,
		       source_input_revision,result_input_revision,result_digest,
		       mapping_state,structure_digest,affected_problem_ids_json,created_at
		FROM k12_problem_source_recognition_results
		WHERE owner_scope=? AND work_id=?`,
		ownerScope,
		workID,
	).Scan(
		&commit.WorkID,
		&commit.CommandReceiptID,
		&commit.OwnerScope,
		&commit.AgentName,
		&commit.SubmissionID,
		&commit.DispatchID,
		&commit.JobID,
		&commit.PathProblemID,
		&commit.ParentInvocationID,
		&commit.ParentRequestDigest,
		&commit.ParentInvocationAttempt,
		&commit.Action,
		&commit.StructureVersion,
		&commit.SourceInputRevision,
		&commit.ResultInputRevision,
		&commit.ResultDigest,
		&mappingState,
		&commit.StructureDigest,
		&affectedJSON,
		&commit.CreatedAt,
	)
	if errors.Is(rowErr, sql.ErrNoRows) {
		return ProblemSourceRecognitionCommit{}, ErrProblemSourceRecognitionNotFound
	}
	if rowErr != nil {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"read problem source recognition result: %w",
			rowErr,
		)
	}
	commit.MappingState = ProblemSourceRecognitionMappingState(mappingState)
	if commit.MappingState != ProblemSourceRecognitionMappingStableExactSet ||
		commit.ResultInputRevision != commit.SourceInputRevision+1 ||
		len(commit.ResultDigest) != 64 {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"%w: stored result identity is inconsistent",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	if err := json.Unmarshal(
		[]byte(affectedJSON),
		&commit.AffectedProblemIDs,
	); err != nil || len(commit.AffectedProblemIDs) == 0 {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"%w: stored affected exact-set is invalid",
			ErrProblemSourceRecognitionInvalid,
		)
	}

	physicalRows, physicalQueryErr := queryer.QueryContext(ctx, `
		SELECT physical_invocation_id,physical_unit,result_digest,
		       recognition_plan_version,plan_digest,candidate_exact_set_digest
		FROM k12_problem_source_recognition_physical_results
		WHERE work_id=? AND parent_invocation_id=?
		ORDER BY ordinal`,
		workID,
		commit.ParentInvocationID,
	)
	if physicalQueryErr != nil {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"read problem source recognition physical lineage: %w",
			physicalQueryErr,
		)
	}
	for physicalRows.Next() {
		var (
			ref         ProblemSourceRecognitionPhysicalResultRef
			planVersion string
		)
		if err := physicalRows.Scan(
			&ref.PhysicalInvocationID,
			&ref.PhysicalUnit,
			&ref.ResultDigest,
			&planVersion,
			&ref.PlanDigest,
			&ref.CandidateExactSetDigest,
		); err != nil {
			_ = physicalRows.Close()
			return ProblemSourceRecognitionCommit{}, err
		}
		parsedPlanVersion, parseErr := parseProblemSourceRecognitionPlanVersion(planVersion)
		if parseErr != nil {
			_ = physicalRows.Close()
			return ProblemSourceRecognitionCommit{}, parseErr
		}
		ref.RecognitionPlanVersion = parsedPlanVersion
		commit.PhysicalResults = append(commit.PhysicalResults, ref)
	}
	if err := physicalRows.Err(); err != nil {
		_ = physicalRows.Close()
		return ProblemSourceRecognitionCommit{}, err
	}
	if err := physicalRows.Close(); err != nil {
		return ProblemSourceRecognitionCommit{}, err
	}
	if len(commit.PhysicalResults) == 0 {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"%w: stored result has no physical recognition lineage",
			ErrProblemSourceRecognitionInvalid,
		)
	}

	itemRows, itemQueryErr := queryer.QueryContext(ctx, `
		SELECT ordinal,problem_id,result_input_revision,input_digest,
		       page_asset_id,source_region_json,source_content_digest,
		       source_media_type,source_size_bytes,source_pixel_width,
		       source_pixel_height,source_orientation_policy,
		       source_orientation_policy_version,source_transform_chain_json,
		       stem_raw,question_canonical_markdown,answer_state,answer_raw,
		       answer_canonical_markdown,answer_bbox_json,subject,
		       knowledge_points_json,recognition_confidence,ocr_signals_json,
		       evidence_transcriptions_json,answer_evidence_transcriptions_json,
		       confirmation_required,confirmation_reasons_json
		FROM k12_problem_source_recognition_items
		WHERE work_id=? AND owner_scope=?
		ORDER BY ordinal`,
		workID,
		ownerScope,
	)
	if itemQueryErr != nil {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"read problem source recognition items: %w",
			itemQueryErr,
		)
	}
	for itemRows.Next() {
		var (
			fact                             ProblemSourceRecognitionFact
			sourceRegion, confidence         sql.NullString
			transformJSON, answerBBoxJSON    string
			knowledgeJSON, signalsJSON       string
			evidenceJSON, answerEvidenceJSON string
			confirmationReasonsJSON          string
			confirmationRequired             int
			orientationPolicy                string
		)
		// Scan REAL through NullString so corrupt NaN/Inf text cannot silently
		// become a zero value. strconv parsing is handled below.
		if err := itemRows.Scan(
			&fact.Ordinal,
			&fact.ProblemID,
			&fact.InputRevision,
			&fact.InputDigest,
			&fact.Source.PageAssetID,
			&sourceRegion,
			&fact.Source.ContentDigest,
			&fact.Source.MediaType,
			&fact.Source.SizeBytes,
			&fact.Source.PixelWidth,
			&fact.Source.PixelHeight,
			&orientationPolicy,
			&fact.Source.OrientationPolicyVersion,
			&transformJSON,
			&fact.StemRaw,
			&fact.QuestionCanonicalMarkdown,
			&fact.AnswerState,
			&fact.AnswerRaw,
			&fact.AnswerCanonicalMarkdown,
			&answerBBoxJSON,
			&fact.Subject,
			&knowledgeJSON,
			&confidence,
			&signalsJSON,
			&evidenceJSON,
			&answerEvidenceJSON,
			&confirmationRequired,
			&confirmationReasonsJSON,
		); err != nil {
			_ = itemRows.Close()
			return ProblemSourceRecognitionCommit{}, err
		}
		fact.Source.OrientationPolicy = PageAssetOrientationPolicy(orientationPolicy)
		fact.Source.TransformChainJSON = append(json.RawMessage(nil), transformJSON...)
		if !json.Valid(fact.Source.TransformChainJSON) {
			_ = itemRows.Close()
			return ProblemSourceRecognitionCommit{}, fmt.Errorf(
				"%w: stored source transform chain is invalid",
				ErrProblemSourceRecognitionInvalid,
			)
		}
		if sourceRegion.Valid {
			var region k12.SourcePixelRegion
			if err := json.Unmarshal([]byte(sourceRegion.String), &region); err != nil {
				_ = itemRows.Close()
				return ProblemSourceRecognitionCommit{}, fmt.Errorf(
					"%w: stored source region is invalid",
					ErrProblemSourceRecognitionInvalid,
				)
			}
			fact.Source.Region = &region
		}
		if answerBBoxJSON != "" {
			var bbox k12.AttemptBBox
			if err := json.Unmarshal([]byte(answerBBoxJSON), &bbox); err != nil {
				_ = itemRows.Close()
				return ProblemSourceRecognitionCommit{}, fmt.Errorf(
					"%w: stored answer bbox is invalid",
					ErrProblemSourceRecognitionInvalid,
				)
			}
			fact.AnswerBBox = &bbox
		}
		if confidence.Valid {
			var value float64
			if _, err := fmt.Sscan(confidence.String, &value); err != nil ||
				math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
				_ = itemRows.Close()
				return ProblemSourceRecognitionCommit{}, fmt.Errorf(
					"%w: stored recognition confidence is invalid",
					ErrProblemSourceRecognitionInvalid,
				)
			}
			fact.RecognitionConfidence = &value
		}
		var decodeErr error
		if fact.KnowledgePoints, decodeErr = decodeRecognitionStringArray(
			"knowledge_points", knowledgeJSON,
		); decodeErr != nil {
			_ = itemRows.Close()
			return ProblemSourceRecognitionCommit{}, decodeErr
		}
		if fact.OCRSignals, decodeErr = decodeRecognitionStringArray(
			"ocr_signals", signalsJSON,
		); decodeErr != nil {
			_ = itemRows.Close()
			return ProblemSourceRecognitionCommit{}, decodeErr
		}
		if fact.EvidenceTranscriptions, decodeErr = decodeRecognitionStringArray(
			"evidence_transcriptions", evidenceJSON,
		); decodeErr != nil {
			_ = itemRows.Close()
			return ProblemSourceRecognitionCommit{}, decodeErr
		}
		if fact.AnswerEvidenceTranscriptions, decodeErr = decodeRecognitionStringArray(
			"answer_evidence_transcriptions", answerEvidenceJSON,
		); decodeErr != nil {
			_ = itemRows.Close()
			return ProblemSourceRecognitionCommit{}, decodeErr
		}
		if fact.ConfirmationReasons, decodeErr = decodeRecognitionStringArray(
			"confirmation_reasons", confirmationReasonsJSON,
		); decodeErr != nil {
			_ = itemRows.Close()
			return ProblemSourceRecognitionCommit{}, decodeErr
		}
		fact.ConfirmationRequired = confirmationRequired == 1
		commit.Items = append(commit.Items, fact)
	}
	if err := itemRows.Err(); err != nil {
		_ = itemRows.Close()
		return ProblemSourceRecognitionCommit{}, err
	}
	if err := itemRows.Close(); err != nil {
		return ProblemSourceRecognitionCommit{}, err
	}
	if len(commit.Items) != len(commit.AffectedProblemIDs) {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"%w: stored recognition item exact-set is incomplete",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	for index := range commit.Items {
		if commit.Items[index].Ordinal != index ||
			commit.Items[index].ProblemID != commit.AffectedProblemIDs[index] ||
			commit.Items[index].InputRevision != commit.ResultInputRevision {
			return ProblemSourceRecognitionCommit{}, fmt.Errorf(
				"%w: stored recognition item order/revision is inconsistent",
				ErrProblemSourceRecognitionInvalid,
			)
		}
	}
	typedItems := make([]ProblemSourceRecognitionItem, len(commit.Items))
	for index := range commit.Items {
		typedItems[index] = commit.Items[index].ProblemSourceRecognitionItem
	}
	_, rebuiltDigest, rebuildErr := normalizeProblemSourceRecognitionResult(
		ProblemSourceRecognitionResult{
			MappingState:       commit.MappingState,
			ParentInvocationID: commit.ParentInvocationID,
			PhysicalResults:    commit.PhysicalResults,
			Items:              typedItems,
		},
	)
	if rebuildErr != nil || rebuiltDigest != commit.ResultDigest {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"%w: stored typed recognition content does not match aggregate digest",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	return commit, nil
}

// GetProblemSourceRecognitionResultByWork rebuilds the complete typed result
// from immutable V73 facts. Owner mismatches intentionally collapse to
// not-found.
func (s *Store) GetProblemSourceRecognitionResultByWork(
	ctx context.Context,
	ownerScope string,
	workID string,
) (ProblemSourceRecognitionCommit, error) {
	if s == nil || s.db == nil || ctx == nil {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"%w: store and context are required",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	ownerScope = strings.TrimSpace(ownerScope)
	workID = strings.TrimSpace(workID)
	if ownerScope == "" || workID == "" {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"%w: owner_scope and work_id are required",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	tx, beginErr := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if beginErr != nil {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"begin problem source recognition read snapshot: %w",
			beginErr,
		)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()
	commit, lookupErr := getProblemSourceRecognitionResultVia(ctx, tx, ownerScope, workID)
	if lookupErr != nil {
		return ProblemSourceRecognitionCommit{}, lookupErr
	}
	if err := tx.Commit(); err != nil {
		return ProblemSourceRecognitionCommit{}, fmt.Errorf(
			"commit problem source recognition read snapshot: %w",
			err,
		)
	}
	return commit, nil
}

type problemSourceRecognitionReceipt struct {
	OwnerScope       string
	AgentName        string
	DispatchID       string
	JobID            string
	PathProblemID    string
	Action           string
	StructureVersion int
	ExpectedRevision int
	ResultRevision   int
	RequestDigest    string
	RequestJSON      string
	AffectedJSON     string
	SubmissionID     string
}

type problemSourceRecognitionMember struct {
	ProblemID     string
	InputRevision int
}

type preparedProblemSourceRecognitionFact struct {
	Item             normalizedProblemSourceRecognitionItem
	InputDigest      string
	Source           ProblemSourceRecognitionSource
	SourceRegionJSON string
}

func decodeProblemSourceRecognitionExactSet(
	raw string,
) ([]string, error) {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil || len(values) == 0 {
		return nil, fmt.Errorf(
			"%w: affected_problem_ids_json must be a non-empty string array",
			ErrProblemSourceRecognitionConflict,
		)
	}
	seen := make(map[string]struct{}, len(values))
	for index := range values {
		values[index] = strings.TrimSpace(values[index])
		if values[index] == "" {
			return nil, fmt.Errorf(
				"%w: affected exact-set contains an empty problem",
				ErrProblemSourceRecognitionConflict,
			)
		}
		if _, duplicate := seen[values[index]]; duplicate {
			return nil, fmt.Errorf(
				"%w: affected exact-set contains duplicate %q",
				ErrProblemSourceRecognitionConflict,
				values[index],
			)
		}
		seen[values[index]] = struct{}{}
	}
	return values, nil
}

func equalProblemSourceRecognitionIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func effectiveProblemSourceRecognitionPlanVersion(version int) int {
	if version == 0 {
		return k12.RecognitionPlanVersionV1
	}
	return version
}

func parseProblemSourceRecognitionPlanVersion(raw string) (int, error) {
	switch strings.TrimSpace(raw) {
	case "v1":
		return k12.RecognitionPlanVersionV1, nil
	case "v2":
		return k12.RecognitionPlanVersionV2, nil
	default:
		return 0, fmt.Errorf(
			"%w: invalid stored recognition plan version %q",
			ErrProblemSourceRecognitionInvalid,
			raw,
		)
	}
}

func validateProblemSourceRecognitionLineage(
	ctx context.Context,
	tx *sql.Tx,
	job ProblemSourceReprocessJob,
	normalized normalizedProblemSourceRecognitionResult,
) (int, []ProblemSourceRecognitionPhysicalResultRef, error) {
	parent, parentErr := scanModelInvocation(tx.QueryRowContext(ctx, `SELECT `+
		modelInvocationColumns+`
		FROM k12_model_invocations
		WHERE invocation_id=?`, normalized.ParentInvocationID))
	if parentErr != nil {
		if errors.Is(parentErr, sql.ErrNoRows) {
			return 0, nil, fmt.Errorf(
				"%w: source recognition parent invocation does not exist",
				ErrProblemSourceRecognitionConflict,
			)
		}
		return 0, nil, parentErr
	}
	terminalSuccess := parent.Status == k12.ModelInvocationSucceeded ||
		(parent.Status == k12.ModelInvocationReconciled &&
			parent.FailureKind == "reconciled_succeeded")
	if parent.AgentName != job.AgentName || parent.JobID != job.JobID ||
		parent.Stage != "recognizing" ||
		!terminalSuccess ||
		parent.Attempt < 1 {
		return 0, nil, fmt.Errorf(
			"%w: parent invocation is not the terminal recognizing call for this work",
			ErrProblemSourceRecognitionConflict,
		)
	}
	wantParentRequestDigest, requestDigestErr := ProblemSourceRecognitionParentRequestDigest(
		job,
		parent.RouteSnapshot,
		parent.RequestPolicySnapshot,
	)
	if requestDigestErr != nil {
		return 0, nil, requestDigestErr
	}
	if parent.RequestDigest != wantParentRequestDigest {
		return 0, nil, fmt.Errorf(
			"%w: recognizing parent request is not bound to this immutable work",
			ErrProblemSourceRecognitionConflict,
		)
	}
	typedResultDigest, typedDigestErr := problemSourceRecognitionTypedDigestFromNormalized(normalized)
	if typedDigestErr != nil {
		return 0, nil, typedDigestErr
	}
	if parent.ResultDigest != typedResultDigest {
		return 0, nil, fmt.Errorf(
			"%w: typed recognition result is not bound to parent result digest",
			ErrProblemSourceRecognitionConflict,
		)
	}
	var latestAttempt int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(attempt),0)
		FROM k12_model_invocations
		WHERE agent_name=? AND job_id=? AND stage='recognizing'`,
		job.AgentName,
		job.JobID,
	).Scan(&latestAttempt); err != nil {
		return 0, nil, err
	}
	if latestAttempt != parent.Attempt {
		return 0, nil, fmt.Errorf(
			"%w: source recognition parent invocation is not the current job attempt",
			ErrProblemSourceRecognitionConflict,
		)
	}

	rows, queryErr := tx.QueryContext(ctx, `
		SELECT physical_invocation_id,parent_invocation_id,agent_name,job_id,
		       stage,physical_unit,status,attempt,result_digest,
		       recognition_plan_version,plan_digest,candidate_exact_set_digest
		FROM k12_model_physical_invocations
		WHERE parent_invocation_id=?
		ORDER BY physical_invocation_id`,
		normalized.ParentInvocationID,
	)
	if queryErr != nil {
		return 0, nil, queryErr
	}
	// 遍历失败时保留主路径错误，延迟关闭仅用于释放游标。
	defer func() { _ = rows.Close() }()
	verified := make([]ProblemSourceRecognitionPhysicalResultRef, 0, len(normalized.PhysicalResults))
	for rows.Next() {
		var (
			ref                                                    ProblemSourceRecognitionPhysicalResultRef
			parentID, agentName, jobID, stage, status, planVersion string
			attempt                                                int
		)
		if err := rows.Scan(
			&ref.PhysicalInvocationID,
			&parentID,
			&agentName,
			&jobID,
			&stage,
			&ref.PhysicalUnit,
			&status,
			&attempt,
			&ref.ResultDigest,
			&planVersion,
			&ref.PlanDigest,
			&ref.CandidateExactSetDigest,
		); err != nil {
			return 0, nil, err
		}
		parsedPlanVersion, parseErr := parseProblemSourceRecognitionPlanVersion(planVersion)
		if parseErr != nil {
			return 0, nil, parseErr
		}
		ref.RecognitionPlanVersion = parsedPlanVersion
		if parentID != normalized.ParentInvocationID || agentName != job.AgentName ||
			jobID != job.JobID || stage != "recognizing" || attempt != 1 {
			return 0, nil, fmt.Errorf(
				"%w: physical recognition child ownership/attempt drifted",
				ErrProblemSourceRecognitionConflict,
			)
		}
		switch status {
		case "succeeded":
			verified = append(verified, ref)
		case "failed", "outcome_unknown", "reconciled":
			// Terminal non-success calls remain in the raw invocation ledger but
			// cannot be evidence inputs to the committed typed result.
		default:
			return 0, nil, fmt.Errorf(
				"%w: physical recognition child %s is not terminal",
				ErrProblemSourceRecognitionConflict,
				ref.PhysicalInvocationID,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if err := rows.Close(); err != nil {
		return 0, nil, err
	}
	if len(verified) != len(normalized.PhysicalResults) {
		return 0, nil, fmt.Errorf(
			"%w: physical recognition lineage is not the exact succeeded set",
			ErrProblemSourceRecognitionConflict,
		)
	}
	for index := range verified {
		provided := normalized.PhysicalResults[index]
		if verified[index].PhysicalInvocationID != provided.PhysicalInvocationID ||
			verified[index].PhysicalUnit != provided.PhysicalUnit ||
			verified[index].RecognitionPlanVersion !=
				effectiveProblemSourceRecognitionPlanVersion(provided.RecognitionPlanVersion) ||
			verified[index].PlanDigest != provided.PlanDigest ||
			verified[index].CandidateExactSetDigest != provided.CandidateExactSetDigest ||
			verified[index].ResultDigest != provided.ResultDigest {
			return 0, nil, fmt.Errorf(
				"%w: physical recognition identity/digest mismatch",
				ErrProblemSourceRecognitionConflict,
			)
		}
	}
	return parent.Attempt, verified, nil
}

func validateProblemSourceRecognitionRegion(
	region *k12.SourcePixelRegion,
	pixelWidth int,
	pixelHeight int,
) error {
	if region == nil {
		return nil
	}
	if region.X < 0 || region.Y < 0 || region.Width <= 0 || region.Height <= 0 ||
		int64(region.X)+int64(region.Width) > int64(pixelWidth) ||
		int64(region.Y)+int64(region.Height) > int64(pixelHeight) {
		return fmt.Errorf(
			"%w: source region falls outside the verified PageAsset",
			ErrProblemSourceRecognitionConflict,
		)
	}
	return nil
}

func publicProblemSourceRecognitionItem(
	item normalizedProblemSourceRecognitionItem,
) ProblemSourceRecognitionItem {
	return ProblemSourceRecognitionItem{
		ProblemID:                    item.ProblemID,
		StemRaw:                      item.StemRaw,
		QuestionCanonicalMarkdown:    item.QuestionCanonicalMarkdown,
		AnswerState:                  item.AnswerState,
		AnswerRaw:                    item.AnswerRaw,
		AnswerCanonicalMarkdown:      item.AnswerCanonicalMarkdown,
		AnswerBBox:                   item.AnswerBBox,
		Subject:                      item.Subject,
		KnowledgePoints:              append([]string(nil), item.KnowledgePoints...),
		RecognitionConfidence:        item.RecognitionConfidence,
		OCRSignals:                   append([]string(nil), item.OCRSignals...),
		EvidenceTranscriptions:       append([]string(nil), item.EvidenceTranscriptions...),
		AnswerEvidenceTranscriptions: append([]string(nil), item.AnswerEvidenceTranscriptions...),
		ConfirmationRequired:         item.ConfirmationRequired,
		ConfirmationReasons:          append([]string(nil), item.ConfirmationReasons...),
	}
}

func loadProblemSourceRecognitionReceipt(
	ctx context.Context,
	tx *sql.Tx,
	job ProblemSourceReprocessJob,
) (problemSourceRecognitionReceipt, []string, error) {
	var receipt problemSourceRecognitionReceipt
	if err := tx.QueryRowContext(ctx, `
		SELECT r.owner_scope,r.agent_name,r.dispatch_id,r.job_id,r.problem_id,
		       r.action,r.structure_version,r.expected_input_revision,
		       r.result_input_revision,r.request_digest,r.request_json,
		       r.affected_problem_ids_json,j.submission_id
		FROM k12_problem_source_action_receipts r
		JOIN k12_grading_jobs j
		  ON j.agent_name=r.agent_name AND j.record_id=r.job_id
		WHERE r.command_receipt_id=?`,
		job.CommandReceiptID,
	).Scan(
		&receipt.OwnerScope,
		&receipt.AgentName,
		&receipt.DispatchID,
		&receipt.JobID,
		&receipt.PathProblemID,
		&receipt.Action,
		&receipt.StructureVersion,
		&receipt.ExpectedRevision,
		&receipt.ResultRevision,
		&receipt.RequestDigest,
		&receipt.RequestJSON,
		&receipt.AffectedJSON,
		&receipt.SubmissionID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return problemSourceRecognitionReceipt{}, nil, fmt.Errorf(
				"%w: source action receipt disappeared",
				ErrProblemSourceRecognitionConflict,
			)
		}
		return problemSourceRecognitionReceipt{}, nil, err
	}
	if receipt.OwnerScope != job.OwnerScope || receipt.AgentName != job.AgentName ||
		receipt.DispatchID != job.DispatchID || receipt.JobID != job.JobID ||
		receipt.PathProblemID != job.ProblemID || receipt.Action != job.Action ||
		receipt.StructureVersion != job.StructureVersion ||
		receipt.ResultRevision != job.InputRevision ||
		receipt.ExpectedRevision+1 != receipt.ResultRevision ||
		receipt.RequestJSON != string(job.RequestJSON) {
		return problemSourceRecognitionReceipt{}, nil, fmt.Errorf(
			"%w: work and immutable source action receipt disagree",
			ErrProblemSourceRecognitionConflict,
		)
	}
	receiptAffected, err := decodeProblemSourceRecognitionExactSet(receipt.AffectedJSON)
	if err != nil {
		return problemSourceRecognitionReceipt{}, nil, err
	}
	if !equalProblemSourceRecognitionIDs(receiptAffected, job.AffectedProblemIDs) {
		return problemSourceRecognitionReceipt{}, nil, fmt.Errorf(
			"%w: work and source action affected exact-sets disagree",
			ErrProblemSourceRecognitionConflict,
		)
	}
	wantWorkDigest := problemSourceInputDigest(
		receipt.RequestDigest,
		strings.Join(job.AffectedProblemIDs, "\x00"),
		job.InputRevision,
	)
	if job.InputDigest != wantWorkDigest {
		return problemSourceRecognitionReceipt{}, nil, fmt.Errorf(
			"%w: source work input digest is not bound to its exact-set",
			ErrProblemSourceRecognitionConflict,
		)
	}
	return receipt, receiptAffected, nil
}

func validateProblemSourceRecognitionStructure(
	ctx context.Context,
	tx *sql.Tx,
	job ProblemSourceReprocessJob,
	receipt problemSourceRecognitionReceipt,
	result normalizedProblemSourceRecognitionResult,
) (string, []problemSourceRecognitionMember, error) {
	var structureDigest, mappingState, disposition string
	if err := tx.QueryRowContext(ctx, `
		SELECT structure_digest,mapping_state,current_disposition
		FROM k12_problem_structure_snapshots
		WHERE agent_name=? AND submission_id=? AND structure_version=?`,
		job.AgentName,
		receipt.SubmissionID,
		job.StructureVersion,
	).Scan(&structureDigest, &mappingState, &disposition); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, fmt.Errorf(
				"%w: source structure snapshot disappeared",
				ErrProblemSourceRecognitionConflict,
			)
		}
		return "", nil, err
	}
	if mappingState != "resolved" || disposition != "current" {
		return "", nil, fmt.Errorf(
			"%w: current structure is %s/%s",
			ErrProblemSourceRecognitionUnstableMapping,
			mappingState,
			disposition,
		)
	}
	var ambiguousMappings int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM k12_problem_structure_mappings
		WHERE agent_name=? AND submission_id=? AND to_structure_version=?
		  AND mapping_kind='ambiguous'`,
		job.AgentName,
		receipt.SubmissionID,
		job.StructureVersion,
	).Scan(&ambiguousMappings); err != nil {
		return "", nil, err
	}
	if ambiguousMappings != 0 {
		return "", nil, fmt.Errorf(
			"%w: current structure contains ambiguous predecessor mappings",
			ErrProblemSourceRecognitionUnstableMapping,
		)
	}
	var dependencyGroupID string
	if err := tx.QueryRowContext(ctx, `
		SELECT dependency_group_id
		FROM k12_problem_structure_members
		WHERE agent_name=? AND submission_id=? AND structure_version=?
		  AND problem_id=?`,
		job.AgentName,
		receipt.SubmissionID,
		job.StructureVersion,
		job.ProblemID,
	).Scan(&dependencyGroupID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, fmt.Errorf(
				"%w: source path problem is not in the current structure",
				ErrProblemSourceRecognitionConflict,
			)
		}
		return "", nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT problem_id,input_revision
		FROM k12_problem_structure_members
		WHERE agent_name=? AND submission_id=? AND structure_version=?
		  AND dependency_group_id=? AND problem_kind!='compound_parent'
		ORDER BY ordinal,problem_id`,
		job.AgentName,
		receipt.SubmissionID,
		job.StructureVersion,
		dependencyGroupID,
	)
	if err != nil {
		return "", nil, err
	}
	// 遍历失败时保留主路径错误，延迟关闭仅用于释放游标。
	defer func() { _ = rows.Close() }()
	members := make([]problemSourceRecognitionMember, 0, len(job.AffectedProblemIDs))
	for rows.Next() {
		var member problemSourceRecognitionMember
		if err := rows.Scan(&member.ProblemID, &member.InputRevision); err != nil {
			return "", nil, err
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if err := rows.Close(); err != nil {
		return "", nil, err
	}
	if len(members) != len(job.AffectedProblemIDs) || len(members) != len(result.Items) {
		return "", nil, fmt.Errorf(
			"%w: dependency group, work and result are not the same exact-set",
			ErrProblemSourceRecognitionConflict,
		)
	}
	for index := range members {
		if members[index].ProblemID != job.AffectedProblemIDs[index] ||
			members[index].ProblemID != result.Items[index].ProblemID ||
			members[index].InputRevision != job.InputRevision {
			return "", nil, fmt.Errorf(
				"%w: dependency group exact-set/order/current revision drifted",
				ErrProblemSourceRecognitionConflict,
			)
		}
	}
	return structureDigest, members, nil
}

func prepareProblemSourceRecognitionFacts(
	ctx context.Context,
	tx *sql.Tx,
	job ProblemSourceReprocessJob,
	receipt problemSourceRecognitionReceipt,
	result normalizedProblemSourceRecognitionResult,
	resultDigest string,
) ([]preparedProblemSourceRecognitionFact, error) {
	prepared := make([]preparedProblemSourceRecognitionFact, len(result.Items))
	for index, item := range result.Items {
		var (
			pageAssetID, inputDigest, originReceiptID, originKind string
			sourceRegion                                          sql.NullString
		)
		if err := tx.QueryRowContext(ctx, `
			SELECT page_asset_id,source_region_json,input_digest,
			       COALESCE(origin_command_receipt_id,''),origin_kind
			FROM k12_problem_input_revisions
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND problem_id=? AND input_revision=?
			  AND current_disposition='current'`,
			job.AgentName,
			receipt.SubmissionID,
			job.StructureVersion,
			item.ProblemID,
			job.InputRevision,
		).Scan(
			&pageAssetID,
			&sourceRegion,
			&inputDigest,
			&originReceiptID,
			&originKind,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf(
					"%w: problem %s no longer has source revision %d as current",
					ErrProblemSourceRecognitionConflict,
					item.ProblemID,
					job.InputRevision,
				)
			}
			return nil, err
		}
		wantSourceDigest := problemSourceInputDigest(
			receipt.RequestDigest,
			item.ProblemID,
			job.InputRevision,
		)
		if inputDigest != wantSourceDigest || originReceiptID != job.CommandReceiptID ||
			originKind != "command" {
			return nil, fmt.Errorf(
				"%w: problem %s source revision is not bound to this command",
				ErrProblemSourceRecognitionConflict,
				item.ProblemID,
			)
		}
		var attemptRevision int
		var attemptDigest string
		if err := tx.QueryRowContext(ctx, `
			SELECT confirmed_version,input_digest
			FROM k12_attempts
			WHERE agent_name=? AND submission_id=? AND problem_id=?`,
			job.AgentName,
			receipt.SubmissionID,
			item.ProblemID,
		).Scan(&attemptRevision, &attemptDigest); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf(
					"%w: answerable problem %s has no attempt",
					ErrProblemSourceRecognitionConflict,
					item.ProblemID,
				)
			}
			return nil, err
		}
		if attemptRevision != job.InputRevision || attemptDigest != inputDigest {
			return nil, fmt.Errorf(
				"%w: problem %s attempt/input head drifted",
				ErrProblemSourceRecognitionConflict,
				item.ProblemID,
			)
		}

		var source ProblemSourceRecognitionSource
		var orientationPolicy, transformJSON, storageState string
		if err := tx.QueryRowContext(ctx, `
			SELECT page_asset_id,content_digest,media_type,size_bytes,pixel_width,
			       pixel_height,orientation_policy,orientation_policy_version,
			       transform_chain_json,storage_state
			FROM k12_page_assets
			WHERE owner_scope=? AND agent_name=? AND page_asset_id=?`,
			job.OwnerScope,
			job.AgentName,
			pageAssetID,
		).Scan(
			&source.PageAssetID,
			&source.ContentDigest,
			&source.MediaType,
			&source.SizeBytes,
			&source.PixelWidth,
			&source.PixelHeight,
			&orientationPolicy,
			&source.OrientationPolicyVersion,
			&transformJSON,
			&storageState,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf(
					"%w: problem %s source PageAsset is unavailable in owner scope",
					ErrProblemSourceRecognitionConflict,
					item.ProblemID,
				)
			}
			return nil, err
		}
		source.OrientationPolicy = PageAssetOrientationPolicy(orientationPolicy)
		source.TransformChainJSON = append(json.RawMessage(nil), transformJSON...)
		var transforms []json.RawMessage
		if storageState != string(PageAssetStorageReady) ||
			source.OrientationPolicy != PageAssetOrientationVerified ||
			json.Unmarshal(source.TransformChainJSON, &transforms) != nil {
			return nil, fmt.Errorf(
				"%w: problem %s PageAsset is not ready with verified orientation",
				ErrProblemSourceRecognitionConflict,
				item.ProblemID,
			)
		}
		regionJSON := ""
		if sourceRegion.Valid {
			var region k12.SourcePixelRegion
			if err := json.Unmarshal([]byte(sourceRegion.String), &region); err != nil {
				return nil, fmt.Errorf(
					"%w: problem %s source region is invalid",
					ErrProblemSourceRecognitionConflict,
					item.ProblemID,
				)
			}
			if err := validateProblemSourceRecognitionRegion(
				&region,
				source.PixelWidth,
				source.PixelHeight,
			); err != nil {
				return nil, err
			}
			source.Region = &region
			regionJSON, _ = recognitionJSON(region)
		}
		if job.Action == "select_region" && source.Region == nil {
			return nil, fmt.Errorf(
				"%w: select_region result for %s has no verified source region",
				ErrProblemSourceRecognitionConflict,
				item.ProblemID,
			)
		}
		prepared[index] = preparedProblemSourceRecognitionFact{
			Item:             item,
			InputDigest:      problemSourceInputDigest(resultDigest, item.ProblemID, job.InputRevision+1),
			Source:           source,
			SourceRegionJSON: regionJSON,
		}
	}
	return prepared, nil
}

func recognitionNullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func recognitionNullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func recognitionBool(value bool) int {
	if value {
		return 1
	}
	return 0
}

// CommitProblemSourceRecognitionResult atomically validates the current queue
// fence and recognition lineage, freezes typed evidence, supersedes every v2
// source head and appends the same v3 revision across the affected exact-set.
// It updates only Attempt.confirmed_version/input_digest; V19 Problem/Attempt
// OCR, answer and bbox facts are deliberately never overwritten.
func (s *Store) CommitProblemSourceRecognitionResult(
	ctx context.Context,
	lease ProblemSourceReprocessLease,
	input ProblemSourceRecognitionResult,
) (ProblemSourceRecognitionCommit, bool, error) {
	if s == nil || s.db == nil || ctx == nil {
		return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
			"%w: store and context are required",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	normalizedLease, leaseErr := normalizeProblemSourceReprocessLease(lease)
	if leaseErr != nil {
		return ProblemSourceRecognitionCommit{}, false, leaseErr
	}
	lease = normalizedLease
	normalized, resultDigest, normalizeErr := normalizeProblemSourceRecognitionResult(input)
	if normalizeErr != nil {
		return ProblemSourceRecognitionCommit{}, false, normalizeErr
	}

	tx, beginErr := s.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
			"begin problem source recognition commit: %w",
			beginErr,
		)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()

	// Make the first transaction statement a real write to the target row.
	// SQLite therefore serializes competing commit/replay attempts before any
	// mutable-head read, avoiding deferred read->write upgrade races.
	reserved, reserveErr := tx.ExecContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET updated_at=updated_at
		WHERE work_id=?`,
		lease.WorkID,
	)
	if reserveErr != nil {
		return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
			"reserve problem source recognition work: %w",
			reserveErr,
		)
	}
	reservedRows, reservedRowsErr := reserved.RowsAffected()
	if reservedRowsErr != nil {
		return ProblemSourceRecognitionCommit{}, false, reservedRowsErr
	}
	if reservedRows != 1 {
		return ProblemSourceRecognitionCommit{}, false, ErrProblemSourceReprocessFenced
	}
	job, jobErr := scanProblemSourceReprocessJob(tx.QueryRowContext(ctx, `SELECT `+
		problemSourceReprocessColumns+`
		FROM k12_problem_source_reprocess_jobs
		WHERE work_id=?`, lease.WorkID))
	if jobErr != nil {
		if errors.Is(jobErr, sql.ErrNoRows) {
			return ProblemSourceRecognitionCommit{}, false, ErrProblemSourceReprocessFenced
		}
		return ProblemSourceRecognitionCommit{}, false, jobErr
	}
	if job.Status != ProblemSourceReprocessRunning ||
		job.LeaseOwner != lease.LeaseOwner || job.LeaseEpoch != lease.LeaseEpoch ||
		job.LeaseExpiresAtMilli <= time.Now().UTC().UnixMilli() {
		return ProblemSourceRecognitionCommit{}, false, ErrProblemSourceReprocessFenced
	}
	if job.Action != "select_region" && job.Action != "retake" {
		return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
			"%w: work action %q does not produce recognition revisions",
			ErrProblemSourceRecognitionInvalid,
			job.Action,
		)
	}

	var existingDigest, existingOwner string
	existingErr := tx.QueryRowContext(ctx, `
		SELECT result_digest,owner_scope
		FROM k12_problem_source_recognition_results
		WHERE work_id=?`,
		job.WorkID,
	).Scan(&existingDigest, &existingOwner)
	switch {
	case existingErr == nil:
		if existingOwner != job.OwnerScope || existingDigest != resultDigest {
			return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
				"%w: work already committed a different immutable result",
				ErrProblemSourceRecognitionConflict,
			)
		}
		replay, replayErr := getProblemSourceRecognitionResultVia(
			ctx,
			tx,
			job.OwnerScope,
			job.WorkID,
		)
		if replayErr != nil {
			return ProblemSourceRecognitionCommit{}, false, replayErr
		}
		return replay, false, nil
	case !errors.Is(existingErr, sql.ErrNoRows):
		return ProblemSourceRecognitionCommit{}, false, existingErr
	}

	receipt, _, receiptErr := loadProblemSourceRecognitionReceipt(ctx, tx, job)
	if receiptErr != nil {
		return ProblemSourceRecognitionCommit{}, false, receiptErr
	}
	structureDigest, _, structureErr := validateProblemSourceRecognitionStructure(
		ctx,
		tx,
		job,
		receipt,
		normalized,
	)
	if structureErr != nil {
		return ProblemSourceRecognitionCommit{}, false, structureErr
	}
	parentAttempt, physicalResults, lineageErr := validateProblemSourceRecognitionLineage(
		ctx,
		tx,
		job,
		normalized,
	)
	if lineageErr != nil {
		return ProblemSourceRecognitionCommit{}, false, lineageErr
	}
	var parentRequestDigest string
	if err := tx.QueryRowContext(ctx, `
		SELECT request_digest FROM k12_model_invocations
		WHERE invocation_id=?`, normalized.ParentInvocationID,
	).Scan(&parentRequestDigest); err != nil {
		return ProblemSourceRecognitionCommit{}, false, err
	}
	prepared, prepareErr := prepareProblemSourceRecognitionFacts(
		ctx,
		tx,
		job,
		receipt,
		normalized,
		resultDigest,
	)
	if prepareErr != nil {
		return ProblemSourceRecognitionCommit{}, false, prepareErr
	}
	createdAt := nowUnix()
	if createdAt <= 0 {
		return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
			"%w: positive commit time is required",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	affectedJSON, affectedErr := recognitionJSON(job.AffectedProblemIDs)
	if affectedErr != nil {
		return ProblemSourceRecognitionCommit{}, false, affectedErr
	}
	resultRevision := job.InputRevision + 1
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO k12_problem_source_recognition_results (
			work_id,command_receipt_id,owner_scope,agent_name,submission_id,
			dispatch_id,job_id,path_problem_id,parent_invocation_id,
			parent_request_digest,
			parent_invocation_attempt,action,structure_version,
			source_input_revision,result_input_revision,result_digest,
			mapping_state,structure_digest,affected_problem_ids_json,created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.WorkID,
		job.CommandReceiptID,
		job.OwnerScope,
		job.AgentName,
		receipt.SubmissionID,
		job.DispatchID,
		job.JobID,
		job.ProblemID,
		normalized.ParentInvocationID,
		parentRequestDigest,
		parentAttempt,
		job.Action,
		job.StructureVersion,
		job.InputRevision,
		resultRevision,
		resultDigest,
		string(normalized.MappingState),
		structureDigest,
		affectedJSON,
		createdAt,
	); err != nil {
		return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
			"insert problem source recognition aggregate: %w",
			err,
		)
	}
	for ordinal, ref := range physicalResults {
		planVersion, planVersionErr := recognitionPlanVersionSQL(ref.RecognitionPlanVersion)
		if planVersionErr != nil {
			return ProblemSourceRecognitionCommit{}, false, planVersionErr
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO k12_problem_source_recognition_physical_results (
				work_id,ordinal,parent_invocation_id,physical_invocation_id,
				physical_unit,result_digest,created_at,recognition_plan_version,
				plan_digest,candidate_exact_set_digest
			) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			job.WorkID,
			ordinal,
			normalized.ParentInvocationID,
			ref.PhysicalInvocationID,
			ref.PhysicalUnit,
			ref.ResultDigest,
			createdAt,
			planVersion,
			ref.PlanDigest,
			ref.CandidateExactSetDigest,
		); err != nil {
			return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
				"insert problem source recognition physical lineage: %w",
				err,
			)
		}
	}

	commit := ProblemSourceRecognitionCommit{
		WorkID:                  job.WorkID,
		CommandReceiptID:        job.CommandReceiptID,
		OwnerScope:              job.OwnerScope,
		AgentName:               job.AgentName,
		SubmissionID:            receipt.SubmissionID,
		DispatchID:              job.DispatchID,
		JobID:                   job.JobID,
		PathProblemID:           job.ProblemID,
		ParentInvocationID:      normalized.ParentInvocationID,
		ParentRequestDigest:     parentRequestDigest,
		ParentInvocationAttempt: parentAttempt,
		PhysicalResults:         append([]ProblemSourceRecognitionPhysicalResultRef(nil), physicalResults...),
		Action:                  job.Action,
		StructureVersion:        job.StructureVersion,
		SourceInputRevision:     job.InputRevision,
		ResultInputRevision:     resultRevision,
		ResultDigest:            resultDigest,
		MappingState:            normalized.MappingState,
		StructureDigest:         structureDigest,
		AffectedProblemIDs:      append([]string(nil), job.AffectedProblemIDs...),
		Items:                   make([]ProblemSourceRecognitionFact, len(prepared)),
		CreatedAt:               createdAt,
	}
	for ordinal, fact := range prepared {
		bboxJSON := ""
		if fact.Item.AnswerBBox != nil {
			var bboxErr error
			bboxJSON, bboxErr = recognitionJSON(fact.Item.AnswerBBox)
			if bboxErr != nil {
				return ProblemSourceRecognitionCommit{}, false, bboxErr
			}
		}
		knowledgeJSON, _ := recognitionJSON(fact.Item.KnowledgePoints)
		signalsJSON, _ := recognitionJSON(fact.Item.OCRSignals)
		evidenceJSON, _ := recognitionJSON(fact.Item.EvidenceTranscriptions)
		answerEvidenceJSON, _ := recognitionJSON(fact.Item.AnswerEvidenceTranscriptions)
		confirmationReasonsJSON, _ := recognitionJSON(fact.Item.ConfirmationReasons)

		superseded, supersedeErr := tx.ExecContext(ctx, `
			UPDATE k12_problem_input_revisions
			SET current_disposition='superseded',updated_at=?
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND problem_id=? AND input_revision=?
			  AND current_disposition='current'`,
			createdAt,
			job.AgentName,
			receipt.SubmissionID,
			job.StructureVersion,
			fact.Item.ProblemID,
			job.InputRevision,
		)
		if supersedeErr != nil {
			return ProblemSourceRecognitionCommit{}, false, supersedeErr
		}
		affectedRows, affectedRowsErr := superseded.RowsAffected()
		if affectedRowsErr != nil || affectedRows != 1 {
			return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
				"%w: input revision CAS lost for problem %s",
				ErrProblemSourceRecognitionConflict,
				fact.Item.ProblemID,
			)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO k12_problem_input_revisions (
				agent_name,submission_id,structure_version,problem_id,input_revision,
				page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
				question_canonical_markdown,answer_canonical_markdown,input_digest,
				current_disposition,origin_command_receipt_id,origin_kind,
				created_at,updated_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,'current',?,'command',?,?)`,
			job.AgentName,
			receipt.SubmissionID,
			job.StructureVersion,
			fact.Item.ProblemID,
			resultRevision,
			fact.Source.PageAssetID,
			recognitionNullableString(fact.SourceRegionJSON),
			fact.Item.StemRaw,
			fact.Item.AnswerRaw,
			bboxJSON,
			fact.Item.QuestionCanonicalMarkdown,
			fact.Item.AnswerCanonicalMarkdown,
			fact.InputDigest,
			job.CommandReceiptID,
			createdAt,
			createdAt,
		); err != nil {
			return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
				"append problem source recognition input revision: %w",
				err,
			)
		}
		memberUpdated, memberUpdateErr := tx.ExecContext(ctx, `
			UPDATE k12_problem_structure_members
			SET input_revision=?
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND problem_id=? AND input_revision=?`,
			resultRevision,
			job.AgentName,
			receipt.SubmissionID,
			job.StructureVersion,
			fact.Item.ProblemID,
			job.InputRevision,
		)
		if memberUpdateErr != nil {
			return ProblemSourceRecognitionCommit{}, false, memberUpdateErr
		}
		affectedRows, affectedRowsErr = memberUpdated.RowsAffected()
		if affectedRowsErr != nil || affectedRows != 1 {
			return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
				"%w: structure member CAS lost for problem %s",
				ErrProblemSourceRecognitionConflict,
				fact.Item.ProblemID,
			)
		}
		attemptUpdated, attemptUpdateErr := tx.ExecContext(ctx, `
			UPDATE k12_attempts
			SET confirmed_version=?,input_digest=?,updated_at=?
			WHERE agent_name=? AND submission_id=? AND problem_id=?
			  AND confirmed_version=? AND input_digest=?`,
			resultRevision,
			fact.InputDigest,
			createdAt,
			job.AgentName,
			receipt.SubmissionID,
			fact.Item.ProblemID,
			job.InputRevision,
			problemSourceInputDigest(
				receipt.RequestDigest,
				fact.Item.ProblemID,
				job.InputRevision,
			),
		)
		if attemptUpdateErr != nil {
			return ProblemSourceRecognitionCommit{}, false, attemptUpdateErr
		}
		affectedRows, affectedRowsErr = attemptUpdated.RowsAffected()
		if affectedRowsErr != nil || affectedRows != 1 {
			return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
				"%w: attempt input CAS lost for problem %s",
				ErrProblemSourceRecognitionConflict,
				fact.Item.ProblemID,
			)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO k12_problem_source_recognition_items (
				work_id,ordinal,owner_scope,agent_name,submission_id,
				structure_version,problem_id,source_input_revision,
				result_input_revision,input_digest,page_asset_id,source_region_json,
				source_content_digest,source_media_type,source_size_bytes,
				source_pixel_width,source_pixel_height,source_orientation_policy,
				source_orientation_policy_version,source_transform_chain_json,
				stem_raw,question_canonical_markdown,answer_state,answer_raw,
				answer_canonical_markdown,answer_bbox_json,subject,
				knowledge_points_json,recognition_confidence,ocr_signals_json,
				evidence_transcriptions_json,answer_evidence_transcriptions_json,
				confirmation_required,confirmation_reasons_json,created_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			job.WorkID,
			ordinal,
			job.OwnerScope,
			job.AgentName,
			receipt.SubmissionID,
			job.StructureVersion,
			fact.Item.ProblemID,
			job.InputRevision,
			resultRevision,
			fact.InputDigest,
			fact.Source.PageAssetID,
			recognitionNullableString(fact.SourceRegionJSON),
			fact.Source.ContentDigest,
			fact.Source.MediaType,
			fact.Source.SizeBytes,
			fact.Source.PixelWidth,
			fact.Source.PixelHeight,
			string(fact.Source.OrientationPolicy),
			fact.Source.OrientationPolicyVersion,
			string(fact.Source.TransformChainJSON),
			fact.Item.StemRaw,
			fact.Item.QuestionCanonicalMarkdown,
			fact.Item.AnswerState,
			fact.Item.AnswerRaw,
			fact.Item.AnswerCanonicalMarkdown,
			bboxJSON,
			fact.Item.Subject,
			knowledgeJSON,
			recognitionNullableFloat(fact.Item.RecognitionConfidence),
			signalsJSON,
			evidenceJSON,
			answerEvidenceJSON,
			recognitionBool(fact.Item.ConfirmationRequired),
			confirmationReasonsJSON,
			createdAt,
		); err != nil {
			return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
				"insert typed problem source recognition fact: %w",
				err,
			)
		}
		commit.Items[ordinal] = ProblemSourceRecognitionFact{
			ProblemSourceRecognitionItem: publicProblemSourceRecognitionItem(fact.Item),
			Ordinal:                      ordinal,
			InputRevision:                resultRevision,
			InputDigest:                  fact.InputDigest,
			Source:                       fact.Source,
		}
	}

	// The lease must still be live at publication time. A long transaction may
	// have crossed its deadline even though no competing owner could write while
	// SQLite held the reservation.
	finalFence, fenceErr := tx.ExecContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET updated_at=updated_at
		WHERE work_id=? AND status='running' AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?`,
		lease.WorkID,
		lease.LeaseOwner,
		lease.LeaseEpoch,
		time.Now().UTC().UnixMilli(),
	)
	if fenceErr != nil {
		return ProblemSourceRecognitionCommit{}, false, fenceErr
	}
	fenceRows, fenceRowsErr := finalFence.RowsAffected()
	if fenceRowsErr != nil || fenceRows != 1 {
		return ProblemSourceRecognitionCommit{}, false, ErrProblemSourceReprocessFenced
	}
	if err := tx.Commit(); err != nil {
		return ProblemSourceRecognitionCommit{}, false, fmt.Errorf(
			"commit problem source recognition result: %w",
			err,
		)
	}
	return commit, true, nil
}

// ListCurrentProblemSourceRecognitionFacts is the only current typed overlay
// for V73 recognition results. It returns no V19 fallback: callers that need
// answer state, subject, KP, confidence, OCR/evidence or confirmation risk must
// either consume Commit.Items or this projection, so an appended v3 can never
// be evaluated with the stale V19 answer/risk facts intentionally preserved
// for audit.
func (s *Store) ListCurrentProblemSourceRecognitionFacts(
	ctx context.Context,
	agentName string,
	submissionID string,
) (map[string]ProblemSourceRecognitionFact, error) {
	if s == nil || s.db == nil || ctx == nil {
		return nil, fmt.Errorf(
			"%w: store and context are required",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	agentName = strings.TrimSpace(agentName)
	submissionID = strings.TrimSpace(submissionID)
	if agentName == "" || submissionID == "" {
		return nil, fmt.Errorf(
			"%w: agent and submission are required",
			ErrProblemSourceRecognitionInvalid,
		)
	}
	tx, beginErr := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if beginErr != nil {
		return nil, fmt.Errorf("begin current V73 recognition snapshot: %w", beginErr)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()
	rows, queryErr := tx.QueryContext(ctx, `
		SELECT i.problem_id,r.owner_scope,r.work_id
		FROM k12_problem_source_recognition_items i
		JOIN k12_problem_source_recognition_results r
		  ON r.work_id=i.work_id
		JOIN k12_problem_structure_snapshots ss
		  ON ss.agent_name=i.agent_name
		 AND ss.submission_id=i.submission_id
		 AND ss.structure_version=i.structure_version
		 AND ss.current_disposition='current'
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=i.agent_name
		 AND sm.submission_id=i.submission_id
		 AND sm.structure_version=i.structure_version
		 AND sm.problem_id=i.problem_id
		 AND sm.input_revision=i.result_input_revision
		JOIN k12_problem_input_revisions ir
		  ON ir.agent_name=i.agent_name
		 AND ir.submission_id=i.submission_id
		 AND ir.structure_version=i.structure_version
		 AND ir.problem_id=i.problem_id
		 AND ir.input_revision=i.result_input_revision
		 AND ir.input_digest=i.input_digest
		 AND ir.current_disposition='current'
		WHERE i.agent_name=? AND i.submission_id=?
		ORDER BY r.created_at,r.work_id,i.ordinal`,
		agentName,
		submissionID,
	)
	if queryErr != nil {
		return nil, fmt.Errorf("list current V73 recognition identities: %w", queryErr)
	}
	type currentRecognitionWork struct {
		ownerScope string
		workID     string
		problemIDs []string
	}
	works := make([]currentRecognitionWork, 0)
	workIndex := make(map[string]int)
	seenProblems := make(map[string]struct{})
	for rows.Next() {
		var problemID, ownerScope, workID string
		if err := rows.Scan(&problemID, &ownerScope, &workID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if _, duplicate := seenProblems[problemID]; duplicate {
			_ = rows.Close()
			return nil, fmt.Errorf(
				"%w: multiple V73 facts claim current problem %s",
				ErrProblemSourceRecognitionInvalid,
				problemID,
			)
		}
		seenProblems[problemID] = struct{}{}
		index, exists := workIndex[workID]
		if !exists {
			index = len(works)
			workIndex[workID] = index
			works = append(works, currentRecognitionWork{
				ownerScope: ownerScope,
				workID:     workID,
			})
		} else if works[index].ownerScope != ownerScope {
			_ = rows.Close()
			return nil, fmt.Errorf(
				"%w: V73 work owner drifted",
				ErrProblemSourceRecognitionInvalid,
			)
		}
		works[index].problemIDs = append(works[index].problemIDs, problemID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate current V73 recognition identities: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	out := make(map[string]ProblemSourceRecognitionFact, len(seenProblems))
	for _, work := range works {
		commit, err := getProblemSourceRecognitionResultVia(
			ctx,
			tx,
			work.ownerScope,
			work.workID,
		)
		if err != nil {
			return nil, err
		}
		wanted := make(map[string]struct{}, len(work.problemIDs))
		for _, problemID := range work.problemIDs {
			wanted[problemID] = struct{}{}
		}
		for _, fact := range commit.Items {
			if _, current := wanted[fact.ProblemID]; !current {
				continue
			}
			if fact.InputRevision != commit.ResultInputRevision {
				return nil, fmt.Errorf(
					"%w: current V73 fact revision drifted for problem %s",
					ErrProblemSourceRecognitionInvalid,
					fact.ProblemID,
				)
			}
			out[fact.ProblemID] = fact
			delete(wanted, fact.ProblemID)
		}
		if len(wanted) != 0 {
			return nil, fmt.Errorf(
				"%w: current V73 result is missing typed facts",
				ErrProblemSourceRecognitionInvalid,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit current V73 recognition snapshot: %w", err)
	}
	return out, nil
}
