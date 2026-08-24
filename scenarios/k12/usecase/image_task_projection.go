package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ImageTaskHomeworkProjection is an owner-scoped, read-only projection. It
// exposes only stable UI facts and never leaks the internal GradingJob id,
// model snapshot, invocation ledger or failure internals.
func (o *GradingOrchestrator) ImageTaskHomeworkProjection(
	ctx context.Context,
	agentName, jobID string,
) (ImageTaskHomeworkProjection, error) {
	job, err := o.deps.GetGradingJob(ctx, agentName, jobID)
	if err != nil {
		return ImageTaskHomeworkProjection{}, err
	}
	questions, _ := o.RecognizedQuestionsForOwner(ctx, agentName, jobID)
	currentInputs, err := o.deps.Records.ListCurrentProblemInputRevisions(
		ctx,
		agentName,
		job.Fields.SubmissionID,
	)
	if err != nil {
		return ImageTaskHomeworkProjection{}, err
	}
	for index := range questions {
		current, ok := currentInputs[questions[index].ProblemID]
		if !ok {
			continue
		}
		questions[index].PageAssetID = current.PageAssetID
		questions[index].SourceWidth = current.SourceWidth
		questions[index].SourceHeight = current.SourceHeight
		questions[index].SourceRegion = current.SourceRegion
		questions[index].RawTranscription = current.StemRaw
		questions[index].AnswerRawTranscription = current.AnswerRaw
		questions[index].CanonicalMarkdown = current.QuestionCanonicalMarkdown
		questions[index].AnswerCanonicalMarkdown = current.AnswerCanonicalMarkdown
		questions[index].InputDigest = current.InputDigest
		questions[index].CanonicalVersion = current.InputRevision
	}
	subject := ""
	counts := map[string]int{}
	for _, question := range questions {
		value := strings.TrimSpace(question.Subject)
		if value == "" {
			continue
		}
		counts[value]++
		if subject == "" || counts[value] > counts[subject] {
			subject = value
		}
	}
	durableProjection, err := o.deps.Records.GetGradingProgressiveProjection(
		ctx, agentName, jobID,
	)
	if err != nil {
		return ImageTaskHomeworkProjection{}, err
	}
	groundingReceipts := []GroundingEvidenceReceipt{}
	if artifact := durableProjection.FinalArtifact; artifact != nil &&
		strings.TrimSpace(artifact.SummaryInvocationID) != "" {
		invocation, invocationErr := o.deps.Records.GetModelInvocation(
			ctx, agentName, artifact.SummaryInvocationID,
		)
		if invocationErr != nil {
			return ImageTaskHomeworkProjection{}, invocationErr
		}
		if invocation.JobID != job.Record.RecordID ||
			invocation.Stage != k12.GradingStageProjecting ||
			invocation.Status != k12.ModelInvocationSucceeded {
			return ImageTaskHomeworkProjection{}, fmt.Errorf(
				"usecase: final artifact grounding invocation is not the linked success",
			)
		}
		tips, recoveryErr := recoverFinalTutoringTips(job, invocation)
		if recoveryErr != nil {
			return ImageTaskHomeworkProjection{}, recoveryErr
		}
		groundingReceipts = cloneGroundingEvidenceReceipts(
			tips.GroundingEvidenceReceipts,
		)
	}
	problemGroundingReceipts, err := o.projectProblemGroundingReceipts(
		ctx,
		agentName,
		jobID,
		questions,
		job.Record.Status == k12.GradingStageCompleted,
		len(groundingReceipts) > 0,
	)
	if err != nil {
		return ImageTaskHomeworkProjection{}, err
	}
	return ImageTaskHomeworkProjection{
		Stage: job.Record.Status, Retryable: job.Fields.Retryable,
		ConfirmationState: job.Fields.ConfirmationState,
		AnchorState:       job.Fields.AnchorState, Subject: subject,
		retryFailureKind: job.Fields.FailureKind,
		Questions:        cloneRecognizedQuestions(questions),
		Progressive: imageTaskProgressiveSnapshotFromStorage(
			durableProjection.ProgressiveSnapshot,
		),
		FinalArtifact:             durableProjection.FinalArtifact,
		GroundingEvidenceReceipts: groundingReceipts,
		ProblemGroundingReceipts:  problemGroundingReceipts,
	}, nil
}

// CancelImageTaskHomework is the internal cancellation bridge used by the
// ImageTaskDispatch facade. It preserves owner checks and interrupts an
// in-flight provider run when one is registered; after restart it falls back
// to the durable GradingJob state machine instead of leaving the child active.
func (o *GradingOrchestrator) CancelImageTaskHomework(
	ctx context.Context,
	agentName, jobID string,
) error {
	if _, err := o.deps.GetGradingJob(ctx, agentName, jobID); err != nil {
		return err
	}
	if _, handled, err := o.CancelPhotoGradingJob(ctx, agentName, jobID); handled {
		return err
	}
	_, err := o.deps.CancelGradingJob(ctx, agentName, jobID)
	return err
}
