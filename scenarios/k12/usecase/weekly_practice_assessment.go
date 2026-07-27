package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type verifiedSolutionWeeklyAssessor struct {
	grader VerifiedSolutionGrader
}

// NewVerifiedSolutionWeeklyAssessor adapts the already-wired independent
// verified-solution grader to weekly-practice assessment.
func NewVerifiedSolutionWeeklyAssessor(
	grader VerifiedSolutionGrader,
) WeeklyPracticeAnswerAssessor {
	if grader == nil {
		return nil
	}
	return verifiedSolutionWeeklyAssessor{grader: grader}
}

func (a verifiedSolutionWeeklyAssessor) AssessWeeklyPracticeAnswer(
	ctx context.Context,
	req WeeklyPracticeAnswerRequest,
) (WeeklyPracticeAnswerAssessment, error) {
	outcome, err := a.grader.GradeVerified(ctx, "数学", req.Item.PromptMarkdown,
		req.StudentAnswer, req.VerifiedSolution)
	if err != nil {
		return WeeklyPracticeAnswerAssessment{}, err
	}
	result := k12.WeeklyAttemptNeedsReview
	switch outcome.Verdict {
	case VerdictAgree:
		result = k12.WeeklyAttemptCorrect
	case VerdictDisagree:
		result = k12.WeeklyAttemptWrong
	}
	binding := digestValue(struct {
		SnapshotID, ItemID, StudentAnswer, VerifiedSolution, Result string
	}{
		req.SnapshotID, req.Item.ItemID, req.StudentAnswer, req.VerifiedSolution, result,
	})
	return WeeklyPracticeAnswerAssessment{
		AssessmentID:         "weekly-assessment-" + shortDigest(binding),
		Result:               result,
		VerificationEvidence: "verified_solution_grader:" + string(outcome.Verdict),
		Subject:              "数学",
		KnowledgePoint:       strings.TrimSpace(outcome.KnowledgePoint),
	}, nil
}

func (d Deps) submitWeeklyPracticeAttemptDurable(
	ctx context.Context,
	agent, snapshotID, itemID, studentAnswer, idempotencyKey string,
) (k12.WeeklyPracticeAttempt, bool, error) {
	agent = strings.TrimSpace(agent)
	snapshotID = strings.TrimSpace(snapshotID)
	itemID = strings.TrimSpace(itemID)
	studentAnswer = strings.TrimSpace(studentAnswer)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if agent == "" || snapshotID == "" || itemID == "" ||
		studentAnswer == "" || idempotencyKey == "" {
		return k12.WeeklyPracticeAttempt{}, false,
			fmt.Errorf("%w: complete attempt required", ErrInvalidInput)
	}
	if d.Records == nil {
		return k12.WeeklyPracticeAttempt{}, false,
			fmt.Errorf("%w: weekly practice store unavailable", ErrSolveFailed)
	}
	if d.WeeklyAssessment == nil {
		return k12.WeeklyPracticeAttempt{}, false,
			fmt.Errorf("%w: weekly answer assessor unavailable", ErrSolveFailed)
	}
	snapshot, err := d.GetWeeklyPracticeSnapshot(ctx, agent, snapshotID)
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	var item *k12.WeeklyPracticeItem
	for i := range snapshot.Tracks {
		for j := range snapshot.Tracks[i].Items {
			if snapshot.Tracks[i].Items[j].ItemID == itemID {
				item = &snapshot.Tracks[i].Items[j]
				break
			}
		}
	}
	if item == nil {
		return k12.WeeklyPracticeAttempt{}, false, records.ErrNotFound
	}
	verifiedSolution := strings.TrimSpace(snapshot.AnswerKeys[itemID])
	if verifiedSolution == "" {
		return k12.WeeklyPracticeAttempt{}, false,
			fmt.Errorf("%w: frozen answer key unavailable", ErrSolveFailed)
	}
	requestDigest := digestValue(struct{ Snapshot, Item, Answer string }{
		snapshotID, itemID, studentAnswer,
	})
	now := d.now()
	command := k12.WeeklyPracticeAssessmentCommand{
		CommandID:      "wassessment-command-" + shortDigest(snapshotID+"\x00"+itemID+"\x00"+idempotencyKey),
		AgentName:      agent,
		SnapshotID:     snapshotID,
		ItemID:         itemID,
		IdempotencyKey: idempotencyKey,
		RequestDigest:  requestDigest,
		Status:         k12.WeeklyAssessmentPrepared,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	command, _, err = d.Records.PrepareWeeklyPracticeAssessmentCommand(ctx, command)
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	if command.Status == k12.WeeklyAssessmentCommitted {
		attempt, getErr := d.Records.GetWeeklyPracticeAttempt(ctx, agent, command.AttemptID)
		return attempt, true, getErr
	}
	command, assessment, err := d.resolveWeeklyPracticeAssessment(
		ctx, command, *item, studentAnswer, verifiedSolution)
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	if command.Status == k12.WeeklyAssessmentCommitted {
		attempt, getErr := d.Records.GetWeeklyPracticeAttempt(ctx, agent, command.AttemptID)
		return attempt, true, getErr
	}
	attempt := k12.WeeklyPracticeAttempt{
		AttemptID:           "wattempt-" + shortDigest(snapshotID+"\x00"+itemID+"\x00"+idempotencyKey),
		SnapshotID:          snapshotID,
		ItemID:              itemID,
		AssessmentID:        assessment.AssessmentID,
		Result:              assessment.Result,
		VerificationEvidence: assessment.VerificationEvidence,
		CreatedAt:            command.CreatedAt,
	}
	effects, err := d.weeklyPracticeAssessmentEffects(
		ctx, agent, *item, studentAnswer, verifiedSolution, assessment, &attempt)
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	return d.Records.CommitWeeklyPracticeAssessment(ctx, command, attempt, effects, d.now())
}

func (d Deps) resolveWeeklyPracticeAssessment(
	ctx context.Context,
	command k12.WeeklyPracticeAssessmentCommand,
	item k12.WeeklyPracticeItem,
	studentAnswer, verifiedSolution string,
) (k12.WeeklyPracticeAssessmentCommand, WeeklyPracticeAnswerAssessment, error) {
	for {
		switch command.Status {
		case k12.WeeklyAssessmentSucceeded:
			var assessment WeeklyPracticeAnswerAssessment
			if err := json.Unmarshal([]byte(command.AssessmentJSON), &assessment); err != nil {
				return command, assessment,
					fmt.Errorf("%w: invalid durable weekly assessment", ErrSolveFailed)
			}
			return command, assessment, nil
		case k12.WeeklyAssessmentCommitted:
			return command, WeeklyPracticeAnswerAssessment{}, nil
		case k12.WeeklyAssessmentPrepared:
			claimed, won, err := d.Records.ClaimWeeklyPracticeAssessment(
				ctx, command, d.now())
			if err != nil {
				return command, WeeklyPracticeAnswerAssessment{}, err
			}
			command = claimed
			if !won {
				continue
			}
			assessment, assessErr := d.WeeklyAssessment.AssessWeeklyPracticeAnswer(
				ctx, WeeklyPracticeAnswerRequest{
					AgentName: command.AgentName, SnapshotID: command.SnapshotID,
					Item: item, StudentAnswer: studentAnswer, VerifiedSolution: verifiedSolution,
				})
			if assessErr != nil {
				status := k12.WeeklyAssessmentFailed
				if errors.Is(assessErr, context.Canceled) ||
					errors.Is(assessErr, context.DeadlineExceeded) {
					status = k12.WeeklyAssessmentOutcomeUnknown
				}
				_ = d.Records.MarkWeeklyPracticeAssessmentTerminal(
					context.Background(), command, status, "provider_error", d.now())
				return command, WeeklyPracticeAnswerAssessment{},
					fmt.Errorf("%w: weekly answer assessment: %v", ErrSolveFailed, assessErr)
			}
			if err := validateWeeklyPracticeAssessment(assessment); err != nil {
				_ = d.Records.MarkWeeklyPracticeAssessmentTerminal(
					context.Background(), command, k12.WeeklyAssessmentFailed,
					"invalid_result", d.now())
				return command, WeeklyPracticeAnswerAssessment{}, err
			}
			payload, err := json.Marshal(assessment)
			if err != nil {
				return command, WeeklyPracticeAnswerAssessment{}, err
			}
			command, err = d.Records.MarkWeeklyPracticeAssessmentSucceeded(
				ctx, command, string(payload), digestValue(assessment), d.now())
			if err != nil {
				return command, WeeklyPracticeAnswerAssessment{}, err
			}
		case k12.WeeklyAssessmentSent:
			updated, err := d.waitWeeklyPracticeAssessment(ctx, command)
			if err != nil {
				return command, WeeklyPracticeAnswerAssessment{}, err
			}
			command = updated
		case k12.WeeklyAssessmentFailed:
			return command, WeeklyPracticeAnswerAssessment{},
				fmt.Errorf("%w: durable weekly assessment failed", ErrSolveFailed)
		case k12.WeeklyAssessmentOutcomeUnknown:
			return command, WeeklyPracticeAnswerAssessment{},
				fmt.Errorf("%w: weekly assessment outcome requires reconciliation", ErrSolveFailed)
		default:
			return command, WeeklyPracticeAnswerAssessment{},
				fmt.Errorf("%w: invalid weekly assessment state %q", ErrSolveFailed, command.Status)
		}
	}
}

func (d Deps) waitWeeklyPracticeAssessment(
	ctx context.Context,
	command k12.WeeklyPracticeAssessmentCommand,
) (k12.WeeklyPracticeAssessmentCommand, error) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return command, ctx.Err()
		case <-timer.C:
			return command,
				fmt.Errorf("%w: weekly assessment still sent; reconciliation required", ErrSolveFailed)
		case <-ticker.C:
			updated, err := d.Records.GetWeeklyPracticeAssessmentCommand(
				ctx, command.AgentName, command.SnapshotID, command.ItemID,
				command.IdempotencyKey, command.RequestDigest)
			if err != nil {
				return command, err
			}
			if updated.Status != k12.WeeklyAssessmentSent {
				return updated, nil
			}
		}
	}
}

func validateWeeklyPracticeAssessment(assessment WeeklyPracticeAnswerAssessment) error {
	if strings.TrimSpace(assessment.AssessmentID) == "" ||
		strings.TrimSpace(assessment.VerificationEvidence) == "" {
		return fmt.Errorf("%w: incomplete weekly assessment", ErrSolveFailed)
	}
	switch assessment.Result {
	case k12.WeeklyAttemptCorrect, k12.WeeklyAttemptWrong, k12.WeeklyAttemptNeedsReview:
		return nil
	default:
		return fmt.Errorf("%w: invalid weekly assessment result", ErrSolveFailed)
	}
}

func (d Deps) weeklyPracticeAssessmentEffects(
	ctx context.Context,
	agent string,
	item k12.WeeklyPracticeItem,
	studentAnswer, verifiedSolution string,
	assessment WeeklyPracticeAnswerAssessment,
	attempt *k12.WeeklyPracticeAttempt,
) (k12storage.GradingAssessmentEffects, error) {
	if item.PlanSection == k12.WeeklySectionDueReview {
		attempt.ReviewScheduled = true
		if assessment.Result != k12.WeeklyAttemptCorrect {
			return k12storage.GradingAssessmentEffects{}, nil
		}
		rec, err := d.Records.Get(ctx, item.SourceRef)
		if err != nil || rec.AgentName != agent || rec.Collection != k12.CollectionMistakes {
			return k12storage.GradingAssessmentEffects{}, records.ErrNotFound
		}
		if rec.Status == k12.StatusMastered {
			return k12storage.GradingAssessmentEffects{}, nil
		}
		fields, _ := k12.ParseMistakeFields(rec.Fields)
		now := d.now()
		status := k12.StatusRetried
		var due *int64
		if rec.Status == k12.StatusRetried && fields.LastRetriedAt > 0 &&
			now-fields.LastRetriedAt >= MasteryGapInterval {
			status = k12.StatusMastered
			fields.LastRetriedAt = now
		} else {
			fields.ReviewStage++
			fields.LastRetriedAt = now
			next := now + reviewIntervalForStage(fields.ReviewStage)
			due = &next
		}
		return k12storage.GradingAssessmentEffects{Review: &k12storage.GradingReviewEffect{
			RecordID: rec.RecordID, ExpectedVersion: rec.Version,
			NewStatus: status, Fields: fields, DueAt: due,
		}}, nil
	}
	if assessment.Result != k12.WeeklyAttemptWrong {
		return k12storage.GradingAssessmentEffects{}, nil
	}
	attempt.ReviewScheduled = true
	subject := strings.TrimSpace(assessment.Subject)
	if subject == "" {
		subject = "数学"
	}
	knowledgePoint := strings.TrimSpace(assessment.KnowledgePoint)
	if knowledgePoint == "" {
		knowledgePoint = "其他"
	}
	due := d.now() + FirstReviewInterval
	return k12storage.GradingAssessmentEffects{Mistake: &k12storage.GradingMistakeEffect{
		SourceSession: "weekly-attempt:" + attempt.AttemptID,
		DueAt:         &due,
		Fields: k12.MistakeFields{
			GradeTerm: d.creationGradeTerm(ctx, agent, ""),
			Subject: subject, Question: item.PromptMarkdown,
			KnowledgePoint: knowledgePoint, ErrorCause: "其他",
			WrongProcess: studentAnswer, CanonicalAnswer: verifiedSolution,
			EntrySource: k12.MistakeEntryVerified,
		},
	}}, nil
}
