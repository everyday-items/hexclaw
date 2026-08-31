//go:build testtools

package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// RecognitionV2FinalizationEvidenceClaim 是 installed-app 测试控制器提供的私有、
// profile-local 目标身份。它有意不包含计划或调用身份；这些身份必须通过持久业务血缘发现。
type RecognitionV2FinalizationEvidenceClaim struct {
	TargetAgent     string
	DispatchID      string
	SourceSessionID string
	SourceDigest    string
}

type RecognitionV2FinalizationEvidenceBatch struct {
	Ordinal                 int
	PhysicalUnit            k12.RecognitionPhysicalUnit
	CandidateCount          int
	CandidateExactSetDigest string
}

type RecognitionV2FinalizationEvidenceRepair struct {
	CandidateID             string
	CandidateOrdinal        int
	PhysicalUnit            k12.RecognitionPhysicalUnit
	RepairRound             int
	AuthorizationDigest     string
	SettlementDigest        string
	CandidateExactSetDigest string
}

type RecognitionV2FinalizationEvidencePhysical struct {
	Ordinal                 int
	PhysicalInvocationID    string
	PhysicalUnit            k12.RecognitionPhysicalUnit
	Provider                string
	Model                   string
	Status                  k12.ModelInvocationStatus
	Attempt                 int
	ResultDigest            string
	RecognitionPlanVersion  int
	PlanDigest              string
	CandidateExactSetDigest string
}

// RecognitionV2FinalizationEvidenceSnapshot 仅包含调用方计算摘要所需的控制面事实、
// 精确集合摘要和不透明身份。它有意排除源字节、裁剪图片、prompt、Provider payload
// 和私有候选结果 JSON。
type RecognitionV2FinalizationEvidenceSnapshot struct {
	FixtureAgentMetadata           map[string]string
	TargetAgent                    string
	DispatchID                     string
	SourceSessionID                string
	SourceDigest                   string
	SubmissionID                   string
	JobID                          string
	ParentInvocationID             string
	PlanID                         string
	RecognitionPlanVersion         int
	PlanStatus                     string
	ParentStatus                   k12.ModelInvocationStatus
	ParentAttempt                  int
	Provider                       string
	Model                          string
	HeaderDigest                   string
	AuthorizedPlanDigest           string
	CandidateExactSetDigest        string
	CandidateResultsExactSetDigest string
	PhysicalResultsExactSetDigest  string
	FinalizationDigest             string
	StageStartedAtUnixMillis       int64
	StageDeadlineAtUnixMillis      int64
	SelectedBucketMaxProblems      int
	BudgetBuckets                  k12.RecognitionLayoutBudgetBucketsV2
	PhysicalCallCapMillis          int64
	AdapterWorkerHardCap           int
	EffectiveConcurrency           int
	CandidateResultCount           int
	QuestionCount                  int
	NonQuestionCount               int
	PhysicalResultCount            int
	AuthorizedBatches              []RecognitionV2FinalizationEvidenceBatch
	AuthorizedRepairs              []RecognitionV2FinalizationEvidenceRepair
	PhysicalReceipts               []RecognitionV2FinalizationEvidencePhysical
}

// LoadRecognitionV2FinalizationEvidenceSnapshotForTesttools 在单个只读事务中重建一个
// 已终态化 V2 计划，随后调用公开 runtime 和终态重放 API，并要求精确只读重放
// （created=false）。发布构建会排除本文件。
func (s *Store) LoadRecognitionV2FinalizationEvidenceSnapshotForTesttools(
	ctx context.Context,
	fixtureAgent string,
	claim RecognitionV2FinalizationEvidenceClaim,
) (RecognitionV2FinalizationEvidenceSnapshot, error) {
	fixtureAgent = strings.TrimSpace(fixtureAgent)
	claim.TargetAgent = strings.TrimSpace(claim.TargetAgent)
	claim.DispatchID = strings.TrimSpace(claim.DispatchID)
	claim.SourceSessionID = strings.TrimSpace(claim.SourceSessionID)
	claim.SourceDigest = strings.TrimSpace(claim.SourceDigest)
	if s == nil || s.db == nil || fixtureAgent == "" || claim.TargetAgent == "" ||
		claim.DispatchID == "" || claim.SourceSessionID == "" || claim.SourceDigest == "" {
		return RecognitionV2FinalizationEvidenceSnapshot{}, errors.New(
			"k12storage: recognition evidence identity is incomplete",
		)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
		ReadOnly:  true,
	})
	if err != nil {
		return RecognitionV2FinalizationEvidenceSnapshot{}, fmt.Errorf(
			"k12storage: begin recognition evidence snapshot: %w",
			err,
		)
	}
	defer tx.Rollback()

	snapshot, runtime, finalization, err := s.loadRecognitionV2FinalizationEvidenceSnapshotTx(
		ctx,
		tx,
		fixtureAgent,
		claim,
	)
	if err != nil {
		return RecognitionV2FinalizationEvidenceSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecognitionV2FinalizationEvidenceSnapshot{}, fmt.Errorf(
			"k12storage: finish recognition evidence snapshot: %w",
			err,
		)
	}

	replayedRuntime, err := s.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		claim.TargetAgent,
		snapshot.ParentInvocationID,
	)
	if err != nil || !reflect.DeepEqual(replayedRuntime, runtime) {
		return RecognitionV2FinalizationEvidenceSnapshot{}, fmt.Errorf(
			"%w: public recognition runtime replay drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	replayedFinalization, created, err := s.FinalizeRecognitionLayoutPlanV2(
		ctx,
		claim.TargetAgent,
		snapshot.ParentInvocationID,
	)
	if err != nil || created || !reflect.DeepEqual(replayedFinalization, finalization) {
		return RecognitionV2FinalizationEvidenceSnapshot{}, fmt.Errorf(
			"%w: public recognition finalization replay was not exact and read-only",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return snapshot, nil
}

func (s *Store) loadRecognitionV2FinalizationEvidenceSnapshotTx(
	ctx context.Context,
	tx *sql.Tx,
	fixtureAgent string,
	claim RecognitionV2FinalizationEvidenceClaim,
) (
	RecognitionV2FinalizationEvidenceSnapshot,
	k12.RecognitionLayoutPlanRuntimeV2,
	k12.RecognitionLayoutPlanFinalizationResultV2,
	error,
) {
	var metadataJSON string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT metadata FROM agents WHERE name=?`,
		fixtureAgent,
	).Scan(&metadataJSON); err != nil {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: fixture agent identity is absent")
	}
	metadata := make(map[string]string)
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil || len(metadata) == 0 {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: fixture agent metadata is invalid")
	}

	dispatch, err := getImageTaskDispatch(
		ctx,
		tx,
		claim.TargetAgent,
		claim.DispatchID,
		"",
	)
	if err != nil || dispatch.AgentName != claim.TargetAgent ||
		dispatch.DispatchID != claim.DispatchID ||
		dispatch.SourceSessionID != claim.SourceSessionID ||
		dispatch.SourceDigest != claim.SourceDigest ||
		dispatch.TargetObjectType != k12.ImageTaskTargetHomeworkSubmission ||
		strings.TrimSpace(dispatch.TargetObjectID) == "" {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			fmt.Errorf("%w: target dispatch lineage drifted", ErrImageTaskConflict)
	}
	submission, err := getHomeworkSubmission(
		ctx,
		tx,
		claim.TargetAgent,
		dispatch.TargetObjectID,
	)
	if err != nil || submission.AgentName != claim.TargetAgent ||
		submission.DispatchID != claim.DispatchID ||
		submission.SubmissionID != dispatch.TargetObjectID ||
		strings.TrimSpace(submission.GradingJobID) == "" {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			fmt.Errorf("%w: homework submission lineage drifted", ErrImageTaskConflict)
	}
	jobs, err := s.queryRecordsVia(
		ctx,
		tx,
		gradingJobMapper{},
		"WHERE record_id=? AND agent_name=?",
		submission.GradingJobID,
		claim.TargetAgent,
	)
	if err != nil || len(jobs) != 1 {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			fmt.Errorf("%w: grading job lineage drifted", records.ErrNotFound)
	}
	job := jobs[0]
	if job == nil || job.RecordID != submission.GradingJobID ||
		job.AgentName != claim.TargetAgent || job.Collection != k12.CollectionGradingJob {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			fmt.Errorf("%w: grading job lineage drifted", records.ErrNotFound)
	}
	jobFields, err := k12.ParseGradingJobFields(job.Fields)
	if err != nil || job.SourceSession != claim.SourceSessionID ||
		jobFields.SubmissionID != submission.SubmissionID ||
		jobFields.SourceKind != "image_task" {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: grading job fields drifted")
	}

	var parentCount int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_model_invocations
		  WHERE agent_name=? AND job_id=? AND stage=?`,
		claim.TargetAgent,
		job.RecordID,
		k12.GradingStageRecognizing,
	).Scan(&parentCount); err != nil || parentCount != 1 {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: recognizing parent lineage is not unique")
	}
	var parentID string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT invocation_id FROM k12_model_invocations
		  WHERE agent_name=? AND job_id=? AND stage=?`,
		claim.TargetAgent,
		job.RecordID,
		k12.GradingStageRecognizing,
	).Scan(&parentID); err != nil {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: recognizing parent lineage is absent")
	}

	var planCount int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_plans
		  WHERE parent_invocation_id=? AND agent_name=?`,
		parentID,
		claim.TargetAgent,
	).Scan(&planCount); err != nil || planCount != 1 {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: recognition V2 plan lineage is not unique")
	}
	authority, err := loadRecognitionLayoutFinalizationAuthorityV2(
		ctx,
		tx,
		claim.TargetAgent,
		parentID,
	)
	if err != nil || authority.Status != "succeeded" ||
		authority.Parent.Status != k12.ModelInvocationSucceeded ||
		authority.Parent.Attempt != 1 {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: recognition V2 plan is not finalized")
	}
	jobRoute := k12.NormalizeGradingModelSnapshot(jobFields.ModelSnapshot)
	headerRoute := k12.NormalizeGradingModelSnapshot(authority.Header.RouteSnapshot)
	jobBudget := jobFields.BudgetSnapshot
	if jobRoute != jobFields.ModelSnapshot || headerRoute != authority.Header.RouteSnapshot ||
		jobRoute != headerRoute || authority.Parent.RouteSnapshot != headerRoute ||
		authority.Parent.RequestPolicySnapshot != authority.Header.RequestPolicySnapshot ||
		headerRoute.RecognizingRequestPolicy != authority.Header.RequestPolicySnapshot ||
		k12.ValidateGradingRecognizingRequestPolicy(headerRoute) != nil ||
		jobBudget.Validate() != nil ||
		jobBudget.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		jobBudget.RecognizingBuckets != authority.Header.BudgetBuckets ||
		jobBudget.PhysicalCallCapMillis != authority.Header.PhysicalCallCapMillis ||
		jobBudget.WorkerHardCap != authority.Header.AdapterWorkerHardCap ||
		jobBudget.EffectiveConcurrency != authority.Header.EffectiveConcurrency {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: recognition route or budget snapshot drifted")
	}
	if headerRoute.Provider != "hexclaw-gpt" ||
		headerRoute.Model != k12.RecognizingPolicyModel ||
		headerRoute.Route != "hexclaw-gpt/"+k12.RecognizingPolicyModel ||
		authority.Header.EffectiveConcurrency != 2 ||
		authority.Header.BudgetBuckets.UpTo8ProblemsMillis <
			authority.Header.BudgetBuckets.UpTo1ProblemMillis ||
		authority.Header.BudgetBuckets.UpTo16ProblemsMillis <
			authority.Header.BudgetBuckets.UpTo8ProblemsMillis ||
		authority.Header.BudgetBuckets.UpTo32ProblemsMillis <
			authority.Header.BudgetBuckets.UpTo16ProblemsMillis {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: recognition route is not the approved live target")
	}

	finalization, finalizationJSON, err := reconstructRecognitionLayoutFinalizationV2(
		ctx,
		tx,
		authority,
	)
	if err != nil {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			err
	}
	found, err := validateStoredRecognitionLayoutFinalizationV2(
		ctx,
		tx,
		parentID,
		finalization,
		finalizationJSON,
	)
	if err != nil || !found || authority.Parent.ResultDigest != finalization.FinalizationDigest {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: finalized recognition receipt drifted")
	}
	var totalPhysical, v2Physical int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN recognition_plan_version='v2' THEN 1 ELSE 0 END),0)
		   FROM k12_model_physical_invocations WHERE parent_invocation_id=?`,
		parentID,
	).Scan(&totalPhysical, &v2Physical); err != nil ||
		totalPhysical != finalization.PhysicalResultCount ||
		v2Physical != finalization.PhysicalResultCount {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: recognition parent carries non-V2 or extra physical evidence")
	}

	runtime := k12.RecognitionLayoutPlanRuntimeV2{
		Header:                       authority.Header,
		HeaderDigest:                 authority.HeaderDigest,
		ManifestPhysicalInvocationID: authority.Plan.ManifestInvocationID,
		ManifestResultDigest:         authority.Plan.ManifestResultDigest,
		CandidateExactSetDigest:      authority.CandidateExactSetDigest,
		Status:                       authority.Status,
		AuthorizedPlan:               &authority.Plan,
	}
	runtime.SelectedBucketMaxProblems, _, err = authority.Header.BudgetBuckets.Select(
		len(authority.Plan.Targets),
	)
	if err != nil {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			err
	}
	runtime.StageDeadlineAtUnixMillis = authority.Header.StageStartedAtUnixMillis
	_, durationMillis, _ := authority.Header.BudgetBuckets.Select(len(authority.Plan.Targets))
	runtime.StageDeadlineAtUnixMillis += durationMillis

	batches := make([]RecognitionV2FinalizationEvidenceBatch, 0, len(authority.Plan.Batches))
	for index, batch := range authority.Plan.Batches {
		exactSet, err := k12.RecognitionLayoutTargetExactSetDigestV2(batch.TargetIDs)
		if err != nil {
			return RecognitionV2FinalizationEvidenceSnapshot{},
				k12.RecognitionLayoutPlanRuntimeV2{},
				k12.RecognitionLayoutPlanFinalizationResultV2{},
				err
		}
		batches = append(batches, RecognitionV2FinalizationEvidenceBatch{
			Ordinal:                 index + 1,
			PhysicalUnit:            batch.Unit,
			CandidateCount:          len(batch.TargetIDs),
			CandidateExactSetDigest: exactSet,
		})
	}

	repairs := make([]RecognitionV2FinalizationEvidenceRepair, 0)
	for _, target := range authority.Plan.Targets {
		repair, found, err := loadRecognitionLayoutFinalRepairAuthorizationV2(
			ctx,
			tx,
			authority.PlanID,
			target.TargetID,
		)
		if err != nil {
			return RecognitionV2FinalizationEvidenceSnapshot{},
				k12.RecognitionLayoutPlanRuntimeV2{},
				k12.RecognitionLayoutPlanFinalizationResultV2{},
				err
		}
		if !found {
			continue
		}
		var repairRound int
		var settlementDigest string
		if err := tx.QueryRowContext(
			ctx,
			`SELECT authorization.repair_round,settlement.settlement_digest
			   FROM k12_recognition_layout_repair_authorizations authorization
			   JOIN k12_recognition_layout_repair_settlements settlement
			     ON settlement.plan_id=authorization.plan_id
			    AND settlement.candidate_id=authorization.candidate_id
			    AND settlement.repair_authorization_id=authorization.repair_authorization_id
			  WHERE authorization.plan_id=? AND authorization.candidate_id=?`,
			authority.PlanID,
			repair.CandidateID,
		).Scan(&repairRound, &settlementDigest); err != nil || repairRound != 1 ||
			!validPrefixedSHA256DigestV2(settlementDigest) {
			return RecognitionV2FinalizationEvidenceSnapshot{},
				k12.RecognitionLayoutPlanRuntimeV2{},
				k12.RecognitionLayoutPlanFinalizationResultV2{},
				errors.New("k12storage: recognition repair settlement drifted")
		}
		exactSet, err := k12.RecognitionLayoutTargetExactSetDigestV2(
			[]string{repair.CandidateID},
		)
		if err != nil {
			return RecognitionV2FinalizationEvidenceSnapshot{},
				k12.RecognitionLayoutPlanRuntimeV2{},
				k12.RecognitionLayoutPlanFinalizationResultV2{},
				err
		}
		repairs = append(repairs, RecognitionV2FinalizationEvidenceRepair{
			CandidateID:             repair.CandidateID,
			CandidateOrdinal:        repair.CandidateOrdinal,
			PhysicalUnit:            repair.PhysicalUnit,
			RepairRound:             repairRound,
			AuthorizationDigest:     repair.AuthorizationDigest,
			SettlementDigest:        settlementDigest,
			CandidateExactSetDigest: exactSet,
		})
	}

	physical := make([]RecognitionV2FinalizationEvidencePhysical, 0, len(finalization.PhysicalResults))
	for index, evidence := range finalization.PhysicalResults {
		invocation, err := getModelPhysicalInvocationByIDVia(
			ctx,
			tx,
			claim.TargetAgent,
			evidence.PhysicalInvocationID,
		)
		if err != nil || invocation.ParentInvocationID != parentID ||
			invocation.PhysicalUnit != evidence.PhysicalUnit ||
			invocation.ResultDigest != evidence.ResultDigest ||
			invocation.PlanDigest != evidence.PlanDigest ||
			invocation.CandidateExactSetDigest != evidence.CandidateExactSetDigest {
			return RecognitionV2FinalizationEvidenceSnapshot{},
				k12.RecognitionLayoutPlanRuntimeV2{},
				k12.RecognitionLayoutPlanFinalizationResultV2{},
				errors.New("k12storage: recognition physical projection drifted")
		}
		physical = append(physical, RecognitionV2FinalizationEvidencePhysical{
			Ordinal:                 index + 1,
			PhysicalInvocationID:    invocation.PhysicalInvocationID,
			PhysicalUnit:            invocation.PhysicalUnit,
			Provider:                invocation.RouteSnapshot.Provider,
			Model:                   invocation.RouteSnapshot.Model,
			Status:                  invocation.Status,
			Attempt:                 invocation.Attempt,
			ResultDigest:            invocation.ResultDigest,
			RecognitionPlanVersion:  invocation.RecognitionPlanVersion,
			PlanDigest:              invocation.PlanDigest,
			CandidateExactSetDigest: invocation.CandidateExactSetDigest,
		})
	}

	questionCount, nonQuestionCount := 0, 0
	for _, result := range finalization.CandidateResults {
		switch result.ResultKind {
		case k12.RecognitionLayoutCandidateQuestionV2:
			questionCount++
		case k12.RecognitionLayoutCandidateNonQuestionV2:
			nonQuestionCount++
		default:
			return RecognitionV2FinalizationEvidenceSnapshot{},
				k12.RecognitionLayoutPlanRuntimeV2{},
				k12.RecognitionLayoutPlanFinalizationResultV2{},
				errors.New("k12storage: recognition candidate result kind drifted")
		}
	}
	if finalization.CandidateResultCount != questionCount+nonQuestionCount ||
		finalization.CandidateResultCount != len(authority.Plan.Targets) {
		return RecognitionV2FinalizationEvidenceSnapshot{},
			k12.RecognitionLayoutPlanRuntimeV2{},
			k12.RecognitionLayoutPlanFinalizationResultV2{},
			errors.New("k12storage: recognition candidate result count drifted")
	}

	return RecognitionV2FinalizationEvidenceSnapshot{
		FixtureAgentMetadata:           metadata,
		TargetAgent:                    claim.TargetAgent,
		DispatchID:                     claim.DispatchID,
		SourceSessionID:                claim.SourceSessionID,
		SourceDigest:                   claim.SourceDigest,
		SubmissionID:                   submission.SubmissionID,
		JobID:                          job.RecordID,
		ParentInvocationID:             parentID,
		PlanID:                         authority.PlanID,
		RecognitionPlanVersion:         k12.RecognitionPlanVersionV2,
		PlanStatus:                     authority.Status,
		ParentStatus:                   authority.Parent.Status,
		ParentAttempt:                  authority.Parent.Attempt,
		Provider:                       headerRoute.Provider,
		Model:                          headerRoute.Model,
		HeaderDigest:                   authority.HeaderDigest,
		AuthorizedPlanDigest:           authority.Plan.AuthorizedPlanDigest,
		CandidateExactSetDigest:        authority.CandidateExactSetDigest,
		CandidateResultsExactSetDigest: finalization.CandidateResultsExactSetDigest,
		PhysicalResultsExactSetDigest:  finalization.PhysicalResultsExactSetDigest,
		FinalizationDigest:             finalization.FinalizationDigest,
		StageStartedAtUnixMillis:       authority.Header.StageStartedAtUnixMillis,
		StageDeadlineAtUnixMillis:      runtime.StageDeadlineAtUnixMillis,
		SelectedBucketMaxProblems:      runtime.SelectedBucketMaxProblems,
		BudgetBuckets:                  authority.Header.BudgetBuckets,
		PhysicalCallCapMillis:          authority.Header.PhysicalCallCapMillis,
		AdapterWorkerHardCap:           authority.Header.AdapterWorkerHardCap,
		EffectiveConcurrency:           authority.Header.EffectiveConcurrency,
		CandidateResultCount:           finalization.CandidateResultCount,
		QuestionCount:                  questionCount,
		NonQuestionCount:               nonQuestionCount,
		PhysicalResultCount:            finalization.PhysicalResultCount,
		AuthorizedBatches:              batches,
		AuthorizedRepairs:              repairs,
		PhysicalReceipts:               physical,
	}, runtime, finalization, nil
}
