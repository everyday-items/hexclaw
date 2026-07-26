package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := o.requireTerminalGradingOperations(
		ctx, job.Record.AgentName, job.Record.RecordID,
	); err != nil {
		return k12.GradingFinalArtifact{}, err
	}
	assessments, err := o.deps.Records.ListGradingAssessmentItems(
		ctx, job.Record.AgentName, job.Record.RecordID,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, err
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
	stored, _, err := o.deps.Records.CommitGradingFinalArtifact(ctx, artifact)
	if err != nil {
		return k12.GradingFinalArtifact{}, err
	}
	return stored, nil
}

func (o *GradingOrchestrator) requireTerminalGradingOperations(
	ctx context.Context,
	agentName, jobID string,
) error {
	invocations, err := o.deps.Records.ListGradingItemInvocations(ctx, agentName, jobID)
	if err != nil {
		return err
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
	invocation, _, err := o.deps.Records.PrepareModelInvocation(
		ctx,
		k12.ModelInvocation{
			InvocationID: "modelinv-" + idgen.ShortID(),
			AgentName:    job.Record.AgentName,
			JobID:        job.Record.RecordID,
			Stage:        k12.GradingStageProjecting,
			RequestDigest: modelInvocationDigest(
				[]byte(fmt.Sprintf("structure:%d", structureVersion)),
				orderedDigestsJSON,
			),
			RouteSnapshot: job.Fields.ModelSnapshot,
			Attempt:       attempt,
			CreatedAt:     o.deps.now(),
			UpdatedAt:     o.deps.now(),
		},
	)
	if err != nil {
		return TutoringTips{}, "", err
	}
	if invocation.Status != k12.ModelInvocationPrepared {
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
	if _, err := o.deps.Records.MarkModelInvocationSucceeded(
		context.WithoutCancel(ctx),
		invocation.AgentName,
		invocation.InvocationID,
		modelInvocationResultDigest(tips),
		"",
	); err != nil {
		return TutoringTips{}, "", err
	}
	return tips, invocation.InvocationID, nil
}

func renderCanonicalGradingFinal(
	entries []gradingFinalEntry,
	tips *TutoringTips,
) string {
	var out strings.Builder
	out.WriteString("# 作业批改结果\n\n")
	for _, entry := range entries {
		label := strings.TrimSpace(entry.question.DisplayLabel)
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
		out.WriteString("**批改状态：** `")
		out.WriteString(string(entry.assessment.Status))
		out.WriteString("`\n\n")
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
