package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

var ErrGradingFinalizationIncomplete = errors.New("grading page finalization incomplete")

type gradingFinalEntry struct {
	question   RecognizedQuestion
	assessment *k12.GradingAssessmentItem
	skip       *k12.ProblemSkipReceipt
}

func (o *GradingOrchestrator) finalizeGradingPage(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
) (k12.GradingFinalArtifact, error) {
	if existing, err := o.deps.Records.GetGradingFinalArtifactByJob(
		ctx, job.Record.AgentName, job.Record.RecordID,
	); err == nil {
		return existing, nil
	} else if !errors.Is(err, records.ErrNotFound) {
		return k12.GradingFinalArtifact{}, err
	}
	finalizationGeneration, err := o.deps.Records.GetGradingFinalizationGeneration(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, err
	}
	assessments, err := o.deps.Records.ListGradingAssessmentItems(
		ctx, job.Record.AgentName, job.Record.RecordID,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, err
	}
	if terminalErr := o.requireTerminalGradingOperations(
		ctx, job.Record.AgentName, job.Record.RecordID, assessments,
	); terminalErr != nil {
		return k12.GradingFinalArtifact{}, terminalErr
	}
	skips, err := o.deps.Records.ListCurrentProblemSkipReceipts(
		ctx, job.Record.AgentName, job.Record.RecordID,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, err
	}
	assessmentByProblem := make(map[string]k12.GradingAssessmentItem, len(assessments))
	for _, assessment := range assessments {
		if _, duplicate := assessmentByProblem[assessment.ProblemID]; duplicate {
			return k12.GradingFinalArtifact{}, fmt.Errorf(
				"%w: duplicate current assessment for problem %s",
				ErrGradingFinalizationIncomplete, assessment.ProblemID,
			)
		}
		assessmentByProblem[assessment.ProblemID] = assessment
	}
	skipByProblem := make(map[string]k12.ProblemSkipReceipt, len(skips))
	for _, skip := range skips {
		if _, duplicate := skipByProblem[skip.ProblemID]; duplicate {
			return k12.GradingFinalArtifact{}, fmt.Errorf(
				"%w: duplicate current skip for problem %s",
				ErrGradingFinalizationIncomplete, skip.ProblemID,
			)
		}
		skipByProblem[skip.ProblemID] = skip
	}

	entries := make([]gradingFinalEntry, 0, len(run.questions))
	orderedDigests := make([]string, 0, len(run.questions))
	structureVersion := 0
	publishedCount := 0
	skippedCount := 0
	seenProblems := make(map[string]struct{}, len(run.questions))
	for _, question := range run.questions {
		if question.ProblemKind == ProblemKindCompoundParent {
			continue
		}
		problemID := strings.TrimSpace(question.ProblemID)
		if problemID == "" || question.ConfirmedVersion < 1 {
			return k12.GradingFinalArtifact{}, fmt.Errorf(
				"%w: frozen problem lacks identity or confirmed revision",
				ErrGradingFinalizationIncomplete,
			)
		}
		if _, duplicate := seenProblems[problemID]; duplicate {
			return k12.GradingFinalArtifact{}, fmt.Errorf(
				"%w: duplicate frozen problem %s",
				ErrGradingFinalizationIncomplete, problemID,
			)
		}
		seenProblems[problemID] = struct{}{}
		assessment, hasAssessment := assessmentByProblem[problemID]
		skip, hasSkip := skipByProblem[problemID]
		if hasAssessment == hasSkip {
			return k12.GradingFinalArtifact{}, fmt.Errorf(
				"%w: problem %s must have exactly one current assessment or skip",
				ErrGradingFinalizationIncomplete, problemID,
			)
		}
		entry := gradingFinalEntry{question: question}
		if hasAssessment {
			if assessment.CurrentDisposition != k12.GradingAssessmentDispositionCurrent ||
				assessment.ConfirmedVersion != question.ConfirmedVersion ||
				assessment.InputRevision != question.ConfirmedVersion ||
				strings.TrimSpace(assessment.ResultDigest) == "" {
				return k12.GradingFinalArtifact{}, fmt.Errorf(
					"%w: assessment for problem %s is not current for frozen revision",
					ErrGradingFinalizationIncomplete, problemID,
				)
			}
			entry.assessment = &assessment
			orderedDigests = append(orderedDigests, assessment.ResultDigest)
			publishedCount++
			structureVersion, err = mergeFinalStructureVersion(
				structureVersion, assessment.StructureVersion,
			)
		} else {
			if skip.CurrentDisposition != k12.GradingAssessmentDispositionCurrent ||
				skip.InputRevision != question.ConfirmedVersion ||
				strings.TrimSpace(skip.ResultDigest) == "" {
				return k12.GradingFinalArtifact{}, fmt.Errorf(
					"%w: skip for problem %s is not current for frozen revision",
					ErrGradingFinalizationIncomplete, problemID,
				)
			}
			entry.skip = &skip
			orderedDigests = append(orderedDigests, skip.ResultDigest)
			skippedCount++
			structureVersion, err = mergeFinalStructureVersion(
				structureVersion, skip.StructureVersion,
			)
		}
		if err != nil {
			return k12.GradingFinalArtifact{}, err
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 ||
		len(assessmentByProblem)+len(skipByProblem) != len(entries) {
		return k12.GradingFinalArtifact{}, fmt.Errorf(
			"%w: current receipts do not match frozen problem exact-set",
			ErrGradingFinalizationIncomplete,
		)
	}
	if structureVersion == 0 {
		structureVersion = k12.GradingFinalArtifactStructureVersion
	}
	orderedJSON, err := json.Marshal(orderedDigests)
	if err != nil {
		return k12.GradingFinalArtifact{}, err
	}

	coverage := k12.GradingFinalArtifactCoverageComplete
	if skippedCount > 0 {
		coverage = k12.GradingFinalArtifactCoverageWithSkips
	} else if strings.TrimSpace(job.Fields.SourceKind) == "webhook" &&
		!gradingFinalEntriesHaveTrustedConceptFacts(entries) {
		coverage = k12.GradingFinalArtifactCoverageGeneralGuidance
	}
	canonicalMarkdown := renderCanonicalGradingFinal(entries, nil)
	if coverage == k12.GradingFinalArtifactCoverageGeneralGuidance {
		canonicalMarkdown += "\n\n## 说明\n\n" +
			"本次没有可核验的课本依据，以上批改与家长讲法为通用参考。"
	}
	artifact := k12.GradingFinalArtifact{
		AgentName:                 job.Record.AgentName,
		JobID:                     job.Record.RecordID,
		StructureVersion:          structureVersion,
		CoverageStatus:            coverage,
		TotalCount:                len(entries),
		PublishedCount:            publishedCount,
		SkippedCount:              skippedCount,
		OrderedCurrentDigestsJSON: string(orderedJSON),
		CanonicalMarkdown:         canonicalMarkdown,
		SummaryInvocationID:       "",
	}
	if err := o.freezeGradingFinalAnnotatedAsset(ctx, run, job, &artifact); err != nil {
		return k12.GradingFinalArtifact{}, err
	}
	artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
	stored, _, err := o.deps.Records.CommitGradingFinalArtifact(
		ctx,
		artifact,
		finalizationGeneration,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, err
	}
	return stored, nil
}

// freezeGradingFinalAnnotatedAsset 在提交 final artifact 前把进程内批注图
// 提升为 owner-scoped PageAsset，并冻结原图与批注图的内容身份。
func (o *GradingOrchestrator) freezeGradingFinalAnnotatedAsset(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
	artifact *k12.GradingFinalArtifact,
) error {
	if run == nil || run.result == nil || run.result.AnnotatedImage == nil {
		return nil
	}
	// 只有 ImageTask 在创建时冻结了不可伪造的 owner 与原图 PageAsset
	// 关系；旧 direct-photo 调用没有该边界，继续保持既有无资产 final artifact。
	if strings.TrimSpace(job.Fields.SourceKind) != "image_task" {
		return nil
	}
	annotated := run.result.AnnotatedImage
	if len(annotated.Data) == 0 {
		return fmt.Errorf("%w: annotated image bytes are empty", ErrGradingFinalizationIncomplete)
	}
	dispatchID, err := gradingFinalImageTaskDispatchID(job)
	if err != nil {
		return err
	}
	dispatch, err := o.deps.Records.GetImageTaskDispatch(
		ctx, job.Record.AgentName, dispatchID,
	)
	if err != nil {
		return err
	}
	if len(dispatch.SourceAssetRefs) == 0 {
		return fmt.Errorf("%w: image task has no original source asset", ErrGradingFinalizationIncomplete)
	}
	ownerScope, err := o.deps.Records.GetImageTaskOwnerScope(
		ctx, job.Record.AgentName, dispatch.DispatchID,
	)
	if err != nil {
		return err
	}
	repository := &PageAssetRepository{Records: o.deps.Records}
	original, err := repository.OpenReady(
		ctx, ownerScope, job.Record.AgentName, dispatch.SourceAssetRefs[0],
	)
	if err != nil {
		return err
	}
	ready, err := repository.Persist(
		ctx, ownerScope, job.Record.AgentName, annotated.Data,
	)
	if err != nil {
		return err
	}
	if mime := strings.TrimSpace(annotated.MIME); mime != "" && mime != ready.Metadata.MediaType {
		return fmt.Errorf("%w: annotated image MIME does not match persisted bytes", ErrGradingFinalizationIncomplete)
	}
	artifact.AnnotatedAssetOwnerScope = ownerScope
	artifact.AnnotatedAssetID = ready.Metadata.PageAssetID
	artifact.AnnotatedMIME = ready.Metadata.MediaType
	artifact.AnnotatedDigest = ready.Metadata.ContentDigest
	artifact.OriginalSourceDigest = original.Metadata.ContentDigest
	return nil
}

func gradingFinalImageTaskDispatchID(job GradingJobView) (string, error) {
	if job.Record == nil || strings.TrimSpace(job.Record.AgentName) == "" ||
		strings.TrimSpace(job.Fields.SourceKind) != "image_task" ||
		job.Fields.ConfirmationState != k12.GradingConfirmationConfirmed ||
		job.Fields.ConfirmedVersion < 0 {
		return "", fmt.Errorf("%w: annotated image job source is invalid", ErrGradingFinalizationIncomplete)
	}
	prefix := "image_task|"
	suffix := fmt.Sprintf("|v%d", job.Fields.ConfirmedVersion)
	key := strings.TrimSpace(job.Fields.IdempotencyKey)
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
		return "", fmt.Errorf("%w: annotated image source identity is invalid", ErrGradingFinalizationIncomplete)
	}
	dispatchID := strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)
	if strings.TrimSpace(dispatchID) == "" {
		return "", fmt.Errorf("%w: annotated image dispatch identity is empty", ErrGradingFinalizationIncomplete)
	}
	return dispatchID, nil
}

// text webhook 不得把模型评估推断成已确认教材知识点；只有冻结 Problem
// 自身携带的非空 concept facts 才允许进入完整 TutoringTips 摘要链。
func gradingFinalEntriesHaveTrustedConceptFacts(entries []gradingFinalEntry) bool {
	for _, entry := range entries {
		for _, concept := range entry.question.KnowledgePoints {
			if strings.TrimSpace(concept) != "" {
				return true
			}
		}
	}
	return false
}

func (o *GradingOrchestrator) requireTerminalGradingOperations(
	ctx context.Context,
	agentName, jobID string,
	assessments []k12.GradingAssessmentItem,
) error {
	invocations, err := o.deps.Records.ListGradingItemInvocations(ctx, agentName, jobID)
	if err != nil {
		return err
	}
	invocationByID := make(map[string]k12.GradingItemInvocation, len(invocations))
	for _, invocation := range invocations {
		invocationByID[invocation.InvocationID] = invocation
	}
	assessmentByProblem := make(map[string]k12.GradingAssessmentItem, len(assessments))
	for _, assessment := range assessments {
		assessmentByProblem[assessment.ProblemID] = assessment
	}
	type logicalOperation struct {
		problemID     string
		operation     k12.GradingItemOperation
		requestDigest string
	}
	latest := make(map[logicalOperation]k12.GradingItemInvocation, len(invocations))
	for _, invocation := range invocations {
		key := logicalOperation{
			problemID: invocation.ProblemID, operation: invocation.Operation,
			requestDigest: invocation.RequestDigest,
		}
		current, ok := latest[key]
		if !ok || invocation.OperationAttempt > current.OperationAttempt {
			latest[key] = invocation
		}
	}
	for _, invocation := range latest {
		switch invocation.Status {
		case k12.ModelInvocationSucceeded, k12.ModelInvocationReconciled:
		case k12.ModelInvocationFailed, k12.ModelInvocationOutcomeUnknown:
			if gradingOperationSupersededByCurrentAssessment(
				invocation, assessmentByProblem[invocation.ProblemID], invocationByID,
			) {
				continue
			}
			fallthrough
		default:
			return fmt.Errorf(
				"%w: problem %s operation %s remains %s",
				ErrGradingFinalizationIncomplete,
				invocation.ProblemID,
				invocation.Operation,
				invocation.Status,
			)
		}
	}
	return nil
}

func gradingOperationSupersededByCurrentAssessment(
	invocation k12.GradingItemInvocation,
	assessment k12.GradingAssessmentItem,
	invocationByID map[string]k12.GradingItemInvocation,
) bool {
	if assessment.CurrentDisposition != k12.GradingAssessmentDispositionCurrent ||
		assessment.AgentName != invocation.AgentName ||
		assessment.JobID != invocation.JobID ||
		assessment.ProblemID != invocation.ProblemID ||
		assessment.AttemptID != invocation.AttemptID {
		return false
	}
	referenceID := ""
	switch invocation.Operation {
	case k12.GradingItemOperationSolve,
		k12.GradingItemOperationSolveGenerate,
		k12.GradingItemOperationSolveVerify:
		referenceID = assessment.SolveInvocationID
	case k12.GradingItemOperationGrade:
		referenceID = assessment.GradeInvocationID
	case k12.GradingItemOperationParentGuide:
		referenceID = assessment.ParentGuideInvocationID
	}
	reference, ok := invocationByID[referenceID]
	if !ok ||
		reference.InvocationID == invocation.InvocationID ||
		reference.AgentName != invocation.AgentName {
		return false
	}
	if reference.JobID != invocation.JobID ||
		reference.ProblemID != invocation.ProblemID ||
		reference.AttemptID != invocation.AttemptID ||
		reference.Status != k12.ModelInvocationSucceeded {
		return false
	}
	if reference.Operation == invocation.Operation {
		return reference.OperationAttempt > invocation.OperationAttempt
	}
	if invocation.Operation != k12.GradingItemOperationSolve &&
		invocation.Operation != k12.GradingItemOperationSolveGenerate &&
		invocation.Operation != k12.GradingItemOperationSolveVerify {
		return false
	}
	if reference.Operation != k12.GradingItemOperationSolve &&
		reference.Operation != k12.GradingItemOperationSolveGenerate &&
		reference.Operation != k12.GradingItemOperationSolveVerify {
		return false
	}
	return reference.CreatedAt >= invocation.UpdatedAt &&
		assessment.UpdatedAt >= invocation.UpdatedAt
}

func mergeFinalStructureVersion(current, candidate int) (int, error) {
	if candidate < 1 {
		return 0, fmt.Errorf(
			"%w: receipt has no structure version",
			ErrGradingFinalizationIncomplete,
		)
	}
	if current != 0 && current != candidate {
		return 0, fmt.Errorf(
			"%w: current receipts span structure versions",
			ErrGradingFinalizationIncomplete,
		)
	}
	return candidate, nil
}

func (o *GradingOrchestrator) buildFinalTutoringTips(
	ctx context.Context,
	job GradingJobView,
	structureVersion int,
	orderedDigestsJSON []byte,
) (TutoringTips, string, error) {
	attempt := job.Fields.AttemptCount + 1
	if attempt < 1 {
		attempt = 1
	}
	requestDigest := modelInvocationDigest(
		[]byte(fmt.Sprintf("structure:%d", structureVersion)),
		orderedDigestsJSON,
	)
	if problemSourceReconciliationOnly(ctx) {
		if o == nil || o.deps.Records == nil {
			return TutoringTips{}, "", fmt.Errorf(
				"%w: reconciliation-only processing has no durable page summary invocation",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		invocation, err := o.deps.Records.GetModelInvocationByAttempt(
			ctx,
			job.Record.AgentName,
			job.Record.RecordID,
			k12.GradingStageProjecting,
			attempt,
		)
		if err != nil {
			return TutoringTips{}, "", fmt.Errorf(
				"%w: reconciliation-only page summary lookup failed: %v",
				ErrModelInvocationRequiresReconciliation,
				err,
			)
		}
		if invocation.RequestDigest != requestDigest ||
			invocation.RouteSnapshot != job.Fields.ModelSnapshot ||
			!invocation.RequestPolicySnapshot.IsZero() ||
			invocation.Status != k12.ModelInvocationSucceeded {
			return TutoringTips{}, "", fmt.Errorf(
				"%w: reconciliation-only page summary invocation %s is not the exact durable success",
				ErrModelInvocationRequiresReconciliation,
				invocation.InvocationID,
			)
		}
		tips, recoverErr := recoverFinalTutoringTips(job, invocation)
		if recoverErr != nil {
			return TutoringTips{}, "", recoverErr
		}
		return tips, invocation.InvocationID, nil
	}
	invocation, _, err := o.deps.Records.PrepareModelInvocation(
		ctx,
		k12.ModelInvocation{
			InvocationID:  "modelinv-" + idgen.ShortID(),
			AgentName:     job.Record.AgentName,
			JobID:         job.Record.RecordID,
			Stage:         k12.GradingStageProjecting,
			RequestDigest: requestDigest,
			RouteSnapshot: job.Fields.ModelSnapshot,
			Attempt:       attempt,
			CreatedAt:     o.deps.now(),
			UpdatedAt:     o.deps.now(),
		},
	)
	if err != nil {
		return TutoringTips{}, "", err
	}
	switch invocation.Status {
	case k12.ModelInvocationSucceeded:
		tips, recoverErr := recoverFinalTutoringTips(job, invocation)
		if recoverErr != nil {
			return TutoringTips{}, "", recoverErr
		}
		return tips, invocation.InvocationID, nil
	case k12.ModelInvocationPrepared:
		if invocation.ResultDigest != "" || invocation.ResultJSON != "" {
			return TutoringTips{}, "", fmt.Errorf(
				"%w: prepared page summary invocation %s carries result state",
				ErrModelInvocationRequiresReconciliation,
				invocation.InvocationID,
			)
		}
	default:
		return TutoringTips{}, "", fmt.Errorf(
			"%w: page summary invocation %s is %s without a final artifact",
			ErrModelInvocationRequiresReconciliation,
			invocation.InvocationID,
			invocation.Status,
		)
	}
	invocation, err = o.deps.Records.MarkModelInvocationSent(
		ctx, invocation.AgentName, invocation.InvocationID, "",
	)
	if err != nil {
		return TutoringTips{}, "", err
	}
	frozenGrounding, hasFrozenGrounding, err := gradingGroundingSnapshotFromItemInvocations(
		ctx, o.deps, job.Record.AgentName, job.Record.RecordID,
	)
	var tips TutoringTips
	if err == nil {
		if hasFrozenGrounding {
			tips, err = o.deps.buildTutoringTipsSubject(
				ctx, job.Record.AgentName, job.Record.RecordID, &frozenGrounding,
			)
		} else {
			tips, err = o.deps.BuildTutoringTips(
				ctx, job.Record.AgentName, job.Record.RecordID,
			)
		}
	}
	if err != nil {
		if invocationOutcomeUnknown(err) {
			_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(
				context.WithoutCancel(ctx), invocation.AgentName,
				invocation.InvocationID, "page_summary_transport_unknown",
			)
		} else {
			_, _ = o.deps.Records.MarkModelInvocationFailed(
				context.WithoutCancel(ctx), invocation.AgentName,
				invocation.InvocationID, "page_summary_failed",
			)
		}
		return TutoringTips{}, "", err
	}
	resultJSON, err := json.Marshal(tips)
	if err != nil {
		return TutoringTips{}, "", fmt.Errorf(
			"usecase: encode successful page summary result: %w",
			err,
		)
	}
	if _, err := o.deps.Records.MarkModelInvocationSucceededWithResult(
		context.WithoutCancel(ctx),
		invocation.AgentName,
		invocation.InvocationID,
		modelInvocationDigest(resultJSON),
		string(resultJSON),
		"",
	); err != nil {
		return TutoringTips{}, "", err
	}
	return tips, invocation.InvocationID, nil
}

func recoverFinalTutoringTips(
	job GradingJobView,
	invocation k12.ModelInvocation,
) (TutoringTips, error) {
	fail := func(reason string) (TutoringTips, error) {
		return TutoringTips{}, fmt.Errorf(
			"%w: page summary invocation %s %s",
			ErrModelInvocationRequiresReconciliation,
			invocation.InvocationID,
			reason,
		)
	}
	if strings.TrimSpace(invocation.ResultJSON) == "" ||
		!json.Valid([]byte(invocation.ResultJSON)) {
		return fail("has no valid durable result payload")
	}
	if strings.TrimSpace(invocation.ResultDigest) == "" ||
		invocation.ResultDigest != modelInvocationDigest([]byte(invocation.ResultJSON)) {
		return fail("result digest does not match its durable payload")
	}

	var tips TutoringTips
	decoder := json.NewDecoder(bytes.NewReader([]byte(invocation.ResultJSON)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tips); err != nil {
		return fail("durable result payload does not match the tutoring-tips contract")
	}
	if tips.GroundingEvidenceReceipts == nil {
		tips.GroundingEvidenceReceipts = []GroundingEvidenceReceipt{}
	}
	if err := validateRecoveredFinalTutoringTips(job, tips); err != nil {
		return fail(err.Error())
	}
	return tips, nil
}

func validateRecoveredFinalTutoringTips(
	job GradingJobView,
	tips TutoringTips,
) error {
	if tips.GradingJobID != job.Record.RecordID ||
		tips.SubmissionID != job.Fields.SubmissionID {
		return fmt.Errorf("durable result identity does not match the grading job")
	}
	if strings.TrimSpace(tips.Grade) == "" ||
		strings.TrimSpace(tips.Subject) == "" ||
		len(tips.KnowledgePoints) == 0 {
		return fmt.Errorf("durable result omits required tutoring facts")
	}
	for _, knowledgePoint := range tips.KnowledgePoints {
		if strings.TrimSpace(knowledgePoint) == "" {
			return fmt.Errorf("durable result contains an empty knowledge point")
		}
	}
	if len(tips.Sections) != 3 {
		return fmt.Errorf("durable result must contain exactly three sections")
	}
	for _, section := range tips.Sections {
		if strings.TrimSpace(section.Title) == "" ||
			strings.TrimSpace(section.Content) == "" ||
			strings.TrimSpace(section.SourceLabel) == "" {
			return fmt.Errorf("durable result contains an incomplete section")
		}
	}
	if strings.TrimSpace(tips.Sections[0].Title) != "这页在练什么" ||
		(tips.Sections[0].SourceLabel != TutoringTipsSourceTextbook &&
			tips.Sections[0].SourceLabel != TutoringTipsSourceAI) {
		return fmt.Errorf("durable result overview section contract changed")
	}
	seenGroundingReceipts := make(map[GroundingEvidenceReceipt]struct{}, len(tips.GroundingEvidenceReceipts))
	for _, receipt := range tips.GroundingEvidenceReceipts {
		if err := validateGroundingEvidenceReceiptIdentity(receipt); err != nil {
			return err
		}
		if _, duplicate := seenGroundingReceipts[receipt]; duplicate {
			return fmt.Errorf("durable result contains a duplicate grounding receipt")
		}
		seenGroundingReceipts[receipt] = struct{}{}
	}
	if len(tips.GroundingEvidenceReceipts) == 0 &&
		tips.Sections[0].SourceLabel == TutoringTipsSourceTextbook {
		return fmt.Errorf("durable result claims textbook grounding without a receipt")
	}
	attentionTitle := strings.TrimSpace(tips.Sections[1].Title)
	if attentionTitle == "要留意" || !strings.HasSuffix(attentionTitle, "要留意") ||
		tips.Sections[1].SourceLabel != TutoringTipsSourceLearningEvidence {
		return fmt.Errorf("durable result learning-evidence section contract changed")
	}
	if strings.TrimSpace(tips.Sections[2].Title) != "每道题怎么带（不直接给答案）" ||
		tips.Sections[2].SourceLabel != TutoringTipsSourceAI {
		return fmt.Errorf("durable result per-problem section contract changed")
	}
	return nil
}

func renderCanonicalGradingFinal(
	entries []gradingFinalEntry,
	_ *TutoringTips,
) string {
	var out strings.Builder
	out.WriteString("# 作业批改结果\n\n")
	correctCount, processIssueCount, attentionCount, skippedCount := 0, 0, 0, 0
	for _, entry := range entries {
		if entry.skip != nil {
			skippedCount++
			continue
		}
		if entry.assessment == nil {
			continue
		}
		switch entry.assessment.Status {
		case k12.GradingAssessmentCorrect:
			correctCount++
		case k12.GradingAssessmentProcessIssue:
			processIssueCount++
		default:
			attentionCount++
		}
	}
	out.WriteString("## 批改摘要\n\n")
	fmt.Fprintf(&out, "**共 %d 题 · %d 题正确", len(entries), correctCount)
	if processIssueCount > 0 {
		fmt.Fprintf(&out, " · %d 题过程需关注", processIssueCount)
	}
	if attentionCount > 0 {
		fmt.Fprintf(&out, " · %d 题需关注", attentionCount)
	}
	if skippedCount > 0 {
		fmt.Fprintf(&out, " · %d 题未判断", skippedCount)
	}
	out.WriteString("**\n\n")
	if processIssueCount > 0 {
		out.WriteString("> 过程问题表示最终答案正确，但书写过程需要核对，不记为错题。\n\n")
	}

	detailCount := processIssueCount + attentionCount + skippedCount
	if detailCount > 0 {
		out.WriteString("## 需关注的题\n\n")
	}
	for _, entry := range entries {
		if entry.skip == nil && (entry.assessment == nil || entry.assessment.Status == k12.GradingAssessmentCorrect) {
			continue
		}
		label := RecognizedQuestionSourceDisplayLabel(entry.question)
		if label == "" {
			label = "题目位置待确认"
		}
		out.WriteString("### ")
		out.WriteString(label)
		out.WriteString("\n\n")
		if question := strings.TrimSpace(entry.question.CanonicalMarkdown); question != "" &&
			(entry.assessment == nil || entry.assessment.Status != k12.GradingAssessmentProcessIssue) {
			out.WriteString(question)
			out.WriteString("\n\n")
		}
		if entry.skip != nil {
			out.WriteString("**已跳过 · 未判断对错**\n\n")
			continue
		}
		out.WriteString("**批改状态：** ")
		out.WriteString(gradingAssessmentParentStatus(entry.assessment.Status))
		out.WriteString("\n\n")
		if entry.assessment.Status == k12.GradingAssessmentProcessIssue {
			writeCanonicalProcessIssueDetails(&out, entry.assessment.ResultJSON)
		} else if details, status, ok := RenderCanonicalGradingAssessmentDetails(
			entry.assessment.ResultJSON,
		); ok && string(status) == string(entry.assessment.Status) && details != "" {
			out.WriteString(details)
			out.WriteString("\n\n")
		}
	}
	if correctCount > 0 {
		out.WriteString("## 已答对的题\n\n")
		if detailCount > 0 {
			fmt.Fprintf(&out, "其余 %d 题已答对。\n\n", correctCount)
		} else {
			fmt.Fprintf(&out, "%d 题已答对。\n\n", correctCount)
		}
	}
	return strings.TrimSpace(out.String())
}

func gradingAssessmentParentStatus(status k12.GradingAssessmentStatus) string {
	switch status {
	case k12.GradingAssessmentCorrect:
		return "✅ 正确"
	case k12.GradingAssessmentProcessIssue:
		return "⚠ 过程问题（最终答案正确，不记为错题）"
	case k12.GradingAssessmentWrong:
		return "❌ 需要订正"
	case k12.GradingAssessmentUnanswered:
		return "⏸ 未作答"
	case k12.GradingAssessmentAnswerUnclear:
		return "⚠ 作答待补录"
	case k12.GradingAssessmentBlankSolved:
		return "📘 已生成家长辅导指南"
	case k12.GradingAssessmentOutOfScope:
		return "⛔ 超出当前年级范围"
	default:
		return "⚠ 待核对"
	}
}

func writeCanonicalProcessIssueDetails(out *strings.Builder, resultJSON string) {
	var item PhotoGradeItem
	if json.Unmarshal([]byte(resultJSON), &item) != nil || item.Status != PhotoCorrectWithProcessIssue {
		return
	}
	writeCanonicalGradingVisibleAnswers(out, item)
	if wrongStep := strings.TrimSpace(item.Grade.Outcome.WrongStep); wrongStep != "" {
		out.WriteString("**错误步骤：** ")
		out.WriteString(photoInline(wrongStep, 300))
		out.WriteString("\n\n")
	}
	if cause := strings.TrimSpace(item.Grade.Outcome.ErrorCause); cause != "" {
		out.WriteString("**原因：** ")
		out.WriteString(photoInline(cause, 300))
		out.WriteString("\n\n")
	}
	if item.ParentGuide != nil {
		out.WriteString("### 家长怎么讲\n\n")
		writeParentTeachingGuideDetailsMarkdown(out, *item.ParentGuide)
		out.WriteString("\n\n")
	}
}

func writeCanonicalGradingVisibleAnswers(out *strings.Builder, item PhotoGradeItem) {
	studentAnswer := strings.TrimSpace(item.Recognized.AnswerCanonicalMarkdown)
	if studentAnswer == "" {
		studentAnswer = strings.TrimSpace(item.Recognized.StudentAnswer)
	}
	if studentAnswer == "" {
		studentAnswer = strings.TrimSpace(item.Recognized.AnswerRawTranscription)
	}
	if studentAnswer != "" {
		out.WriteString("**原始作答：** ")
		out.WriteString(photoInline(studentAnswer, 600))
		out.WriteString("\n\n")
	}
	if item.ParentGuide != nil && strings.TrimSpace(item.ParentGuide.Answer) != "" {
		out.WriteString("**正确答案：** ")
		out.WriteString(photoInline(item.ParentGuide.Answer, 600))
		out.WriteString("\n\n")
	}
}

// RenderCanonicalGradingAssessmentDetails 将最终产物中的受控评估对象投影为家长可读
// Markdown。返回 ok=false 时调用方必须把原文视为普通用户内容，不得按内部证据删除。
func RenderCanonicalGradingAssessmentDetails(resultJSON string) (string, PhotoItemStatus, bool) {
	expectedKeys := map[string]struct{}{
		"Grade": {}, "ParentGuide": {}, "Recognized": {}, "ResultKind": {},
		"Solve": {}, "Status": {}, "Warning": {},
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(resultJSON), &object) != nil || len(object) != len(expectedKeys) {
		return "", "", false
	}
	for key := range object {
		if _, ok := expectedKeys[key]; !ok {
			return "", "", false
		}
	}

	var item PhotoGradeItem
	decoder := json.NewDecoder(strings.NewReader(resultJSON))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&item) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return "", "", false
	}
	if item.ResultKind != "" || item.Grade.RecordID != "" || item.Grade.RecordCreated ||
		strings.TrimSpace(item.Recognized.ProblemID) == "" ||
		strings.TrimSpace(item.Recognized.AttemptID) == "" ||
		strings.TrimSpace(item.Recognized.InputDigest) == "" ||
		item.Recognized.ConfirmedVersion < 1 {
		return "", "", false
	}

	requiresGuide := false
	switch item.Status {
	case PhotoCorrect, PhotoUnanswered, PhotoAnswerUnclear, PhotoOutOfScope, PhotoUntrusted:
	case PhotoCorrectWithProcessIssue:
		requiresGuide = true
		if strings.TrimSpace(item.Grade.Outcome.WrongStep) == "" ||
			strings.TrimSpace(item.Grade.Outcome.ErrorCause) == "" {
			return "", "", false
		}
	case PhotoWrong:
		requiresGuide = true
		if strings.TrimSpace(item.Grade.Solution) == "" {
			return "", "", false
		}
	case PhotoBlankSolved:
		requiresGuide = true
	default:
		return "", "", false
	}
	if requiresGuide {
		if item.ParentGuide == nil || validateParentTeachingGuide(*item.ParentGuide) != nil {
			return "", "", false
		}
	} else if item.ParentGuide != nil {
		return "", "", false
	}

	var out strings.Builder
	writeCanonicalGradingVisibleAnswers(&out, item)
	switch item.Status {
	case PhotoWrong:
		if wrongStep := strings.TrimSpace(item.Grade.Outcome.WrongStep); wrongStep != "" {
			out.WriteString("**第一个错步：** ")
			out.WriteString(photoInline(wrongStep, 300))
			out.WriteString("\n\n")
		}
		if cause := strings.TrimSpace(item.Grade.Outcome.ErrorCause); cause != "" {
			out.WriteString("**错因：** ")
			out.WriteString(photoInline(cause, 300))
			out.WriteString("\n\n")
		}
		out.WriteString("### 家长怎么讲\n\n")
		writeParentTeachingGuideDetailsMarkdown(&out, *item.ParentGuide)
	case PhotoBlankSolved:
		out.WriteString("### 家长辅导指南\n\n")
		writeParentTeachingGuideDetailsMarkdown(&out, *item.ParentGuide)
	}
	return strings.TrimSpace(out.String()), item.Status, true
}
