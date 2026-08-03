package usecase

import (
	"context"
	"strings"
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
	return ImageTaskHomeworkProjection{
		Stage: job.Record.Status, Retryable: job.Fields.Retryable,
		ConfirmationState: job.Fields.ConfirmationState,
		AnchorState:       job.Fields.AnchorState, Subject: subject,
		retryFailureKind: job.Fields.FailureKind,
		Questions:        cloneRecognizedQuestions(questions),
		Progressive: imageTaskProgressiveSnapshotFromStorage(
			durableProjection.ProgressiveSnapshot,
		),
		FinalArtifact: durableProjection.FinalArtifact,
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
