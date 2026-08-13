package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// loadStoredRecognitionLayoutFinalizationV2 是回执优先的重放路径。
// 它仅在不可变终态记录存在后可用：Store 冻结候选结果、物理调用精确集合摘要和规范回执后，
// 不再需要 Provider payload 字节。
func loadStoredRecognitionLayoutFinalizationV2(
	ctx context.Context,
	q dbQueryer,
	authority recognitionLayoutFinalizationAuthorityV2,
) (k12.RecognitionLayoutPlanFinalizationResultV2, []byte, bool, error) {
	var result k12.RecognitionLayoutPlanFinalizationResultV2
	var finalizationJSON string
	err := q.QueryRowContext(ctx, `SELECT authorized_plan_digest,
		candidate_exact_set_digest,candidate_results_exact_set_digest,
		physical_results_exact_set_digest,candidate_result_count,
		physical_result_count,finalization_json,finalization_digest
		FROM k12_recognition_layout_finalizations WHERE plan_id=?
		AND parent_invocation_id=?`,
		authority.PlanID,
		authority.Parent.InvocationID,
	).Scan(
		&result.PlanDigest,
		&result.CandidateExactSetDigest,
		&result.CandidateResultsExactSetDigest,
		&result.PhysicalResultsExactSetDigest,
		&result.CandidateResultCount,
		&result.PhysicalResultCount,
		&finalizationJSON,
		&result.FinalizationDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil, false, nil
	}
	if err != nil {
		return result, nil, false, fmt.Errorf(
			"k12storage: read stored layout finalization replay: %w",
			err,
		)
	}
	result.PlanID = authority.PlanID
	if result.PlanDigest != authority.Plan.AuthorizedPlanDigest ||
		result.CandidateExactSetDigest != authority.CandidateExactSetDigest {
		return result, nil, true, fmt.Errorf(
			"%w: stored layout finalization authority drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	for _, target := range authority.Plan.Targets {
		var candidate k12.RecognitionLayoutCandidateFinalResultV2
		var resultJSON string
		candidateReadErr := q.QueryRowContext(ctx, `SELECT candidate_id,result_kind,
			result_digest,result_json,source_physical_invocation_id,
			source_physical_result_digest
			FROM k12_recognition_layout_candidate_results
			WHERE plan_id=? AND candidate_id=?`,
			authority.PlanID,
			target.TargetID,
		).Scan(
			&candidate.CandidateID,
			&candidate.ResultKind,
			&candidate.ResultDigest,
			&resultJSON,
			&candidate.SourcePhysicalInvocationID,
			&candidate.SourcePhysicalResultDigest,
		)
		if candidateReadErr != nil {
			return result, nil, true, fmt.Errorf(
				"k12storage: read stored finalized candidate: %w",
				candidateReadErr,
			)
		}
		canonicalResult, canonicalErr := canonicalRecognitionLayoutResultJSONV2(
			json.RawMessage(resultJSON),
		)
		if canonicalErr != nil || string(canonicalResult) != resultJSON {
			return result, nil, true, fmt.Errorf(
				"%w: stored finalized candidate JSON drifted: %v",
				ErrModelPhysicalInvocationConflict,
				canonicalErr,
			)
		}
		child, childReadErr := getModelPhysicalInvocationByIDVia(
			ctx,
			q,
			authority.Parent.AgentName,
			candidate.SourcePhysicalInvocationID,
		)
		if childReadErr != nil || child.ParentInvocationID != authority.Parent.InvocationID ||
			child.Status != k12.ModelInvocationSucceeded || child.Attempt != 1 ||
			child.ResultDigest != candidate.SourcePhysicalResultDigest {
			return result, nil, true, fmt.Errorf(
				"%w: stored finalized candidate source drifted: %v",
				ErrModelPhysicalInvocationConflict,
				childReadErr,
			)
		}
		candidate.ResultJSON = append(json.RawMessage(nil), canonicalResult...)
		candidate.SourcePhysicalUnit = child.PhysicalUnit
		result.CandidateResults = append(result.CandidateResults, candidate)
	}
	physicalIDs := []string{authority.Plan.ManifestInvocationID}
	for _, batch := range authority.Plan.Batches {
		var physicalID string
		if batchReadErr := q.QueryRowContext(ctx, `SELECT source_physical_invocation_id
			FROM k12_recognition_layout_batch_settlements
			WHERE plan_id=? AND batch_id=?`,
			authority.PlanID,
			string(batch.Unit),
		).Scan(&physicalID); batchReadErr != nil {
			return result, nil, true, fmt.Errorf(
				"k12storage: read stored finalized batch source: %w",
				batchReadErr,
			)
		}
		physicalIDs = append(physicalIDs, physicalID)
	}
	for _, target := range authority.Plan.Targets {
		var physicalID string
		repairReadErr := q.QueryRowContext(ctx, `SELECT s.source_physical_invocation_id
			FROM k12_recognition_layout_repair_authorizations a
			JOIN k12_recognition_layout_repair_settlements s
			ON s.plan_id=a.plan_id AND s.candidate_id=a.candidate_id
			WHERE a.plan_id=? AND a.candidate_id=?`,
			authority.PlanID,
			target.TargetID,
		).Scan(&physicalID)
		if errors.Is(repairReadErr, sql.ErrNoRows) {
			continue
		}
		if repairReadErr != nil {
			return result, nil, true, fmt.Errorf(
				"k12storage: read stored finalized repair source: %w",
				repairReadErr,
			)
		}
		physicalIDs = append(physicalIDs, physicalID)
	}
	seen := make(map[string]struct{}, len(physicalIDs))
	for _, physicalID := range physicalIDs {
		if _, duplicate := seen[physicalID]; duplicate {
			return result, nil, true, fmt.Errorf(
				"%w: stored finalized physical exact-set is duplicated",
				ErrModelPhysicalInvocationConflict,
			)
		}
		seen[physicalID] = struct{}{}
		child, childReadErr := getModelPhysicalInvocationByIDVia(
			ctx,
			q,
			authority.Parent.AgentName,
			physicalID,
		)
		if childReadErr != nil || child.ParentInvocationID != authority.Parent.InvocationID ||
			child.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
			child.Status != k12.ModelInvocationSucceeded || child.Attempt != 1 {
			return result, nil, true, fmt.Errorf(
				"%w: stored finalized physical evidence drifted: %v",
				ErrModelPhysicalInvocationConflict,
				childReadErr,
			)
		}
		result.PhysicalResults = append(
			result.PhysicalResults,
			k12.RecognitionLayoutPhysicalResultEvidenceV2{
				PhysicalInvocationID:    child.PhysicalInvocationID,
				PhysicalUnit:            child.PhysicalUnit,
				ResultDigest:            child.ResultDigest,
				PlanDigest:              child.PlanDigest,
				CandidateExactSetDigest: child.CandidateExactSetDigest,
				Attempt:                 child.Attempt,
			},
		)
	}
	var physicalCount int
	if countErr := q.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM k12_model_physical_invocations WHERE parent_invocation_id=?
		AND recognition_plan_version='v2'`,
		authority.Parent.InvocationID,
	).Scan(&physicalCount); countErr != nil || physicalCount != len(physicalIDs) {
		return result, nil, true, fmt.Errorf(
			"%w: stored finalized physical exact-set cardinality drifted: %v",
			ErrModelPhysicalInvocationConflict,
			countErr,
		)
	}
	canonical, digest, err := k12.CanonicalRecognitionLayoutPlanFinalizationV2(
		authority.Parent.InvocationID,
		result,
	)
	if err != nil || string(canonical) != finalizationJSON ||
		digest != result.FinalizationDigest {
		return result, nil, true, fmt.Errorf(
			"%w: stored layout finalization receipt is not reproducible: %v",
			ErrModelPhysicalInvocationConflict,
			err,
		)
	}
	return result, canonical, true, nil
}
