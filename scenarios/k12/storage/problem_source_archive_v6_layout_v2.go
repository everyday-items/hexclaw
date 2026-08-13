package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ProblemSourceArchiveRecognitionLayoutV2 是仅用于 finalized-before-V73 崩溃窗口的
// 私有、受校验和保护的 V76 聚合。它直接携带权威计划表；物理调用行仅作为引用证据，
// 绝不会用于重建授权。
type ProblemSourceArchiveRecognitionLayoutV2 struct {
	WorkID               string                                                     `json:"work_id"`
	Plan                 ProblemSourceArchiveRecognitionLayoutPlanV2                `json:"plan"`
	Candidates           []ProblemSourceArchiveRecognitionLayoutCandidate           `json:"candidates"`
	Batches              []ProblemSourceArchiveRecognitionLayoutBatch               `json:"batches"`
	BatchMembers         []ProblemSourceArchiveRecognitionLayoutBatchMember         `json:"batch_members"`
	BatchSettlements     []ProblemSourceArchiveRecognitionLayoutBatchSettlement     `json:"batch_settlements"`
	CandidateResults     []ProblemSourceArchiveRecognitionLayoutCandidateResult     `json:"candidate_results"`
	RepairAuthorizations []ProblemSourceArchiveRecognitionLayoutRepairAuthorization `json:"repair_authorizations"`
	RepairSettlements    []ProblemSourceArchiveRecognitionLayoutRepairSettlement    `json:"repair_settlements"`
	Finalization         ProblemSourceArchiveRecognitionLayoutFinalization          `json:"finalization"`
}

type ProblemSourceArchiveRecognitionLayoutPlanV2 struct {
	PlanID                       string `json:"plan_id"`
	ParentInvocationID           string `json:"parent_invocation_id"`
	AgentName                    string `json:"agent_name"`
	JobID                        string `json:"job_id"`
	Stage                        string `json:"stage"`
	ManifestPhysicalInvocationID string `json:"manifest_physical_invocation_id"`
	PageDigest                   string `json:"page_digest"`
	HeaderDigest                 string `json:"header_digest"`
	ManifestResultDigest         string `json:"manifest_result_digest"`
	AuthorizedPlanDigest         string `json:"authorized_plan_digest"`
	CandidateExactSetDigest      string `json:"candidate_exact_set_digest"`
	LayoutHeaderJSON             string `json:"layout_header_json"`
	AuthorizedPlanJSON           string `json:"authorized_plan_json"`
	StageStartedAt               int64  `json:"stage_started_at"`
	StageDeadlineAt              int64  `json:"stage_deadline_at"`
	SelectedBucketMaxProblems    int    `json:"selected_bucket_max_problems"`
	EffectiveConcurrency         int    `json:"effective_concurrency"`
	Status                       string `json:"status"`
	CreatedAt                    int64  `json:"created_at"`
	UpdatedAt                    int64  `json:"updated_at"`
}

type ProblemSourceArchiveRecognitionLayoutCandidate struct {
	PlanID        string `json:"plan_id"`
	CandidateID   string `json:"candidate_id"`
	Ordinal       int    `json:"ordinal"`
	BBoxX         int    `json:"bbox_x"`
	BBoxY         int    `json:"bbox_y"`
	BBoxWidth     int    `json:"bbox_width"`
	BBoxHeight    int    `json:"bbox_height"`
	CropDigest    string `json:"crop_digest"`
	CandidateJSON string `json:"candidate_json"`
	CreatedAt     int64  `json:"created_at"`
}

type ProblemSourceArchiveRecognitionLayoutBatch struct {
	PlanID       string `json:"plan_id"`
	BatchID      string `json:"batch_id"`
	Ordinal      int    `json:"ordinal"`
	PhysicalUnit string `json:"physical_unit"`
	MemberCount  int    `json:"member_count"`
	BatchDigest  string `json:"batch_digest"`
	InputDigest  string `json:"input_digest"`
	CreatedAt    int64  `json:"created_at"`
}

type ProblemSourceArchiveRecognitionLayoutBatchMember struct {
	PlanID      string `json:"plan_id"`
	BatchID     string `json:"batch_id"`
	Slot        int    `json:"slot"`
	CandidateID string `json:"candidate_id"`
	CreatedAt   int64  `json:"created_at"`
}

type ProblemSourceArchiveRecognitionLayoutBatchSettlement struct {
	PlanID                     string `json:"plan_id"`
	BatchID                    string `json:"batch_id"`
	ParentInvocationID         string `json:"parent_invocation_id"`
	SourcePhysicalInvocationID string `json:"source_physical_invocation_id"`
	SourcePhysicalUnit         string `json:"source_physical_unit"`
	SourcePhysicalResultDigest string `json:"source_physical_result_digest"`
	Classification             string `json:"classification"`
	AmbiguityKind              string `json:"ambiguity_kind"`
	SettlementDigest           string `json:"settlement_digest"`
	CreatedAt                  int64  `json:"created_at"`
}

type ProblemSourceArchiveRecognitionLayoutCandidateResult struct {
	PlanID                     string `json:"plan_id"`
	CandidateID                string `json:"candidate_id"`
	ParentInvocationID         string `json:"parent_invocation_id"`
	SourcePhysicalInvocationID string `json:"source_physical_invocation_id"`
	SourcePhysicalResultDigest string `json:"source_physical_result_digest"`
	ResultKind                 string `json:"result_kind"`
	ResultDigest               string `json:"result_digest"`
	ResultJSON                 string `json:"result_json"`
	CreatedAt                  int64  `json:"created_at"`
}

type ProblemSourceArchiveRecognitionLayoutRepairAuthorization struct {
	PlanID                          string `json:"plan_id"`
	RepairAuthorizationID           string `json:"repair_authorization_id"`
	RepairPhysicalUnit              string `json:"repair_physical_unit"`
	CandidateID                     string `json:"candidate_id"`
	SourceBatchID                   string `json:"source_batch_id"`
	SourceBatchPhysicalInvocationID string `json:"source_batch_physical_invocation_id"`
	SourceBatchResultDigest         string `json:"source_batch_result_digest"`
	RepairRound                     int    `json:"repair_round"`
	AuthorizationDigest             string `json:"authorization_digest"`
	CreatedAt                       int64  `json:"created_at"`
}

type ProblemSourceArchiveRecognitionLayoutRepairSettlement struct {
	PlanID                     string `json:"plan_id"`
	RepairAuthorizationID      string `json:"repair_authorization_id"`
	AuthorizationDigest        string `json:"authorization_digest"`
	CandidateID                string `json:"candidate_id"`
	ParentInvocationID         string `json:"parent_invocation_id"`
	SourcePhysicalInvocationID string `json:"source_physical_invocation_id"`
	SourcePhysicalUnit         string `json:"source_physical_unit"`
	SourcePhysicalResultDigest string `json:"source_physical_result_digest"`
	Classification             string `json:"classification"`
	ResultKind                 string `json:"result_kind"`
	ResultDigest               string `json:"result_digest"`
	SettlementDigest           string `json:"settlement_digest"`
	CreatedAt                  int64  `json:"created_at"`
}

type ProblemSourceArchiveRecognitionLayoutFinalization struct {
	PlanID                         string `json:"plan_id"`
	ParentInvocationID             string `json:"parent_invocation_id"`
	AuthorizedPlanDigest           string `json:"authorized_plan_digest"`
	CandidateExactSetDigest        string `json:"candidate_exact_set_digest"`
	CandidateResultsExactSetDigest string `json:"candidate_results_exact_set_digest"`
	PhysicalResultsExactSetDigest  string `json:"physical_results_exact_set_digest"`
	CandidateResultCount           int    `json:"candidate_result_count"`
	PhysicalResultCount            int    `json:"physical_result_count"`
	FinalizationJSON               string `json:"finalization_json"`
	FinalizationDigest             string `json:"finalization_digest"`
	CreatedAt                      int64  `json:"created_at"`
}

func exportProblemSourceArchiveRecognitionLayoutsV2(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	out *ProblemSourceArchiveV6,
) (map[string]struct{}, map[string]struct{}, error) {
	parents := map[string]struct{}{}
	physical := map[string]struct{}{}
	if out == nil {
		return parents, physical, errors.New("nil problem-source archive")
	}
	committed := make(map[string]struct{}, len(out.RecognitionResults))
	for _, result := range out.RecognitionResults {
		committed[result.WorkID] = struct{}{}
	}
	for _, work := range out.ReprocessJobs {
		if _, ok := committed[work.WorkID]; ok ||
			(work.Action != "select_region" && work.Action != "retake") {
			continue
		}
		rows, queryErr := q.QueryContext(ctx, `SELECT
			p.plan_id,p.parent_invocation_id,p.agent_name,p.job_id,p.stage,
			p.manifest_physical_invocation_id,p.page_digest,p.header_digest,
			p.manifest_result_digest,p.authorized_plan_digest,
			p.candidate_exact_set_digest,p.layout_header_json,
			p.authorized_plan_json,p.stage_started_at,p.stage_deadline_at,
			p.selected_bucket_max_problems,p.effective_concurrency,p.status,
			p.created_at,p.updated_at
			FROM k12_recognition_layout_plans p
			JOIN k12_recognition_layout_finalizations f ON f.plan_id=p.plan_id
			WHERE p.agent_name=? AND p.job_id=? AND p.status='succeeded'
			ORDER BY p.created_at,p.plan_id`, agentName, work.JobID)
		if queryErr != nil {
			return nil, nil, queryErr
		}
		var matches []ProblemSourceArchiveRecognitionLayoutPlanV2
		for rows.Next() {
			var plan ProblemSourceArchiveRecognitionLayoutPlanV2
			if err := rows.Scan(
				&plan.PlanID, &plan.ParentInvocationID, &plan.AgentName,
				&plan.JobID, &plan.Stage, &plan.ManifestPhysicalInvocationID,
				&plan.PageDigest, &plan.HeaderDigest, &plan.ManifestResultDigest,
				&plan.AuthorizedPlanDigest, &plan.CandidateExactSetDigest,
				&plan.LayoutHeaderJSON, &plan.AuthorizedPlanJSON,
				&plan.StageStartedAt, &plan.StageDeadlineAt,
				&plan.SelectedBucketMaxProblems, &plan.EffectiveConcurrency,
				&plan.Status, &plan.CreatedAt, &plan.UpdatedAt,
			); err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			parent, err := getModelInvocationByIDVia(
				ctx,
				q,
				plan.ParentInvocationID,
			)
			if err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			wantRequestDigest, err := ProblemSourceRecognitionParentRequestDigest(
				problemSourceArchiveJob(work),
				parent.RouteSnapshot,
				parent.RequestPolicySnapshot,
			)
			if err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			if parent.Status == k12.ModelInvocationSucceeded &&
				parent.RequestDigest == wantRequestDigest {
				matches = append(matches, plan)
			}
		}
		if err := rowsDone(rows); err != nil {
			return nil, nil, err
		}
		if len(matches) == 0 {
			continue
		}
		if len(matches) != 1 {
			return nil, nil, fmt.Errorf(
				"problem-source work %q has %d finalized V2 layout parents",
				work.WorkID,
				len(matches),
			)
		}
		aggregate, err := loadProblemSourceArchiveRecognitionLayoutV2(
			ctx,
			q,
			work.WorkID,
			matches[0],
		)
		if err != nil {
			return nil, nil, err
		}
		out.RecognitionLayoutsV2 = append(out.RecognitionLayoutsV2, aggregate)
		parents[aggregate.Plan.ParentInvocationID] = struct{}{}
		childRows, err := q.QueryContext(ctx, `SELECT physical_invocation_id
			FROM k12_model_physical_invocations WHERE parent_invocation_id=?
			ORDER BY physical_invocation_id`, aggregate.Plan.ParentInvocationID)
		if err != nil {
			return nil, nil, err
		}
		for childRows.Next() {
			var physicalID string
			if err := childRows.Scan(&physicalID); err != nil {
				_ = childRows.Close()
				return nil, nil, err
			}
			physical[physicalID] = struct{}{}
		}
		if err := rowsDone(childRows); err != nil {
			return nil, nil, err
		}
	}
	sort.Slice(out.RecognitionLayoutsV2, func(i, j int) bool {
		return out.RecognitionLayoutsV2[i].WorkID < out.RecognitionLayoutsV2[j].WorkID
	})
	return parents, physical, nil
}

func loadProblemSourceArchiveRecognitionLayoutV2(
	ctx context.Context,
	q dbQueryer,
	workID string,
	plan ProblemSourceArchiveRecognitionLayoutPlanV2,
) (ProblemSourceArchiveRecognitionLayoutV2, error) {
	out := ProblemSourceArchiveRecognitionLayoutV2{WorkID: workID, Plan: plan}
	rows, queryErr := q.QueryContext(ctx, `SELECT plan_id,candidate_id,ordinal,
		bbox_x,bbox_y,bbox_width,bbox_height,crop_digest,candidate_json,created_at
		FROM k12_recognition_layout_candidates WHERE plan_id=?
		ORDER BY ordinal,candidate_id`, plan.PlanID)
	if queryErr != nil {
		return out, queryErr
	}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionLayoutCandidate
		if err := rows.Scan(&v.PlanID, &v.CandidateID, &v.Ordinal, &v.BBoxX,
			&v.BBoxY, &v.BBoxWidth, &v.BBoxHeight, &v.CropDigest,
			&v.CandidateJSON, &v.CreatedAt); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.Candidates = append(out.Candidates, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}
	rows, queryErr = q.QueryContext(ctx, `SELECT plan_id,batch_id,ordinal,
		physical_unit,member_count,batch_digest,input_digest,created_at
		FROM k12_recognition_layout_batches WHERE plan_id=?
		ORDER BY ordinal,batch_id`, plan.PlanID)
	if queryErr != nil {
		return out, queryErr
	}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionLayoutBatch
		if err := rows.Scan(&v.PlanID, &v.BatchID, &v.Ordinal, &v.PhysicalUnit,
			&v.MemberCount, &v.BatchDigest, &v.InputDigest, &v.CreatedAt); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.Batches = append(out.Batches, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}
	rows, queryErr = q.QueryContext(ctx, `SELECT plan_id,batch_id,slot,candidate_id,created_at
		FROM k12_recognition_layout_batch_members WHERE plan_id=?
		ORDER BY batch_id,slot`, plan.PlanID)
	if queryErr != nil {
		return out, queryErr
	}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionLayoutBatchMember
		if err := rows.Scan(&v.PlanID, &v.BatchID, &v.Slot, &v.CandidateID,
			&v.CreatedAt); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.BatchMembers = append(out.BatchMembers, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}
	rows, queryErr = q.QueryContext(ctx, `SELECT plan_id,batch_id,parent_invocation_id,
		source_physical_invocation_id,source_physical_unit,
		source_physical_result_digest,classification,ambiguity_kind,
		settlement_digest,created_at
		FROM k12_recognition_layout_batch_settlements WHERE plan_id=?
		ORDER BY batch_id`, plan.PlanID)
	if queryErr != nil {
		return out, queryErr
	}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionLayoutBatchSettlement
		if err := rows.Scan(&v.PlanID, &v.BatchID, &v.ParentInvocationID,
			&v.SourcePhysicalInvocationID, &v.SourcePhysicalUnit,
			&v.SourcePhysicalResultDigest, &v.Classification,
			&v.AmbiguityKind, &v.SettlementDigest, &v.CreatedAt); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.BatchSettlements = append(out.BatchSettlements, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}
	rows, queryErr = q.QueryContext(ctx, `SELECT plan_id,candidate_id,
		parent_invocation_id,source_physical_invocation_id,
		source_physical_result_digest,result_kind,result_digest,result_json,created_at
		FROM k12_recognition_layout_candidate_results WHERE plan_id=?
		ORDER BY candidate_id`, plan.PlanID)
	if queryErr != nil {
		return out, queryErr
	}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionLayoutCandidateResult
		if err := rows.Scan(&v.PlanID, &v.CandidateID, &v.ParentInvocationID,
			&v.SourcePhysicalInvocationID, &v.SourcePhysicalResultDigest,
			&v.ResultKind, &v.ResultDigest, &v.ResultJSON, &v.CreatedAt); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.CandidateResults = append(out.CandidateResults, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}
	rows, queryErr = q.QueryContext(ctx, `SELECT plan_id,repair_authorization_id,
		repair_physical_unit,candidate_id,source_batch_id,
		source_batch_physical_invocation_id,source_batch_result_digest,
		repair_round,authorization_digest,created_at
		FROM k12_recognition_layout_repair_authorizations WHERE plan_id=?
		ORDER BY candidate_id`, plan.PlanID)
	if queryErr != nil {
		return out, queryErr
	}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionLayoutRepairAuthorization
		if err := rows.Scan(&v.PlanID, &v.RepairAuthorizationID,
			&v.RepairPhysicalUnit, &v.CandidateID, &v.SourceBatchID,
			&v.SourceBatchPhysicalInvocationID, &v.SourceBatchResultDigest,
			&v.RepairRound, &v.AuthorizationDigest, &v.CreatedAt); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.RepairAuthorizations = append(out.RepairAuthorizations, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}
	rows, queryErr = q.QueryContext(ctx, `SELECT plan_id,repair_authorization_id,
		authorization_digest,candidate_id,parent_invocation_id,
		source_physical_invocation_id,source_physical_unit,
		source_physical_result_digest,classification,result_kind,result_digest,
		settlement_digest,created_at
		FROM k12_recognition_layout_repair_settlements WHERE plan_id=?
		ORDER BY candidate_id`, plan.PlanID)
	if queryErr != nil {
		return out, queryErr
	}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionLayoutRepairSettlement
		if err := rows.Scan(&v.PlanID, &v.RepairAuthorizationID,
			&v.AuthorizationDigest, &v.CandidateID, &v.ParentInvocationID,
			&v.SourcePhysicalInvocationID, &v.SourcePhysicalUnit,
			&v.SourcePhysicalResultDigest, &v.Classification, &v.ResultKind,
			&v.ResultDigest, &v.SettlementDigest, &v.CreatedAt); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.RepairSettlements = append(out.RepairSettlements, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}
	if err := q.QueryRowContext(ctx, `SELECT plan_id,parent_invocation_id,
		authorized_plan_digest,candidate_exact_set_digest,
		candidate_results_exact_set_digest,physical_results_exact_set_digest,
		candidate_result_count,physical_result_count,finalization_json,
		finalization_digest,created_at
		FROM k12_recognition_layout_finalizations WHERE plan_id=?`,
		plan.PlanID,
	).Scan(
		&out.Finalization.PlanID,
		&out.Finalization.ParentInvocationID,
		&out.Finalization.AuthorizedPlanDigest,
		&out.Finalization.CandidateExactSetDigest,
		&out.Finalization.CandidateResultsExactSetDigest,
		&out.Finalization.PhysicalResultsExactSetDigest,
		&out.Finalization.CandidateResultCount,
		&out.Finalization.PhysicalResultCount,
		&out.Finalization.FinalizationJSON,
		&out.Finalization.FinalizationDigest,
		&out.Finalization.CreatedAt,
	); err != nil {
		return out, err
	}
	return out, nil
}

func problemSourceArchiveRecognitionLayoutPhysicalIDsV2(
	layouts []ProblemSourceArchiveRecognitionLayoutV2,
) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, layout := range layouts {
		ids := []string{layout.Plan.ManifestPhysicalInvocationID}
		for _, settlement := range layout.BatchSettlements {
			ids = append(ids, settlement.SourcePhysicalInvocationID)
		}
		for _, settlement := range layout.RepairSettlements {
			ids = append(ids, settlement.SourcePhysicalInvocationID)
		}
		for _, physicalID := range ids {
			if strings.TrimSpace(physicalID) == "" {
				return nil, errors.New("recognition layout has empty physical identity")
			}
			if _, duplicate := out[physicalID]; duplicate {
				return nil, fmt.Errorf(
					"duplicate recognition layout physical invocation %q",
					physicalID,
				)
			}
			out[physicalID] = struct{}{}
		}
	}
	return out, nil
}

func validateProblemSourceArchiveRecognitionLayoutsV2(
	agentName string,
	layouts []ProblemSourceArchiveRecognitionLayoutV2,
	works map[string]ProblemSourceArchiveReprocessJob,
	results map[string]ProblemSourceArchiveRecognitionResult,
	parents map[string]k12.ModelInvocation,
	physical map[string]k12.ModelPhysicalInvocation,
) error {
	seenWorks := map[string]struct{}{}
	seenPlans := map[string]struct{}{}
	seenParents := map[string]struct{}{}
	for _, aggregate := range layouts {
		work, ok := works[aggregate.WorkID]
		if !ok || work.AgentName != agentName ||
			(work.Action != "select_region" && work.Action != "retake") {
			return fmt.Errorf("recognition layout work scope mismatch")
		}
		if _, committed := results[aggregate.WorkID]; committed {
			return fmt.Errorf("committed V73 result retained private layout aggregate")
		}
		if _, duplicate := seenWorks[aggregate.WorkID]; duplicate {
			return fmt.Errorf("duplicate recognition layout work %q", aggregate.WorkID)
		}
		if _, duplicate := seenPlans[aggregate.Plan.PlanID]; duplicate {
			return fmt.Errorf("duplicate recognition layout plan %q", aggregate.Plan.PlanID)
		}
		if _, duplicate := seenParents[aggregate.Plan.ParentInvocationID]; duplicate {
			return fmt.Errorf(
				"duplicate recognition layout parent %q",
				aggregate.Plan.ParentInvocationID,
			)
		}
		seenWorks[aggregate.WorkID] = struct{}{}
		seenPlans[aggregate.Plan.PlanID] = struct{}{}
		seenParents[aggregate.Plan.ParentInvocationID] = struct{}{}
		if err := validateProblemSourceArchiveRecognitionLayoutV2(
			agentName,
			work,
			aggregate,
			parents,
			physical,
		); err != nil {
			return fmt.Errorf(
				"recognition layout work %q: %w",
				aggregate.WorkID,
				err,
			)
		}
	}
	return nil
}

func validateProblemSourceArchiveRecognitionLayoutV2(
	agentName string,
	work ProblemSourceArchiveReprocessJob,
	aggregate ProblemSourceArchiveRecognitionLayoutV2,
	parents map[string]k12.ModelInvocation,
	physical map[string]k12.ModelPhysicalInvocation,
) error {
	row := aggregate.Plan
	parent, ok := parents[row.ParentInvocationID]
	if !ok || parent.AgentName != agentName || parent.JobID != work.JobID ||
		parent.Stage != k12.GradingStageRecognizing ||
		parent.Status != k12.ModelInvocationSucceeded {
		return errors.New("finalized layout parent is missing or non-terminal")
	}
	wantRequestDigest, requestDigestErr := ProblemSourceRecognitionParentRequestDigest(
		problemSourceArchiveJob(work),
		parent.RouteSnapshot,
		parent.RequestPolicySnapshot,
	)
	if requestDigestErr != nil || parent.RequestDigest != wantRequestDigest {
		return fmt.Errorf("layout parent request identity drifted: %v", requestDigestErr)
	}
	if row.PlanID == "" || row.ParentInvocationID != parent.InvocationID ||
		row.AgentName != agentName || row.JobID != work.JobID ||
		row.Stage != k12.GradingStageRecognizing || row.Status != "succeeded" {
		return errors.New("layout plan scope/status drifted")
	}
	var headerEnvelope struct {
		Contract string `json:"contract"`
		k12.RecognitionLayoutPlanHeaderV2
	}
	if err := json.Unmarshal([]byte(row.LayoutHeaderJSON), &headerEnvelope); err != nil {
		return fmt.Errorf("decode layout header: %w", err)
	}
	canonicalHeader, headerDigest, headerErr :=
		k12.CanonicalRecognitionLayoutPlanHeaderV2(
			headerEnvelope.RecognitionLayoutPlanHeaderV2,
		)
	if headerErr != nil || headerEnvelope.Contract != "recognition_layout_plan_header_v2" ||
		string(canonicalHeader) != row.LayoutHeaderJSON ||
		headerDigest != row.HeaderDigest {
		return fmt.Errorf("layout header digest is not reproducible: %v", headerErr)
	}
	header := headerEnvelope.RecognitionLayoutPlanHeaderV2
	if header.PlanID != row.PlanID ||
		header.ParentInvocationID != row.ParentInvocationID ||
		header.AgentName != row.AgentName || header.JobID != row.JobID ||
		header.PageDigest != row.PageDigest ||
		header.ParentRequestDigest != parent.RequestDigest ||
		header.RouteSnapshot != parent.RouteSnapshot ||
		header.RequestPolicySnapshot != parent.RequestPolicySnapshot ||
		header.StageStartedAtUnixMillis != row.StageStartedAt ||
		header.EffectiveConcurrency != row.EffectiveConcurrency {
		return errors.New("layout header/plan/parent cross-table facts drifted")
	}
	var plan k12.RecognitionLayoutPlanV2
	if err := json.Unmarshal([]byte(row.AuthorizedPlanJSON), &plan); err != nil {
		return fmt.Errorf("decode authorized layout plan: %w", err)
	}
	encodedPlan, planErr := json.Marshal(plan)
	if planErr != nil || string(encodedPlan) != row.AuthorizedPlanJSON ||
		k12.ValidateRecognitionLayoutPlanV2(plan) != nil {
		return fmt.Errorf("authorized layout plan is not canonical: %v", planErr)
	}
	if plan.PageDigest != row.PageDigest ||
		plan.ManifestInvocationID != row.ManifestPhysicalInvocationID ||
		plan.ManifestResultDigest != row.ManifestResultDigest ||
		plan.AuthorizedPlanDigest != row.AuthorizedPlanDigest {
		return errors.New("authorized plan identity/digest drifted")
	}
	targetIDs := make([]string, len(plan.Targets))
	for index, target := range plan.Targets {
		targetIDs[index] = target.TargetID
	}
	wantCandidateSet, candidateSetErr := k12.RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	if candidateSetErr != nil || wantCandidateSet != row.CandidateExactSetDigest {
		return fmt.Errorf("candidate exact-set digest drifted: %v", candidateSetErr)
	}
	bucket, duration, budgetErr := header.BudgetBuckets.Select(len(plan.Targets))
	if budgetErr != nil || bucket != row.SelectedBucketMaxProblems ||
		row.StageDeadlineAt != row.StageStartedAt+duration {
		return fmt.Errorf("layout selected budget/deadline drifted: %v", budgetErr)
	}
	if len(aggregate.Candidates) != len(plan.Targets) ||
		len(aggregate.Batches) != len(plan.Batches) ||
		len(aggregate.BatchSettlements) != len(plan.Batches) {
		return errors.New("layout plan/candidate/batch exact-set is incomplete")
	}
	candidateRows := make(map[string]ProblemSourceArchiveRecognitionLayoutCandidate)
	for index, target := range plan.Targets {
		candidate := aggregate.Candidates[index]
		candidateJSON, _ := json.Marshal(target)
		if candidate.PlanID != row.PlanID || candidate.CandidateID != target.TargetID ||
			candidate.Ordinal != index+1 || candidate.BBoxX != target.Region.X ||
			candidate.BBoxY != target.Region.Y ||
			candidate.BBoxWidth != target.Region.Width ||
			candidate.BBoxHeight != target.Region.Height ||
			candidate.CropDigest != target.CropDigest ||
			candidate.CandidateJSON != string(candidateJSON) {
			return fmt.Errorf("layout candidate %d drifted", index+1)
		}
		candidateRows[candidate.CandidateID] = candidate
	}
	memberRows := make(map[string][]ProblemSourceArchiveRecognitionLayoutBatchMember)
	for _, member := range aggregate.BatchMembers {
		if member.PlanID != row.PlanID {
			return errors.New("layout batch member plan drifted")
		}
		memberRows[member.BatchID] = append(memberRows[member.BatchID], member)
	}
	batchRows := make(map[string]ProblemSourceArchiveRecognitionLayoutBatch)
	batchSettlements := make(map[string]ProblemSourceArchiveRecognitionLayoutBatchSettlement)
	for _, settlement := range aggregate.BatchSettlements {
		if _, duplicate := batchSettlements[settlement.BatchID]; duplicate {
			return errors.New("duplicate layout batch settlement")
		}
		batchSettlements[settlement.BatchID] = settlement
	}
	memberCount := 0
	for index, batch := range plan.Batches {
		stored := aggregate.Batches[index]
		batchID := string(batch.Unit)
		batchDigest, batchDigestErr := recognitionLayoutBatchDigestV2(batch)
		if batchDigestErr != nil || stored.PlanID != row.PlanID || stored.BatchID != batchID ||
			stored.Ordinal != index+1 || stored.PhysicalUnit != batchID ||
			stored.MemberCount != len(batch.TargetIDs) ||
			stored.BatchDigest != batchDigest || stored.InputDigest != batch.InputDigest {
			return fmt.Errorf("layout batch %d drifted: %v", index+1, batchDigestErr)
		}
		if _, duplicate := batchRows[stored.BatchID]; duplicate {
			return errors.New("duplicate layout batch")
		}
		batchRows[stored.BatchID] = stored
		members := memberRows[batchID]
		if len(members) != len(batch.TargetIDs) {
			return fmt.Errorf("layout batch %s member exact-set drifted", batchID)
		}
		sort.Slice(members, func(i, j int) bool { return members[i].Slot < members[j].Slot })
		for slot, targetID := range batch.TargetIDs {
			if members[slot].Slot != slot || members[slot].CandidateID != targetID {
				return fmt.Errorf("layout batch %s member order drifted", batchID)
			}
		}
		memberCount += len(members)
	}
	if memberCount != len(plan.Targets) || len(aggregate.BatchMembers) != memberCount {
		return errors.New("layout batch member exact-set is incomplete")
	}

	candidateResults := make(map[string]ProblemSourceArchiveRecognitionLayoutCandidateResult)
	for _, result := range aggregate.CandidateResults {
		if result.PlanID != row.PlanID || result.ParentInvocationID != parent.InvocationID ||
			!json.Valid([]byte(result.ResultJSON)) {
			return errors.New("layout candidate result scope/JSON drifted")
		}
		if _, duplicate := candidateResults[result.CandidateID]; duplicate {
			return errors.New("duplicate layout candidate result")
		}
		candidateResults[result.CandidateID] = result
	}
	if len(candidateResults) != len(plan.Targets) {
		return errors.New("layout candidate result exact-set is incomplete")
	}
	repairAuthorizations := make(map[string]ProblemSourceArchiveRecognitionLayoutRepairAuthorization)
	for _, authorization := range aggregate.RepairAuthorizations {
		if authorization.PlanID != row.PlanID || authorization.RepairRound != 1 {
			return errors.New("layout repair authorization scope/round drifted")
		}
		if _, duplicate := repairAuthorizations[authorization.CandidateID]; duplicate {
			return errors.New("duplicate layout repair authorization")
		}
		repairAuthorizations[authorization.CandidateID] = authorization
	}
	repairSettlements := make(map[string]ProblemSourceArchiveRecognitionLayoutRepairSettlement)
	for _, settlement := range aggregate.RepairSettlements {
		if settlement.PlanID != row.PlanID || settlement.ParentInvocationID != parent.InvocationID {
			return errors.New("layout repair settlement scope drifted")
		}
		if _, duplicate := repairSettlements[settlement.CandidateID]; duplicate {
			return errors.New("duplicate layout repair settlement")
		}
		repairSettlements[settlement.CandidateID] = settlement
	}
	for _, batch := range plan.Batches {
		batchID := string(batch.Unit)
		stored, ok := batchSettlements[batchID]
		if !ok || stored.PlanID != row.PlanID ||
			stored.ParentInvocationID != parent.InvocationID ||
			stored.SourcePhysicalUnit != batchID ||
			stored.Classification != string(k12.RecognitionLayoutBatchClassifiedV2) ||
			stored.AmbiguityKind != "" {
			return fmt.Errorf("layout batch settlement %s drifted", batchID)
		}
		batchInput := k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			SourcePhysicalInvocationID: stored.SourcePhysicalInvocationID,
			SourcePhysicalUnit:         k12.RecognitionPhysicalUnit(stored.SourcePhysicalUnit),
			SourcePhysicalResultDigest: stored.SourcePhysicalResultDigest,
			Classification:             k12.RecognitionLayoutBatchClassifiedV2,
			Candidates:                 make([]k12.RecognitionLayoutCandidateSettlementV2, 0, len(batch.TargetIDs)),
		}
		for _, candidateID := range batch.TargetIDs {
			result, ok := candidateResults[candidateID]
			if !ok {
				return errors.New("layout batch candidate result missing")
			}
			authorization, repaired := repairAuthorizations[candidateID]
			if !repaired {
				if result.SourcePhysicalInvocationID != stored.SourcePhysicalInvocationID ||
					result.SourcePhysicalResultDigest != stored.SourcePhysicalResultDigest {
					return errors.New("primary candidate result source drifted")
				}
				candidate := k12.RecognitionLayoutCandidateSettlementV2{
					CandidateID:    candidateID,
					Classification: k12.RecognitionLayoutCandidateValidV2,
					ResultKind:     k12.RecognitionLayoutCandidateResultKindV2(result.ResultKind),
					ResultJSON:     json.RawMessage(result.ResultJSON),
				}
				wantDigest, err := recognitionLayoutCandidateResultDigestV2(
					parent.InvocationID,
					batchInput,
					candidate,
				)
				if err != nil || wantDigest != result.ResultDigest {
					return fmt.Errorf("primary candidate result digest drifted: %v", err)
				}
				batchInput.Candidates = append(batchInput.Candidates, candidate)
				continue
			}
			if authorization.SourceBatchID != batchID ||
				authorization.SourceBatchPhysicalInvocationID != stored.SourcePhysicalInvocationID ||
				authorization.SourceBatchResultDigest != stored.SourcePhysicalResultDigest {
				return errors.New("repair authorization source batch drifted")
			}
			var original k12.RecognitionLayoutCandidateClassificationV2
			for _, classification := range []k12.RecognitionLayoutCandidateClassificationV2{
				k12.RecognitionLayoutCandidateMissingV2,
				k12.RecognitionLayoutCandidateInvalidV2,
			} {
				candidate := k12.RecognitionLayoutCandidateSettlementV2{
					CandidateID:    candidateID,
					Classification: classification,
				}
				digest, digestErr := recognitionLayoutRepairAuthorizationDigestV2(
					batchInput,
					batchID,
					candidate,
					k12.RecognitionPhysicalUnit(authorization.RepairPhysicalUnit),
				)
				if digestErr == nil && digest == authorization.AuthorizationDigest &&
					recognitionLayoutRepairAuthorizationIDV2(digest) == authorization.RepairAuthorizationID {
					original = classification
					break
				}
			}
			if original == "" {
				return errors.New("repair authorization digest is not reproducible")
			}
			batchInput.Candidates = append(batchInput.Candidates, k12.RecognitionLayoutCandidateSettlementV2{
				CandidateID:    candidateID,
				Classification: original,
			})
			repair, ok := repairSettlements[candidateID]
			if !ok || repair.RepairAuthorizationID != authorization.RepairAuthorizationID ||
				repair.AuthorizationDigest != authorization.AuthorizationDigest ||
				repair.SourcePhysicalUnit != authorization.RepairPhysicalUnit ||
				repair.Classification != string(k12.RecognitionLayoutCandidateValidV2) ||
				result.SourcePhysicalInvocationID != repair.SourcePhysicalInvocationID ||
				result.SourcePhysicalResultDigest != repair.SourcePhysicalResultDigest ||
				result.ResultKind != repair.ResultKind || result.ResultDigest != repair.ResultDigest {
				return errors.New("repair settlement/candidate result cross-table facts drifted")
			}
			repairInput := k12.RecognitionLayoutRepairSettlementV2{
				PlanDigest:                 plan.AuthorizedPlanDigest,
				AuthorizationID:            repair.RepairAuthorizationID,
				AuthorizationDigest:        repair.AuthorizationDigest,
				CandidateID:                candidateID,
				SourcePhysicalInvocationID: repair.SourcePhysicalInvocationID,
				SourcePhysicalUnit:         k12.RecognitionPhysicalUnit(repair.SourcePhysicalUnit),
				SourcePhysicalResultDigest: repair.SourcePhysicalResultDigest,
				Classification:             k12.RecognitionLayoutCandidateValidV2,
				ResultKind:                 k12.RecognitionLayoutCandidateResultKindV2(repair.ResultKind),
				ResultJSON:                 json.RawMessage(result.ResultJSON),
			}
			wantResultDigest, err := recognitionLayoutRepairCandidateResultDigestV2(
				parent.InvocationID,
				repairInput,
			)
			if err != nil || wantResultDigest != result.ResultDigest {
				return fmt.Errorf("repair candidate result digest drifted: %v", err)
			}
			wantSettlementDigest, err := recognitionLayoutRepairSettlementDigestV2(
				parent.InvocationID,
				repairInput,
			)
			if err != nil || wantSettlementDigest != repair.SettlementDigest {
				return fmt.Errorf("repair settlement digest drifted: %v", err)
			}
		}
		wantSettlementDigest, err := recognitionLayoutPrimaryBatchSettlementDigestV2(
			parent.InvocationID,
			batchInput,
			batchID,
		)
		if err != nil || wantSettlementDigest != stored.SettlementDigest {
			return fmt.Errorf("batch settlement digest drifted: %v", err)
		}
	}
	if len(repairSettlements) != len(repairAuthorizations) {
		return errors.New("repair authorization/settlement exact-set drifted")
	}

	finalRow := aggregate.Finalization
	var finalEnvelope struct {
		Contract                       string `json:"contract"`
		PlanID                         string `json:"plan_id"`
		ParentInvocationID             string `json:"parent_invocation_id"`
		PlanDigest                     string `json:"plan_digest"`
		CandidateExactSetDigest        string `json:"candidate_exact_set_digest"`
		CandidateResultsExactSetDigest string `json:"candidate_results_exact_set_digest"`
		PhysicalResultsExactSetDigest  string `json:"physical_results_exact_set_digest"`
		CandidateResultCount           int    `json:"candidate_result_count"`
		PhysicalResultCount            int    `json:"physical_result_count"`
	}
	if err := json.Unmarshal([]byte(finalRow.FinalizationJSON), &finalEnvelope); err != nil {
		return fmt.Errorf("decode layout finalization: %w", err)
	}
	finalized := k12.RecognitionLayoutPlanFinalizationResultV2{
		PlanID:                         row.PlanID,
		PlanDigest:                     row.AuthorizedPlanDigest,
		CandidateExactSetDigest:        row.CandidateExactSetDigest,
		CandidateResultsExactSetDigest: finalRow.CandidateResultsExactSetDigest,
		PhysicalResultsExactSetDigest:  finalRow.PhysicalResultsExactSetDigest,
		CandidateResultCount:           finalRow.CandidateResultCount,
		PhysicalResultCount:            finalRow.PhysicalResultCount,
		FinalizationDigest:             finalRow.FinalizationDigest,
	}
	for _, target := range plan.Targets {
		stored := candidateResults[target.TargetID]
		child, ok := physical[stored.SourcePhysicalInvocationID]
		if !ok {
			return errors.New("finalized candidate source child is missing")
		}
		finalized.CandidateResults = append(
			finalized.CandidateResults,
			k12.RecognitionLayoutCandidateFinalResultV2{
				CandidateID:                target.TargetID,
				ResultKind:                 k12.RecognitionLayoutCandidateResultKindV2(stored.ResultKind),
				ResultDigest:               stored.ResultDigest,
				ResultJSON:                 json.RawMessage(stored.ResultJSON),
				SourcePhysicalInvocationID: stored.SourcePhysicalInvocationID,
				SourcePhysicalUnit:         child.PhysicalUnit,
				SourcePhysicalResultDigest: stored.SourcePhysicalResultDigest,
			},
		)
	}
	physicalOrder := []string{row.ManifestPhysicalInvocationID}
	for _, batch := range plan.Batches {
		physicalOrder = append(
			physicalOrder,
			batchSettlements[string(batch.Unit)].SourcePhysicalInvocationID,
		)
	}
	for _, target := range plan.Targets {
		if _, repaired := repairAuthorizations[target.TargetID]; repaired {
			physicalOrder = append(
				physicalOrder,
				repairSettlements[target.TargetID].SourcePhysicalInvocationID,
			)
		}
	}
	for _, physicalID := range physicalOrder {
		child, ok := physical[physicalID]
		if !ok {
			return errors.New("finalized physical child is missing")
		}
		finalized.PhysicalResults = append(
			finalized.PhysicalResults,
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
	canonicalFinalization, finalizationDigest, finalizationErr :=
		k12.CanonicalRecognitionLayoutPlanFinalizationV2(
			parent.InvocationID,
			finalized,
		)
	if finalizationErr != nil || finalEnvelope.Contract != "recognition_layout_plan_finalization_v2" ||
		string(canonicalFinalization) != finalRow.FinalizationJSON ||
		finalizationDigest != finalRow.FinalizationDigest {
		return fmt.Errorf("layout finalization digest is not reproducible: %v", finalizationErr)
	}
	if finalRow.PlanID != row.PlanID ||
		finalRow.ParentInvocationID != parent.InvocationID ||
		finalRow.AuthorizedPlanDigest != row.AuthorizedPlanDigest ||
		finalRow.CandidateExactSetDigest != row.CandidateExactSetDigest ||
		finalRow.CandidateResultsExactSetDigest != finalized.CandidateResultsExactSetDigest ||
		finalRow.PhysicalResultsExactSetDigest != finalized.PhysicalResultsExactSetDigest ||
		finalRow.CandidateResultCount != len(plan.Targets) ||
		finalRow.CandidateResultCount != finalized.CandidateResultCount ||
		finalRow.PhysicalResultCount != finalized.PhysicalResultCount ||
		finalEnvelope.PlanID != row.PlanID ||
		finalEnvelope.ParentInvocationID != parent.InvocationID ||
		finalEnvelope.PlanDigest != row.AuthorizedPlanDigest ||
		finalEnvelope.CandidateExactSetDigest != row.CandidateExactSetDigest ||
		finalEnvelope.CandidateResultsExactSetDigest != finalRow.CandidateResultsExactSetDigest ||
		finalEnvelope.PhysicalResultsExactSetDigest != finalRow.PhysicalResultsExactSetDigest ||
		finalEnvelope.CandidateResultCount != finalRow.CandidateResultCount ||
		finalEnvelope.PhysicalResultCount != finalRow.PhysicalResultCount {
		return errors.New("layout finalization aggregate facts drifted")
	}
	if len(finalized.CandidateResults) != len(plan.Targets) ||
		len(finalized.PhysicalResults) != finalRow.PhysicalResultCount {
		return errors.New("layout finalization exact-set is incomplete")
	}
	for index, target := range plan.Targets {
		result := finalized.CandidateResults[index]
		stored := candidateResults[target.TargetID]
		if result.CandidateID != target.TargetID ||
			result.ResultKind != k12.RecognitionLayoutCandidateResultKindV2(stored.ResultKind) ||
			result.ResultDigest != stored.ResultDigest ||
			string(result.ResultJSON) != stored.ResultJSON ||
			result.SourcePhysicalInvocationID != stored.SourcePhysicalInvocationID ||
			result.SourcePhysicalResultDigest != stored.SourcePhysicalResultDigest {
			return fmt.Errorf("finalized candidate %d drifted from result table", index+1)
		}
	}
	finalPhysical := make(map[string]struct{}, len(finalized.PhysicalResults))
	for _, evidence := range finalized.PhysicalResults {
		child, ok := physical[evidence.PhysicalInvocationID]
		if !ok || child.ParentInvocationID != parent.InvocationID ||
			child.AgentName != agentName || child.JobID != parent.JobID ||
			child.Stage != k12.GradingStageRecognizing ||
			child.Status != k12.ModelInvocationSucceeded || child.Attempt != 1 ||
			child.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
			child.PhysicalUnit != evidence.PhysicalUnit ||
			child.ResultDigest != evidence.ResultDigest ||
			child.PlanDigest != evidence.PlanDigest ||
			child.CandidateExactSetDigest != evidence.CandidateExactSetDigest {
			return errors.New("finalized physical exact-set drifted from child ledger")
		}
		finalPhysical[evidence.PhysicalInvocationID] = struct{}{}
	}
	for _, child := range physical {
		if child.ParentInvocationID == parent.InvocationID {
			if _, ok := finalPhysical[child.PhysicalInvocationID]; !ok {
				return errors.New("layout parent carries a physical child outside final exact-set")
			}
		}
	}
	return nil
}

func insertProblemSourceArchiveRecognitionLayoutsV2(
	ctx context.Context,
	tx *sql.Tx,
	layouts []ProblemSourceArchiveRecognitionLayoutV2,
) error {
	for _, aggregate := range layouts {
		v := aggregate.Plan
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_recognition_layout_plans (
			plan_id,parent_invocation_id,agent_name,job_id,stage,
			manifest_physical_invocation_id,page_digest,header_digest,
			manifest_result_digest,authorized_plan_digest,candidate_exact_set_digest,
			layout_header_json,authorized_plan_json,stage_started_at,stage_deadline_at,
			selected_bucket_max_problems,effective_concurrency,status,created_at,updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			v.PlanID, v.ParentInvocationID, v.AgentName, v.JobID, v.Stage,
			v.ManifestPhysicalInvocationID, v.PageDigest, v.HeaderDigest,
			v.ManifestResultDigest, v.AuthorizedPlanDigest, v.CandidateExactSetDigest,
			v.LayoutHeaderJSON, v.AuthorizedPlanJSON, v.StageStartedAt,
			v.StageDeadlineAt, v.SelectedBucketMaxProblems, v.EffectiveConcurrency,
			"running", v.CreatedAt, v.UpdatedAt,
		); err != nil {
			return fmt.Errorf("import recognition layout plan: %w", err)
		}
		for _, row := range aggregate.Candidates {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_recognition_layout_candidates (
				plan_id,candidate_id,ordinal,bbox_x,bbox_y,bbox_width,bbox_height,
				crop_digest,candidate_json,created_at
			) VALUES (?,?,?,?,?,?,?,?,?,?)`, row.PlanID, row.CandidateID, row.Ordinal,
				row.BBoxX, row.BBoxY, row.BBoxWidth, row.BBoxHeight, row.CropDigest,
				row.CandidateJSON, row.CreatedAt); err != nil {
				return fmt.Errorf("import recognition layout candidate: %w", err)
			}
		}
		for _, row := range aggregate.Batches {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_recognition_layout_batches (
				plan_id,batch_id,ordinal,physical_unit,member_count,batch_digest,
				input_digest,created_at
			) VALUES (?,?,?,?,?,?,?,?)`, row.PlanID, row.BatchID, row.Ordinal,
				row.PhysicalUnit, row.MemberCount, row.BatchDigest, row.InputDigest,
				row.CreatedAt); err != nil {
				return fmt.Errorf("import recognition layout batch: %w", err)
			}
		}
		for _, row := range aggregate.BatchMembers {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_recognition_layout_batch_members (
				plan_id,batch_id,slot,candidate_id,created_at
			) VALUES (?,?,?,?,?)`, row.PlanID, row.BatchID, row.Slot,
				row.CandidateID, row.CreatedAt); err != nil {
				return fmt.Errorf("import recognition layout batch member: %w", err)
			}
		}
		for _, row := range aggregate.BatchSettlements {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_recognition_layout_batch_settlements (
				plan_id,batch_id,parent_invocation_id,source_physical_invocation_id,
				source_physical_unit,source_physical_result_digest,classification,
				ambiguity_kind,settlement_digest,created_at
			) VALUES (?,?,?,?,?,?,?,?,?,?)`, row.PlanID, row.BatchID,
				row.ParentInvocationID, row.SourcePhysicalInvocationID,
				row.SourcePhysicalUnit, row.SourcePhysicalResultDigest,
				row.Classification, row.AmbiguityKind, row.SettlementDigest,
				row.CreatedAt); err != nil {
				return fmt.Errorf("import recognition layout batch settlement: %w", err)
			}
		}
		for _, row := range aggregate.RepairAuthorizations {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_recognition_layout_repair_authorizations (
				plan_id,repair_authorization_id,repair_physical_unit,candidate_id,
				source_batch_id,source_batch_physical_invocation_id,
				source_batch_result_digest,repair_round,authorization_digest,created_at
			) VALUES (?,?,?,?,?,?,?,?,?,?)`, row.PlanID, row.RepairAuthorizationID,
				row.RepairPhysicalUnit, row.CandidateID, row.SourceBatchID,
				row.SourceBatchPhysicalInvocationID, row.SourceBatchResultDigest,
				row.RepairRound, row.AuthorizationDigest, row.CreatedAt); err != nil {
				return fmt.Errorf("import recognition layout repair authorization: %w", err)
			}
		}
		for _, row := range aggregate.RepairSettlements {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_recognition_layout_repair_settlements (
				plan_id,repair_authorization_id,authorization_digest,candidate_id,
				parent_invocation_id,source_physical_invocation_id,source_physical_unit,
				source_physical_result_digest,classification,result_kind,result_digest,
				settlement_digest,created_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.PlanID,
				row.RepairAuthorizationID, row.AuthorizationDigest, row.CandidateID,
				row.ParentInvocationID, row.SourcePhysicalInvocationID,
				row.SourcePhysicalUnit, row.SourcePhysicalResultDigest,
				row.Classification, row.ResultKind, row.ResultDigest,
				row.SettlementDigest, row.CreatedAt); err != nil {
				return fmt.Errorf("import recognition layout repair settlement: %w", err)
			}
		}
		for _, row := range aggregate.CandidateResults {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_recognition_layout_candidate_results (
				plan_id,candidate_id,parent_invocation_id,source_physical_invocation_id,
				source_physical_result_digest,result_kind,result_digest,result_json,created_at
			) VALUES (?,?,?,?,?,?,?,?,?)`, row.PlanID, row.CandidateID,
				row.ParentInvocationID, row.SourcePhysicalInvocationID,
				row.SourcePhysicalResultDigest, row.ResultKind, row.ResultDigest,
				row.ResultJSON, row.CreatedAt); err != nil {
				return fmt.Errorf("import recognition layout candidate result: %w", err)
			}
		}
		row := aggregate.Finalization
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_recognition_layout_finalizations (
			plan_id,parent_invocation_id,authorized_plan_digest,candidate_exact_set_digest,
			candidate_results_exact_set_digest,physical_results_exact_set_digest,
			candidate_result_count,physical_result_count,finalization_json,
			finalization_digest,created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, row.PlanID, row.ParentInvocationID,
			row.AuthorizedPlanDigest, row.CandidateExactSetDigest,
			row.CandidateResultsExactSetDigest, row.PhysicalResultsExactSetDigest,
			row.CandidateResultCount, row.PhysicalResultCount,
			row.FinalizationJSON, row.FinalizationDigest, row.CreatedAt); err != nil {
			return fmt.Errorf("import recognition layout finalization: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE k12_recognition_layout_plans
			SET status='succeeded' WHERE plan_id=? AND status='running'`,
			aggregate.Plan.PlanID,
		); err != nil {
			return fmt.Errorf("finish imported recognition layout plan: %w", err)
		}
	}
	return nil
}
