package usecase

import (
	"context"
	"errors"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
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
	var finalArtifact *k12.GradingFinalArtifact
	artifact, artifactErr := o.deps.Records.GetGradingFinalArtifactByJob(
		ctx, agentName, jobID,
	)
	if artifactErr == nil {
		finalArtifact = &artifact
	} else if !errors.Is(artifactErr, records.ErrNotFound) {
		return ImageTaskHomeworkProjection{}, artifactErr
	}
	return ImageTaskHomeworkProjection{
		Stage: job.Record.Status, Retryable: job.Fields.Retryable,
		ConfirmationState: job.Fields.ConfirmationState,
		AnchorState: job.Fields.AnchorState, Subject: subject,
		Questions: cloneRecognizedQuestions(questions),
		FinalArtifact: finalArtifact,
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
