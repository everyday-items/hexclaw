package k12

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ModelPhysicalInvocation is the immutable child receipt for one actual
// structured recognition provider request. It stores digests and frozen
// control-plane facts only; prompt text, image bytes, and raw responses are
// deliberately excluded.
type ModelPhysicalInvocation struct {
	PhysicalInvocationID    string                     `json:"physical_invocation_id"`
	ParentInvocationID      string                     `json:"parent_invocation_id"`
	AgentName               string                     `json:"agent_name"`
	JobID                   string                     `json:"job_id"`
	Stage                   string                     `json:"stage"`
	PhysicalUnit            RecognitionPhysicalUnit    `json:"physical_unit"`
	RecognitionPlanVersion  int                        `json:"recognition_plan_version"`
	PlanDigest              string                     `json:"plan_digest,omitempty"`
	CandidateExactSetDigest string                     `json:"candidate_exact_set_digest,omitempty"`
	RequestDigest           string                     `json:"request_digest"`
	RouteSnapshot           GradingModelSnapshot       `json:"route_snapshot"`
	RequestPolicySnapshot   ModelRequestPolicySnapshot `json:"request_policy_snapshot,omitzero"`
	Status                  ModelInvocationStatus      `json:"status"`
	Attempt                 int                        `json:"attempt"`
	ResultDigest            string                     `json:"result_digest,omitempty"`
	ExternalRequestID       string                     `json:"external_request_id,omitempty"`
	FailureKind             string                     `json:"failure_kind,omitempty"`
	CreatedAt               int64                      `json:"created_at"`
	UpdatedAt               int64                      `json:"updated_at"`
}

// RecognitionLayoutPlanHeaderV2 冻结发送紧凑清单前已存在的控制面事实。
// 它有意不包含图像、裁剪、提示词或 Provider 响应内容。
type RecognitionLayoutPlanHeaderV2 struct {
	PlanID                   string                           `json:"plan_id"`
	ParentInvocationID       string                           `json:"parent_invocation_id"`
	AgentName                string                           `json:"agent_name"`
	JobID                    string                           `json:"job_id"`
	PageDigest               string                           `json:"page_digest"`
	ParentRequestDigest      string                           `json:"parent_request_digest"`
	RouteSnapshot            GradingModelSnapshot             `json:"route_snapshot"`
	RequestPolicySnapshot    ModelRequestPolicySnapshot       `json:"request_policy_snapshot"`
	StageStartedAtUnixMillis int64                            `json:"stage_started_at_unix_millis"`
	PhysicalCallCapMillis    int64                            `json:"physical_call_cap_millis"`
	BudgetBuckets            RecognitionLayoutBudgetBucketsV2 `json:"budget_buckets"`
	AdapterWorkerHardCap     int                              `json:"adapter_worker_hard_cap"`
	EffectiveConcurrency     int                              `json:"effective_concurrency"`
}

// RecognitionLayoutBudgetBucketsV2 是不可变的四档识别预算快照。
// 各值均为发布策略提供的显式毫秒数；本领域契约不会自行设定发布默认值。
type RecognitionLayoutBudgetBucketsV2 struct {
	UpTo1ProblemMillis   int64 `json:"up_to_1_problem_millis"`
	UpTo8ProblemsMillis  int64 `json:"up_to_8_problems_millis"`
	UpTo16ProblemsMillis int64 `json:"up_to_16_problems_millis"`
	UpTo32ProblemsMillis int64 `json:"up_to_32_problems_millis"`
}

// RecognitionLayoutPlanRuntimeV2 是可安全重启的公共控制面投影。
// 它公开不可变摘要、选定预算和已授权计划，但绝不公开源图像、裁剪、
// 提示词或 Provider 结果内容。
type RecognitionLayoutPlanRuntimeV2 struct {
	Header                       RecognitionLayoutPlanHeaderV2 `json:"header"`
	HeaderDigest                 string                        `json:"header_digest"`
	ManifestPhysicalInvocationID string                        `json:"manifest_physical_invocation_id"`
	ManifestResultDigest         string                        `json:"manifest_result_digest,omitempty"`
	CandidateExactSetDigest      string                        `json:"candidate_exact_set_digest,omitempty"`
	SelectedBucketMaxProblems    int                           `json:"selected_bucket_max_problems,omitempty"`
	StageDeadlineAtUnixMillis    int64                         `json:"stage_deadline_at_unix_millis,omitempty"`
	Status                       string                        `json:"status"`
	AuthorizedPlan               *RecognitionLayoutPlanV2      `json:"authorized_plan,omitempty"`
}

type RecognitionLayoutBatchClassificationV2 string

const (
	RecognitionLayoutBatchClassifiedV2        RecognitionLayoutBatchClassificationV2 = "classified"
	RecognitionLayoutBatchTerminalAmbiguousV2 RecognitionLayoutBatchClassificationV2 = "terminal_ambiguous"
)

type RecognitionLayoutCandidateClassificationV2 string

const (
	RecognitionLayoutCandidateValidV2   RecognitionLayoutCandidateClassificationV2 = "valid"
	RecognitionLayoutCandidateMissingV2 RecognitionLayoutCandidateClassificationV2 = "missing"
	RecognitionLayoutCandidateInvalidV2 RecognitionLayoutCandidateClassificationV2 = "invalid"
)

type RecognitionLayoutCandidateResultKindV2 string

const (
	RecognitionLayoutCandidateQuestionV2    RecognitionLayoutCandidateResultKindV2 = "question"
	RecognitionLayoutCandidateNonQuestionV2 RecognitionLayoutCandidateResultKindV2 = "non_question"
)

type RecognitionLayoutBatchAmbiguityKindV2 string

const (
	RecognitionLayoutAmbiguityExtraCandidateV2     RecognitionLayoutBatchAmbiguityKindV2 = "extra_candidate"
	RecognitionLayoutAmbiguityDuplicateCandidateV2 RecognitionLayoutBatchAmbiguityKindV2 = "duplicate_candidate"
	RecognitionLayoutAmbiguitySourceConflictV2     RecognitionLayoutBatchAmbiguityKindV2 = "source_conflict"
	RecognitionLayoutAmbiguityUnattributableV2     RecognitionLayoutBatchAmbiguityKindV2 = "unattributable"
)

// RecognitionLayoutCandidateSettlementV2 是解析器对恰好一个已授权主批次成员
// 作出的分类。有效成员携带一个规范的类型化结果对象；缺失或无效成员不携带结果。
type RecognitionLayoutCandidateSettlementV2 struct {
	CandidateID    string                                     `json:"candidate_id"`
	Classification RecognitionLayoutCandidateClassificationV2 `json:"classification"`
	ResultKind     RecognitionLayoutCandidateResultKindV2     `json:"result_kind,omitempty"`
	ResultJSON     json.RawMessage                            `json:"result_json,omitempty"`
}

// RecognitionLayoutPrimaryBatchSettlementV2 将调用方分类绑定到一个确定的成功主子调用。
// Store 实现必须重新校验私有子调用内容，并自行计算每个持久化摘要。
type RecognitionLayoutPrimaryBatchSettlementV2 struct {
	PlanDigest                 string                                   `json:"plan_digest"`
	SourcePhysicalInvocationID string                                   `json:"source_physical_invocation_id"`
	SourcePhysicalUnit         RecognitionPhysicalUnit                  `json:"source_physical_unit"`
	SourcePhysicalResultDigest string                                   `json:"source_physical_result_digest"`
	Classification             RecognitionLayoutBatchClassificationV2   `json:"classification"`
	AmbiguityKind              RecognitionLayoutBatchAmbiguityKindV2    `json:"ambiguity_kind,omitempty"`
	Candidates                 []RecognitionLayoutCandidateSettlementV2 `json:"candidates,omitempty"`
}

type RecognitionLayoutCandidateResultReceiptV2 struct {
	CandidateID  string                                 `json:"candidate_id"`
	ResultKind   RecognitionLayoutCandidateResultKindV2 `json:"result_kind"`
	ResultDigest string                                 `json:"result_digest"`
}

type RecognitionLayoutRepairAuthorizationV2 struct {
	AuthorizationID     string                  `json:"authorization_id"`
	AuthorizationDigest string                  `json:"authorization_digest"`
	CandidateID         string                  `json:"candidate_id"`
	PhysicalUnit        RecognitionPhysicalUnit `json:"physical_unit"`
	RepairRound         int                     `json:"repair_round"`
}

type RecognitionLayoutPrimaryBatchSettlementResultV2 struct {
	Classification         RecognitionLayoutBatchClassificationV2      `json:"classification"`
	SettlementDigest       string                                      `json:"settlement_digest"`
	FrozenResults          []RecognitionLayoutCandidateResultReceiptV2 `json:"frozen_results,omitempty"`
	RepairAuthorizations   []RecognitionLayoutRepairAuthorizationV2    `json:"repair_authorizations,omitempty"`
	UnresolvedCandidateIDs []string                                    `json:"unresolved_candidate_ids,omitempty"`
}

// RecognitionLayoutRepairSettlementV2 将解析器作出的单项分类绑定到确定成功的
// 第一轮修复子调用及其不可变授权。有效结果携带规范 JSON；无效结果为终态，
// 不携带结果载荷。
type RecognitionLayoutRepairSettlementV2 struct {
	PlanDigest                 string                                     `json:"plan_digest"`
	AuthorizationID            string                                     `json:"authorization_id"`
	AuthorizationDigest        string                                     `json:"authorization_digest"`
	CandidateID                string                                     `json:"candidate_id"`
	SourcePhysicalInvocationID string                                     `json:"source_physical_invocation_id"`
	SourcePhysicalUnit         RecognitionPhysicalUnit                    `json:"source_physical_unit"`
	SourcePhysicalResultDigest string                                     `json:"source_physical_result_digest"`
	Classification             RecognitionLayoutCandidateClassificationV2 `json:"classification"`
	ResultKind                 RecognitionLayoutCandidateResultKindV2     `json:"result_kind,omitempty"`
	ResultJSON                 json.RawMessage                            `json:"result_json,omitempty"`
}

// RecognitionLayoutRepairSettlementResultV2 是单项修复回执的持久投影。
// FrozenResult 与 UnresolvedCandidateID 中恰好一个有值。
type RecognitionLayoutRepairSettlementResultV2 struct {
	Classification        RecognitionLayoutCandidateClassificationV2 `json:"classification"`
	SettlementDigest      string                                     `json:"settlement_digest"`
	FrozenResult          *RecognitionLayoutCandidateResultReceiptV2 `json:"frozen_result,omitempty"`
	UnresolvedCandidateID string                                     `json:"unresolved_candidate_id,omitempty"`
}

// RecognitionLayoutCandidateFinalResultV2 是按计划顺序排列的私有结果投影，
// 仅在 Store 所有的精确集合完成最终化后返回。Provider 响应仍为私有内容；
// 此对象是已为候选项及其不可变来源回执冻结的规范类型化结果。
type RecognitionLayoutCandidateFinalResultV2 struct {
	CandidateID                string                                 `json:"candidate_id"`
	ResultKind                 RecognitionLayoutCandidateResultKindV2 `json:"result_kind"`
	ResultDigest               string                                 `json:"result_digest"`
	ResultJSON                 json.RawMessage                        `json:"result_json"`
	SourcePhysicalInvocationID string                                 `json:"source_physical_invocation_id"`
	SourcePhysicalUnit         RecognitionPhysicalUnit                `json:"source_physical_unit"`
	SourcePhysicalResultDigest string                                 `json:"source_physical_result_digest"`
}

// RecognitionLayoutPhysicalResultEvidenceV2 是最终化精确集合中单次物理调用的
// 稳定、非敏感证据投影。
type RecognitionLayoutPhysicalResultEvidenceV2 struct {
	PhysicalInvocationID    string                  `json:"physical_invocation_id"`
	PhysicalUnit            RecognitionPhysicalUnit `json:"physical_unit"`
	ResultDigest            string                  `json:"result_digest"`
	PlanDigest              string                  `json:"plan_digest"`
	CandidateExactSetDigest string                  `json:"candidate_exact_set_digest,omitempty"`
	Attempt                 int                     `json:"attempt"`
}

// RecognitionLayoutPlanFinalizationResultV2 完全由 Store 重建。
// 调用方不能提供候选结果或物理证据。
type RecognitionLayoutPlanFinalizationResultV2 struct {
	PlanID                         string                                      `json:"plan_id"`
	PlanDigest                     string                                      `json:"plan_digest"`
	CandidateExactSetDigest        string                                      `json:"candidate_exact_set_digest"`
	CandidateResultsExactSetDigest string                                      `json:"candidate_results_exact_set_digest"`
	PhysicalResultsExactSetDigest  string                                      `json:"physical_results_exact_set_digest"`
	CandidateResultCount           int                                         `json:"candidate_result_count"`
	PhysicalResultCount            int                                         `json:"physical_result_count"`
	FinalizationDigest             string                                      `json:"finalization_digest"`
	CandidateResults               []RecognitionLayoutCandidateFinalResultV2   `json:"candidate_results"`
	PhysicalResults                []RecognitionLayoutPhysicalResultEvidenceV2 `json:"physical_results"`
}

// RecognitionLayoutCandidateResultsExactSetDigestV2 对有序的私有候选结果精确集合
// 计算摘要。候选顺序在语义上就是已授权计划顺序，因此此处不再排序。
func RecognitionLayoutCandidateResultsExactSetDigestV2(
	results []RecognitionLayoutCandidateFinalResultV2,
) (string, error) {
	if len(results) < 1 || len(results) > recognitionLayoutTargetLimitV2 {
		return "", fmt.Errorf(
			"%w: finalized candidate result count must be 1..%d",
			ErrRecognitionLayoutPlanInvalid,
			recognitionLayoutTargetLimitV2,
		)
	}
	seen := make(map[string]struct{}, len(results))
	for index, result := range results {
		if result.CandidateID == "" ||
			strings.TrimSpace(result.CandidateID) != result.CandidateID ||
			(result.ResultKind != RecognitionLayoutCandidateQuestionV2 &&
				result.ResultKind != RecognitionLayoutCandidateNonQuestionV2) ||
			!validRecognitionLayoutSHA256(result.ResultDigest) ||
			result.SourcePhysicalInvocationID == "" ||
			strings.TrimSpace(result.SourcePhysicalInvocationID) !=
				result.SourcePhysicalInvocationID ||
			!result.SourcePhysicalUnit.Valid() ||
			!validRecognitionLayoutSHA256(result.SourcePhysicalResultDigest) {
			return "", fmt.Errorf(
				"%w: invalid finalized candidate result at ordinal %d",
				ErrRecognitionLayoutPlanInvalid,
				index+1,
			)
		}
		if _, duplicate := seen[result.CandidateID]; duplicate {
			return "", fmt.Errorf(
				"%w: duplicate finalized candidate identity",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		seen[result.CandidateID] = struct{}{}
		if err := validateCanonicalRecognitionLayoutFinalResultJSONV2(
			result.ResultJSON,
		); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(struct {
		Contract string                                    `json:"contract"`
		Results  []RecognitionLayoutCandidateFinalResultV2 `json:"results"`
	}{
		Contract: "recognition_layout_candidate_results_exact_set_v2",
		Results:  results,
	})
	if err != nil {
		return "", fmt.Errorf(
			"%w: encode finalized candidate exact-set: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	return recognitionLayoutSHA256(encoded), nil
}

// RecognitionLayoutPhysicalResultsExactSetDigestV2 按稳定的物理证据顺序计算摘要：
// 先是清单，再是按计划顺序排列的主批次，最后是按候选顺序排列的实际授权修复。
func RecognitionLayoutPhysicalResultsExactSetDigestV2(
	results []RecognitionLayoutPhysicalResultEvidenceV2,
) (string, error) {
	const maxPhysicalResults = 1 + recognitionLayoutTargetLimitV2 +
		recognitionLayoutTargetLimitV2
	if len(results) < 2 || len(results) > maxPhysicalResults {
		return "", fmt.Errorf(
			"%w: finalized physical result count must be 2..%d",
			ErrRecognitionLayoutPlanInvalid,
			maxPhysicalResults,
		)
	}
	seenIDs := make(map[string]struct{}, len(results))
	seenUnits := make(map[RecognitionPhysicalUnit]struct{}, len(results))
	for index, result := range results {
		if result.PhysicalInvocationID == "" ||
			strings.TrimSpace(result.PhysicalInvocationID) != result.PhysicalInvocationID ||
			!result.PhysicalUnit.Valid() ||
			!validRecognitionLayoutSHA256(result.ResultDigest) ||
			!validRecognitionLayoutSHA256(result.PlanDigest) || result.Attempt != 1 {
			return "", fmt.Errorf(
				"%w: invalid finalized physical result at ordinal %d",
				ErrRecognitionLayoutPlanInvalid,
				index+1,
			)
		}
		if index == 0 {
			if result.PhysicalUnit != RecognitionPhysicalUnitWholePage ||
				result.CandidateExactSetDigest != "" {
				return "", fmt.Errorf(
					"%w: finalized physical exact-set must start with the manifest",
					ErrRecognitionLayoutPlanInvalid,
				)
			}
		} else if (!strings.HasPrefix(string(result.PhysicalUnit), "layout_batch_") &&
			!strings.HasPrefix(string(result.PhysicalUnit), "layout_repair_")) ||
			!validRecognitionLayoutSHA256(result.CandidateExactSetDigest) {
			return "", fmt.Errorf(
				"%w: finalized V2 child lacks a layout exact-set",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		if _, duplicate := seenIDs[result.PhysicalInvocationID]; duplicate {
			return "", fmt.Errorf(
				"%w: duplicate finalized physical identity",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		if _, duplicate := seenUnits[result.PhysicalUnit]; duplicate {
			return "", fmt.Errorf(
				"%w: duplicate finalized physical unit",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		seenIDs[result.PhysicalInvocationID] = struct{}{}
		seenUnits[result.PhysicalUnit] = struct{}{}
	}
	encoded, err := json.Marshal(struct {
		Contract string                                      `json:"contract"`
		Results  []RecognitionLayoutPhysicalResultEvidenceV2 `json:"results"`
	}{
		Contract: "recognition_layout_physical_results_exact_set_v2",
		Results:  results,
	})
	if err != nil {
		return "", fmt.Errorf(
			"%w: encode finalized physical exact-set: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	return recognitionLayoutSHA256(encoded), nil
}

// CanonicalRecognitionLayoutPlanFinalizationV2 在计算非敏感最终化回执摘要前，
// 重新计算两个聚合精确集合的摘要与数量。看似有效但由调用方选择的聚合摘要会被拒绝。
func CanonicalRecognitionLayoutPlanFinalizationV2(
	parentInvocationID string,
	result RecognitionLayoutPlanFinalizationResultV2,
) ([]byte, string, error) {
	if parentInvocationID == "" ||
		strings.TrimSpace(parentInvocationID) != parentInvocationID ||
		result.PlanID == "" || strings.TrimSpace(result.PlanID) != result.PlanID ||
		!validRecognitionLayoutSHA256(result.PlanDigest) ||
		!validRecognitionLayoutSHA256(result.CandidateExactSetDigest) {
		return nil, "", fmt.Errorf(
			"%w: invalid layout finalization identity",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	candidateDigest, err := RecognitionLayoutCandidateResultsExactSetDigestV2(
		result.CandidateResults,
	)
	if err != nil {
		return nil, "", err
	}
	physicalDigest, err := RecognitionLayoutPhysicalResultsExactSetDigestV2(
		result.PhysicalResults,
	)
	if err != nil {
		return nil, "", err
	}
	if result.CandidateResultsExactSetDigest != candidateDigest ||
		result.PhysicalResultsExactSetDigest != physicalDigest ||
		result.CandidateResultCount != len(result.CandidateResults) ||
		result.PhysicalResultCount != len(result.PhysicalResults) {
		return nil, "", fmt.Errorf(
			"%w: layout finalization aggregate facts are not reproducible",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	physicalByID := make(map[string]RecognitionLayoutPhysicalResultEvidenceV2, len(result.PhysicalResults))
	for _, physical := range result.PhysicalResults {
		physicalByID[physical.PhysicalInvocationID] = physical
	}
	for _, candidate := range result.CandidateResults {
		physical, ok := physicalByID[candidate.SourcePhysicalInvocationID]
		if !ok || physical.PhysicalUnit != candidate.SourcePhysicalUnit ||
			physical.ResultDigest != candidate.SourcePhysicalResultDigest {
			return nil, "", fmt.Errorf(
				"%w: finalized candidate is detached from physical exact-set",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
	}
	encoded, err := json.Marshal(struct {
		Contract                       string `json:"contract"`
		PlanID                         string `json:"plan_id"`
		ParentInvocationID             string `json:"parent_invocation_id"`
		PlanDigest                     string `json:"plan_digest"`
		CandidateExactSetDigest        string `json:"candidate_exact_set_digest"`
		CandidateResultsExactSetDigest string `json:"candidate_results_exact_set_digest"`
		PhysicalResultsExactSetDigest  string `json:"physical_results_exact_set_digest"`
		CandidateResultCount           int    `json:"candidate_result_count"`
		PhysicalResultCount            int    `json:"physical_result_count"`
	}{
		Contract:                       "recognition_layout_plan_finalization_v2",
		PlanID:                         result.PlanID,
		ParentInvocationID:             parentInvocationID,
		PlanDigest:                     result.PlanDigest,
		CandidateExactSetDigest:        result.CandidateExactSetDigest,
		CandidateResultsExactSetDigest: candidateDigest,
		PhysicalResultsExactSetDigest:  physicalDigest,
		CandidateResultCount:           len(result.CandidateResults),
		PhysicalResultCount:            len(result.PhysicalResults),
	})
	if err != nil {
		return nil, "", fmt.Errorf(
			"%w: encode layout finalization: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	digest := recognitionLayoutSHA256(encoded)
	if result.FinalizationDigest != "" && result.FinalizationDigest != digest {
		return nil, "", fmt.Errorf(
			"%w: layout finalization digest is not reproducible",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	return encoded, digest, nil
}

func validateCanonicalRecognitionLayoutFinalResultJSONV2(raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return fmt.Errorf(
			"%w: finalized candidate result must be a JSON object",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf(
			"%w: finalized candidate result contains trailing JSON",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	canonical, err := json.Marshal(object)
	if err != nil || !bytes.Equal(canonical, raw) {
		return fmt.Errorf(
			"%w: finalized candidate result JSON is not canonical",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	return nil
}

func (b RecognitionLayoutBudgetBucketsV2) Select(
	problemCount int,
) (maxProblems int, durationMillis int64, err error) {
	switch {
	case problemCount == 1:
		return 1, b.UpTo1ProblemMillis, nil
	case problemCount >= 2 && problemCount <= 8:
		return 8, b.UpTo8ProblemsMillis, nil
	case problemCount >= 9 && problemCount <= 16:
		return 16, b.UpTo16ProblemsMillis, nil
	case problemCount >= 17 && problemCount <= 32:
		return 32, b.UpTo32ProblemsMillis, nil
	default:
		return 0, 0, fmt.Errorf(
			"%w: problem count must be 1..32",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
}

// CanonicalRecognitionLayoutPlanHeaderV2 返回存入 V76 头部账本的精确 JSON，
// 以及绑定到初始 whole_page 回执的摘要。
func CanonicalRecognitionLayoutPlanHeaderV2(
	header RecognitionLayoutPlanHeaderV2,
) ([]byte, string, error) {
	if header.PlanID == "" || strings.TrimSpace(header.PlanID) != header.PlanID ||
		header.ParentInvocationID == "" ||
		strings.TrimSpace(header.ParentInvocationID) != header.ParentInvocationID ||
		header.AgentName == "" || strings.TrimSpace(header.AgentName) != header.AgentName ||
		header.JobID == "" || strings.TrimSpace(header.JobID) != header.JobID ||
		header.ParentRequestDigest == "" ||
		strings.TrimSpace(header.ParentRequestDigest) != header.ParentRequestDigest ||
		!validRecognitionLayoutSHA256(header.PageDigest) ||
		header.StageStartedAtUnixMillis <= 0 ||
		header.PhysicalCallCapMillis != 120000 ||
		header.BudgetBuckets.UpTo1ProblemMillis <= 0 ||
		header.BudgetBuckets.UpTo8ProblemsMillis <= 0 ||
		header.BudgetBuckets.UpTo16ProblemsMillis <= 0 ||
		header.BudgetBuckets.UpTo32ProblemsMillis <= 0 ||
		header.AdapterWorkerHardCap != 2 ||
		header.EffectiveConcurrency < 1 || header.EffectiveConcurrency > 2 {
		return nil, "", fmt.Errorf(
			"%w: invalid immutable layout-plan header",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	normalizedRoute := NormalizeGradingModelSnapshot(header.RouteSnapshot)
	normalizedPolicy := NormalizeModelRequestPolicySnapshot(header.RequestPolicySnapshot)
	if header.RouteSnapshot != normalizedRoute ||
		header.RequestPolicySnapshot != normalizedPolicy ||
		normalizedRoute.Provider == "" || normalizedRoute.Model == "" ||
		normalizedRoute.Route == "" {
		return nil, "", fmt.Errorf(
			"%w: non-canonical layout-plan route or request policy",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	canonical := struct {
		Contract string `json:"contract"`
		RecognitionLayoutPlanHeaderV2
	}{
		Contract:                      "recognition_layout_plan_header_v2",
		RecognitionLayoutPlanHeaderV2: header,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", fmt.Errorf(
			"%w: encode layout-plan header: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	digest := sha256.Sum256(encoded)
	return encoded, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func RecognitionLayoutPlanHeaderDigestV2(
	header RecognitionLayoutPlanHeaderV2,
) (string, error) {
	_, digest, err := CanonicalRecognitionLayoutPlanHeaderV2(header)
	return digest, err
}

// RecognitionLayoutTargetExactSetDigestV2 绑定有序目标标识集合。
// 完整授权并集与每个物理批次的有序子集共用同一契约。
func RecognitionLayoutTargetExactSetDigestV2(
	targetIDs []string,
) (string, error) {
	if len(targetIDs) < 1 || len(targetIDs) > recognitionLayoutTargetLimitV2 {
		return "", fmt.Errorf(
			"%w: target exact-set count must be 1..%d",
			ErrRecognitionLayoutPlanInvalid,
			recognitionLayoutTargetLimitV2,
		)
	}
	seen := make(map[string]struct{}, len(targetIDs))
	canonicalIDs := make([]string, len(targetIDs))
	for index, targetID := range targetIDs {
		if targetID == "" || strings.TrimSpace(targetID) != targetID {
			return "", fmt.Errorf(
				"%w: target exact-set contains non-canonical identity",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		if _, duplicate := seen[targetID]; duplicate {
			return "", fmt.Errorf(
				"%w: target exact-set contains duplicate identity",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		seen[targetID] = struct{}{}
		canonicalIDs[index] = targetID
	}
	encoded, err := json.Marshal(struct {
		Contract  string   `json:"contract"`
		TargetIDs []string `json:"target_ids"`
	}{
		Contract:  "recognition_layout_target_exact_set_v2",
		TargetIDs: canonicalIDs,
	})
	if err != nil {
		return "", fmt.Errorf(
			"%w: encode target exact-set: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// ValidateRecognitionLayoutPlanV2 是持久化边界校验器。
// 它重新计算计划器所有的摘要，并证明主批次对全部目标构成有序、两两不相交的精确覆盖。
func ValidateRecognitionLayoutPlanV2(plan RecognitionLayoutPlanV2) error {
	if plan.Version != RecognitionPlanVersionV2 ||
		!validRecognitionLayoutSHA256(plan.PageDigest) ||
		!validRecognitionLayoutSHA256(plan.ManifestResultDigest) ||
		len(plan.Targets) < 1 || len(plan.Targets) > recognitionLayoutTargetLimitV2 {
		return fmt.Errorf(
			"%w: invalid authorized layout-plan identity",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	if err := validateRecognitionLayoutManifestSuccessV2(
		RecognitionLayoutManifestSuccessV2{
			InvocationID: plan.ManifestInvocationID,
			ResultDigest: plan.ManifestResultDigest,
		},
	); err != nil {
		return err
	}
	targetIDs := make([]string, len(plan.Targets))
	for index, target := range plan.Targets {
		if target.TargetID == "" || strings.TrimSpace(target.TargetID) != target.TargetID ||
			!validRecognitionLayoutSHA256(target.CropDigest) ||
			target.Region.X < 0 || target.Region.Y < 0 ||
			target.Region.Width <= 0 || target.Region.Height <= 0 {
			return fmt.Errorf(
				"%w: invalid durable target at ordinal %d",
				ErrRecognitionLayoutPlanInvalid,
				index+1,
			)
		}
		if err := validateRecognitionLayoutSourceNumberV2(
			target.SourceNumberPath,
			target.DisplayLabel,
		); err != nil {
			return err
		}
		if err := validateRecognitionLayoutSourceSectionV2(
			target.SourceSectionPath,
			target.SourceSectionLabel,
		); err != nil {
			return err
		}
		targetIDs[index] = target.TargetID
	}
	if _, err := RecognitionLayoutTargetExactSetDigestV2(targetIDs); err != nil {
		return err
	}
	union := make([]string, 0, len(targetIDs))
	for index, batch := range plan.Batches {
		wantUnit, err := RecognitionLayoutBatchUnitV2(index + 1)
		if err != nil || batch.Unit != wantUnit ||
			len(batch.TargetIDs) < 1 ||
			len(batch.TargetIDs) > RecognitionLayoutBatchTargetLimitV2 ||
			!validRecognitionLayoutSHA256(batch.InputDigest) {
			return fmt.Errorf(
				"%w: invalid primary batch at ordinal %d",
				ErrRecognitionLayoutPlanInvalid,
				index+1,
			)
		}
		if _, err := RecognitionLayoutTargetExactSetDigestV2(batch.TargetIDs); err != nil {
			return err
		}
		union = append(union, batch.TargetIDs...)
	}
	if len(union) != len(targetIDs) {
		return fmt.Errorf(
			"%w: primary batches do not cover the target exact-set",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	for index := range targetIDs {
		if union[index] != targetIDs[index] {
			return fmt.Errorf(
				"%w: primary batch union drifted at ordinal %d",
				ErrRecognitionLayoutPlanInvalid,
				index+1,
			)
		}
	}
	wantDigest, err := recognitionLayoutAuthorizedPlanDigestV2(plan)
	if err != nil || wantDigest != plan.AuthorizedPlanDigest {
		return fmt.Errorf(
			"%w: authorized plan digest mismatch",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	return nil
}
