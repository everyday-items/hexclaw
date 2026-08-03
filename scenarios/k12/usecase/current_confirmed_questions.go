package usecase

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// loadCurrentConfirmedQuestions is the single read model for assessment and
// tutoring projections. V19 supplies immutable structure/Attempt identity,
// V72 supplies every answerable Problem's current text/source head, and V73
// supplies current typed recognition facts when a select/retake produced them.
// No caller may silently mix a current assessment with the original OCR facts.
func (d Deps) loadCurrentConfirmedQuestions(
	ctx context.Context,
	agentName string,
	submissionID string,
) ([]RecognizedQuestion, error) {
	if d.Records == nil {
		return nil, fmt.Errorf("usecase: canonical K12 store unavailable")
	}
	agentName = strings.TrimSpace(agentName)
	submissionID = strings.TrimSpace(submissionID)
	if agentName == "" || submissionID == "" {
		return nil, fmt.Errorf("%w: current confirmed question scope is incomplete", ErrInvalidInput)
	}
	snapshot, err := d.Records.GetProblemAttemptSnapshot(ctx, agentName, submissionID)
	if err != nil {
		return nil, fmt.Errorf("usecase: load current confirmed structure: %w", err)
	}
	questions, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	currentInputs, err := d.Records.ListCurrentProblemInputRevisions(
		ctx, agentName, submissionID,
	)
	if err != nil {
		return nil, err
	}
	for _, question := range questions {
		if question.ProblemKind == ProblemKindCompoundParent {
			continue
		}
		current, ok := currentInputs[question.ProblemID]
		if !ok || current.InputRevision < 1 || strings.TrimSpace(current.InputDigest) == "" {
			return nil, fmt.Errorf(
				"%w: answerable Problem %s has no current immutable input head",
				ErrInvalidInput,
				question.ProblemID,
			)
		}
	}
	questions = overlayCurrentProblemInputRevisions(questions, currentInputs)

	currentRecognition, err := d.Records.ListCurrentProblemSourceRecognitionFacts(
		ctx, agentName, submissionID,
	)
	if err != nil {
		return nil, err
	}
	problemIDs := make([]string, 0, len(currentRecognition))
	for problemID := range currentRecognition {
		problemIDs = append(problemIDs, problemID)
	}
	sort.Strings(problemIDs)
	for _, problemID := range problemIDs {
		fact := currentRecognition[problemID]
		questions, err = overlayProblemSourceRecognitionFacts(
			questions,
			currentRecognition,
			[]string{problemID},
			fact.InputRevision,
		)
		if err != nil {
			return nil, err
		}
	}
	for index := range questions {
		questions[index] = NormalizeRecognizedQuestion(questions[index])
	}
	return questions, nil
}
