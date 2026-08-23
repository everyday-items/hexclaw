package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	summaryInvocationID := ""
	var tips *TutoringTips
	if skippedCount > 0 {
		coverage = k12.GradingFinalArtifactCoverageWithSkips
	} else if strings.TrimSpace(job.Fields.SourceKind) == "webhook" &&
		!gradingFinalEntriesHaveTrustedConceptFacts(entries) {
		coverage = k12.GradingFinalArtifactCoverageGeneralGuidance
	} else {
		generated, invocationID, summaryErr := o.buildFinalTutoringTips(
			ctx, job, structureVersion, orderedJSON,
		)
		if summaryErr != nil {
			return k12.GradingFinalArtifact{}, summaryErr
		}
		tips = &generated
		summaryInvocationID = invocationID
	}
	canonicalMarkdown := renderCanonicalGradingFinal(entries, tips)
	if coverage == k12.GradingFinalArtifactCoverageGeneralGuidance {
		canonicalMarkdown += "\n\n# General guidance\n\n" +
			"No verified textbook grounding is available. " +
			"The item-level assessment and parent guidance above are general guidance and are not based on a textbook."
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
		SummaryInvocationID:       summaryInvocationID,
	}
	artifact.ArtifactDigest = gradingFinalArtifactDigest(artifact)
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
	return ok &&
		reference.InvocationID != invocation.InvocationID &&
		reference.AgentName == invocation.AgentName &&
		reference.JobID == invocation.JobID &&
		reference.ProblemID == invocation.ProblemID &&
		reference.AttemptID == invocation.AttemptID &&
		reference.Operation == invocation.Operation &&
		reference.OperationAttempt > invocation.OperationAttempt &&
		reference.Status == k12.ModelInvocationSucceeded
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
	tips, err := o.deps.BuildTutoringTips(
		ctx, job.Record.AgentName, job.Record.RecordID,
	)
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
	tips *TutoringTips,
) string {
	var out strings.Builder
	out.WriteString("# 作业批改结果\n\n")
	correctCount, processIssueCount, wrongCount := 0, 0, 0
	for _, entry := range entries {
		if entry.assessment == nil {
			continue
		}
		switch entry.assessment.Status {
		case k12.GradingAssessmentCorrect:
			correctCount++
		case k12.GradingAssessmentProcessIssue:
			processIssueCount++
		case k12.GradingAssessmentWrong:
			wrongCount++
		}
	}
	if processIssueCount > 0 {
		out.WriteString("## Grading summary\n\n")
		fmt.Fprintf(
			&out,
			"This run determined **%d** questions: **%d correct / %d with process issues / %d requiring correction**.\n\n"+
				"> A process issue has a correct final answer and is not recorded as wrong.\n\n",
			correctCount+processIssueCount+wrongCount,
			correctCount,
			processIssueCount,
			wrongCount,
		)
	}
	for _, entry := range entries {
		label := RecognizedQuestionSourceDisplayLabel(entry.question)
		if label == "" {
			label = strings.TrimSpace(entry.question.ProblemID)
		}
		out.WriteString("## ")
		out.WriteString(label)
		out.WriteString("\n\n")
		if question := strings.TrimSpace(entry.question.CanonicalMarkdown); question != "" {
			out.WriteString(question)
			out.WriteString("\n\n")
		}
		if entry.skip != nil {
			out.WriteString("**已跳过 · 未判断对错**\n\n")
			continue
		}
		if entry.assessment.Status == k12.GradingAssessmentProcessIssue {
			out.WriteString("**Grading status:** ⚠ Process issue (final answer correct; not recorded as wrong) · `correct_with_process_issue`\n\n")
			writeCanonicalProcessIssueDetails(&out, entry.assessment.ResultJSON)
		} else {
			out.WriteString("**Grading status:** `")
			out.WriteString(string(entry.assessment.Status))
			out.WriteString("`\n\n")
		}
		var indented bytes.Buffer
		if json.Indent(&indented, []byte(entry.assessment.ResultJSON), "", "  ") == nil {
			out.WriteString("```json\n")
			out.Write(indented.Bytes())
			out.WriteString("\n```\n\n")
		}
	}
	if tips != nil {
		out.WriteString("# 这份作业的辅导要点\n\n")
		for _, section := range tips.Sections {
			out.WriteString("## ")
			out.WriteString(strings.TrimSpace(section.Title))
			out.WriteString("\n\n")
			out.WriteString(strings.TrimSpace(section.Content))
			out.WriteString("\n\n")
			if source := strings.TrimSpace(section.SourceLabel); source != "" {
				out.WriteString("_")
				out.WriteString(source)
				out.WriteString("_\n\n")
			}
		}
	}
	return strings.TrimSpace(out.String())
}

func writeCanonicalProcessIssueDetails(out *strings.Builder, resultJSON string) {
	var item PhotoGradeItem
	if json.Unmarshal([]byte(resultJSON), &item) != nil || item.Status != PhotoCorrectWithProcessIssue {
		return
	}
	if wrongStep := strings.TrimSpace(item.Grade.Outcome.WrongStep); wrongStep != "" {
		out.WriteString("**Process note:** ")
		out.WriteString(photoInline(wrongStep, 300))
		out.WriteString("\n\n")
	}
	if cause := strings.TrimSpace(item.Grade.Outcome.ErrorCause); cause != "" {
		out.WriteString("**Cause:** ")
		out.WriteString(photoInline(cause, 300))
		out.WriteString("\n\n")
	}
	if item.ParentGuide != nil {
		out.WriteString("### How the parent can explain it\n\n")
		writeParentTeachingGuideMarkdown(out, *item.ParentGuide)
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
	switch item.Status {
	case PhotoWrong:
		out.WriteString("### 订正参考\n\n")
		out.WriteString(photoMarkdownQuote(item.Grade.Solution, 1000))
		if wrongStep := strings.TrimSpace(item.Grade.Outcome.WrongStep); wrongStep != "" {
			out.WriteString("\n\n**第一个错步：** ")
			out.WriteString(photoInline(wrongStep, 300))
		}
		if cause := strings.TrimSpace(item.Grade.Outcome.ErrorCause); cause != "" {
			out.WriteString("\n\n**错因：** ")
			out.WriteString(photoInline(cause, 300))
		}
		out.WriteString("\n\n### 家长怎么讲\n\n")
		writeParentTeachingGuideMarkdown(&out, *item.ParentGuide)
	case PhotoBlankSolved:
		out.WriteString("### 家长辅导指南\n\n")
		writeParentTeachingGuideMarkdown(&out, *item.ParentGuide)
	}
	return strings.TrimSpace(out.String()), item.Status, true
}

func gradingFinalArtifactDigest(artifact k12.GradingFinalArtifact) string {
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
