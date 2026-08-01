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

func (d Deps) projectWeeklyArithmetic(
	ctx context.Context,
	plan k12.WeeklyPracticePlan,
) (k12.WeeklyPracticePlan, error) {
	for index := range plan.Tracks {
		track := &plan.Tracks[index]
		track.ArithmeticBatch = nil
		if track.PlanSection != k12.WeeklySectionArithmeticWarmup {
			continue
		}
		track.Items = []k12.WeeklyPracticeItem{}
		batch, err := d.Records.GetLatestWeeklyArithmeticBatch(
			ctx, plan.AgentName, plan.PlanID)
		if errors.Is(err, records.ErrNotFound) {
			continue
		}
		if err != nil {
			return k12.WeeklyPracticePlan{}, err
		}
		track.ArithmeticBatch = &batch
		switch batch.State {
		case k12.WeeklyArithmeticReady,
			k12.WeeklyArithmeticInProgress,
			k12.WeeklyArithmeticCompleted:
			track.Status = k12.WeeklyTrackReady
			track.FailureMessage = ""
			track.Items = append([]k12.WeeklyPracticeItem(nil), batch.Items...)
		default:
			track.Status = k12.WeeklyTrackFailed
			track.FailureMessage = batch.FailureMessage
		}
	}
	if plan.Status == k12.WeeklyPlanDraft {
		progress, err := d.GetCurriculumProgress(ctx, plan.AgentName, "math")
		if err != nil {
			return k12.WeeklyPracticePlan{}, err
		}
		if progress != nil && plan.CurriculumProgressRevision != nil &&
			progress.Revision > *plan.CurriculumProgressRevision {
			for index := range plan.Tracks {
				if plan.Tracks[index].PlanSection ==
					k12.WeeklySectionTextbookConsolidation &&
					plan.Tracks[index].Status != k12.WeeklyTrackDisabled {
					plan.Tracks[index].Status = k12.WeeklyTrackStale
				}
			}
		}
	}
	return d.projectWeeklyManualRecommendations(ctx, plan)
}

func (d Deps) CreateWeeklyArithmeticBatch(
	ctx context.Context,
	agentName, planID string,
	expectedRevision int,
	key string,
) (k12.WeeklyArithmeticBatch, bool, error) {
	agentName, planID, key = strings.TrimSpace(agentName), strings.TrimSpace(planID), strings.TrimSpace(key)
	if agentName == "" || planID == "" || key == "" || expectedRevision < 1 {
		return k12.WeeklyArithmeticBatch{}, false, fmt.Errorf("%w: invalid arithmetic create", ErrInvalidInput)
	}
	settings, err := d.GetWeeklyPracticeSettings(ctx, agentName)
	if err != nil {
		return k12.WeeklyArithmeticBatch{}, false, err
	}
	return d.createWeeklyArithmeticBatch(
		ctx, agentName, planID, expectedRevision,
		min(20, max(1, settings.ArithmeticMinutes*2)), key)
}

func (d Deps) CreateWeeklyArithmeticBatchWithItemCount(
	ctx context.Context,
	agentName, planID string,
	expectedRevision, itemCount int,
	key string,
) (k12.WeeklyArithmeticBatch, bool, error) {
	agentName, planID, key = strings.TrimSpace(agentName), strings.TrimSpace(planID), strings.TrimSpace(key)
	if agentName == "" || planID == "" || key == "" || expectedRevision < 1 ||
		itemCount < 1 || itemCount > 20 {
		return k12.WeeklyArithmeticBatch{}, false, fmt.Errorf("%w: invalid arithmetic create", ErrInvalidInput)
	}
	return d.createWeeklyArithmeticBatch(ctx, agentName, planID, expectedRevision, itemCount, key)
}

func (d Deps) createWeeklyArithmeticBatch(
	ctx context.Context,
	agentName, planID string,
	expectedRevision, itemCount int,
	key string,
) (k12.WeeklyArithmeticBatch, bool, error) {
	progress, err := d.GetCurriculumProgress(ctx, agentName, "math")
	if err != nil || progress == nil {
		return k12.WeeklyArithmeticBatch{}, false, records.ErrIllegalTransition
	}
	checkpoint := WeeklyPracticeCandidateRequest{
		AgentName: agentName, PlanSection: k12.WeeklySectionArithmeticWarmup,
		MaxItems: itemCount, ArithmeticMinutes: max(1, (itemCount+1)/2), Progress: *progress,
	}
	checkpointJSON, _ := json.Marshal(checkpoint)
	digest := digestValue(struct {
		Agent, Plan string
		Revision    int
		ItemCount   int
	}{agentName, planID, expectedRevision, itemCount})
	batch, replay, err := d.Records.PrepareWeeklyArithmeticBatch(
		ctx, agentName, planID, expectedRevision, key, digest,
		string(checkpointJSON), d.now())
	if err != nil || replay {
		return batch, replay, err
	}
	if err := d.finishWeeklyArithmeticGeneration(ctx, batch); err != nil {
		return batch, false, fmt.Errorf("finish weekly arithmetic generation: %w", err)
	}
	return batch, false, nil
}

func (d Deps) finishWeeklyArithmeticGeneration(
	ctx context.Context,
	batch k12.WeeklyArithmeticBatch,
) error {
	var request WeeklyPracticeCandidateRequest
	if err := json.Unmarshal([]byte(batch.GenerationCheckpoint), &request); err != nil {
		if err := d.Records.FinishWeeklyArithmeticGeneration(ctx, batch.AgentName,
			batch.BatchID, k12.WeeklyArithmeticFailedTerminal, nil, nil, "",
			"invalid generation checkpoint", d.now()); err != nil {
			return err
		}
		return nil
	}
	if d.WeeklyCandidates == nil {
		if err := d.Records.FinishWeeklyArithmeticGeneration(ctx, batch.AgentName,
			batch.BatchID, k12.WeeklyArithmeticFailedRetryable, nil, nil, "",
			"arithmetic generator unavailable", d.now()); err != nil {
			return err
		}
		return nil
	}
	candidates, err := d.WeeklyCandidates.GenerateWeeklyPracticeCandidates(ctx, request)
	if err != nil {
		if finishErr := d.Records.FinishWeeklyArithmeticGeneration(ctx, batch.AgentName,
			batch.BatchID, k12.WeeklyArithmeticFailedRetryable, nil, nil, "",
			err.Error(), d.now()); finishErr != nil {
			return finishErr
		}
		return nil
	}
	items, keys, valid := weeklyArithmeticItems(request, candidates)
	if !valid {
		if err := d.Records.FinishWeeklyArithmeticGeneration(ctx, batch.AgentName,
			batch.BatchID, k12.WeeklyArithmeticFailedTerminal, nil, nil, "",
			"invalid arithmetic generation result", d.now()); err != nil {
			return err
		}
		return nil
	}
	contentDigest := digestValue(struct {
		Items []k12.WeeklyPracticeItem
		Keys  map[string]string
	}{items, keys})
	if err := d.Records.FinishWeeklyArithmeticGeneration(ctx, batch.AgentName,
		batch.BatchID, k12.WeeklyArithmeticReady, items, keys,
		contentDigest, "", d.now()); err != nil {
		return err
	}
	return nil
}

func weeklyArithmeticItems(
	request WeeklyPracticeCandidateRequest,
	candidates []WeeklyPracticeCandidate,
) ([]k12.WeeklyPracticeItem, map[string]string, bool) {
	items := make([]k12.WeeklyPracticeItem, 0, len(candidates))
	keys := make(map[string]string)
	for _, candidate := range candidates {
		method := strings.TrimSpace(candidate.GenerationMethod)
		answer := strings.TrimSpace(candidate.ExpectedAnswer)
		if len(items) >= request.MaxItems {
			break
		}
		if strings.TrimSpace(candidate.PromptMarkdown) == "" ||
			strings.TrimSpace(candidate.SourceKind) == "" ||
			strings.TrimSpace(candidate.SourceRef) == "" ||
			!k12.WeeklySupplementGenerationMethodAllowed(method) ||
			len(candidate.EvidenceRefs) == 0 || answer == "" {
			return nil, nil, false
		}
		itemID := "witem-" + shortDigest(
			request.PlanSection+"\x00"+candidate.SourceRef+"\x00"+candidate.PromptMarkdown)
		items = append(items, k12.WeeklyPracticeItem{
			ItemID: itemID, Position: len(items) + 1,
			PlanSection: request.PlanSection, SourceKind: candidate.SourceKind,
			GenerationMethod: method, SourceRef: candidate.SourceRef,
			Verification: k12.WeeklyPracticeVerification{
				Status:       k12.WeeklyVerificationVerified,
				EvidenceRefs: append([]string(nil), candidate.EvidenceRefs...),
			},
			PromptMarkdown: candidate.PromptMarkdown,
		})
		keys[itemID] = answer
	}
	return items, keys, len(items) == request.MaxItems
}

func (d Deps) StartWeeklyArithmeticBatch(
	ctx context.Context,
	agentName, batchID, key string,
) (k12.WeeklyArithmeticBatch, bool, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(batchID) == "" ||
		strings.TrimSpace(key) == "" {
		return k12.WeeklyArithmeticBatch{}, false, fmt.Errorf("%w: invalid arithmetic start", ErrInvalidInput)
	}
	digest := digestValue(struct{ Agent, Batch string }{agentName, batchID})
	return d.Records.StartWeeklyArithmeticBatch(
		ctx, agentName, batchID, key, digest, d.now())
}

func (d Deps) RetryWeeklyArithmeticBatch(
	ctx context.Context,
	agentName, batchID, key string,
) (k12.WeeklyArithmeticBatch, bool, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(batchID) == "" ||
		strings.TrimSpace(key) == "" {
		return k12.WeeklyArithmeticBatch{}, false, fmt.Errorf("%w: invalid arithmetic retry", ErrInvalidInput)
	}
	digest := digestValue(struct{ Agent, Batch string }{agentName, batchID})
	batch, replay, err := d.Records.PrepareWeeklyArithmeticRetry(
		ctx, agentName, batchID, key, digest, d.now())
	if err != nil || replay {
		return batch, replay, err
	}
	if err := d.finishWeeklyArithmeticGeneration(ctx, batch); err != nil {
		return batch, false, fmt.Errorf("finish weekly arithmetic generation: %w", err)
	}
	return batch, false, nil
}

func (d Deps) SubmitWeeklyArithmeticAttempt(
	ctx context.Context,
	agentName, batchID, itemID, studentAnswer, key string,
) (k12.WeeklyArithmeticAttempt, bool, error) {
	agentName, batchID, itemID = strings.TrimSpace(agentName), strings.TrimSpace(batchID), strings.TrimSpace(itemID)
	studentAnswer, key = strings.TrimSpace(studentAnswer), strings.TrimSpace(key)
	if agentName == "" || batchID == "" || itemID == "" ||
		studentAnswer == "" || key == "" {
		return k12.WeeklyArithmeticAttempt{}, false, fmt.Errorf("%w: invalid arithmetic attempt", ErrInvalidInput)
	}
	if d.WeeklyAssessment == nil {
		return k12.WeeklyArithmeticAttempt{}, false, fmt.Errorf("%w: weekly assessor unavailable", ErrSolveFailed)
	}
	batch, err := d.Records.GetWeeklyArithmeticBatch(ctx, agentName, batchID)
	if err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	var item *k12.WeeklyPracticeItem
	for index := range batch.Items {
		if batch.Items[index].ItemID == itemID {
			item = &batch.Items[index]
		}
	}
	if item == nil || strings.TrimSpace(batch.AnswerKeys[itemID]) == "" {
		return k12.WeeklyArithmeticAttempt{}, false, records.ErrNotFound
	}
	digest := digestValue(struct{ Batch, Item, Answer string }{
		batchID, itemID, studentAnswer,
	})
	command, replayAttempt, err := d.Records.PrepareWeeklyArithmeticAttempt(
		ctx, agentName, batchID, itemID, key, digest, d.now())
	if err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	if replayAttempt != nil {
		return *replayAttempt, true, nil
	}
	assessment, command, err := d.resolveWeeklyArithmeticAssessment(
		ctx, command, *item, studentAnswer, batch.AnswerKeys[itemID])
	if err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	attempt := k12.WeeklyArithmeticAttempt{
		AttemptID: weeklyArithmeticAttemptID(batchID, itemID, key),
		BatchID:   batchID, ItemID: itemID,
		AssessmentID: assessment.AssessmentID, Result: assessment.Result,
		VerificationEvidence: assessment.VerificationEvidence,
		CreatedAt:            command.CreatedAt,
	}
	effects := k12storage.GradingAssessmentEffects{}
	if assessment.Result == k12.WeeklyAttemptWrong {
		attempt.ReviewScheduled = true
		subject := strings.TrimSpace(assessment.Subject)
		if subject == "" {
			subject = "数学"
		}
		point := strings.TrimSpace(assessment.KnowledgePoint)
		if point == "" {
			point = "其他"
		}
		due := d.now() + FirstReviewInterval
		effects.Mistake = &k12storage.GradingMistakeEffect{
			SourceSession: "weekly-arithmetic-attempt:" + attempt.AttemptID,
			DueAt:         &due,
			Fields: k12.MistakeFields{
				GradeTerm: d.creationGradeTerm(ctx, agentName, ""),
				Subject:   subject, Question: item.PromptMarkdown,
				KnowledgePoint: point, ErrorCause: "其他",
				WrongProcess:    studentAnswer,
				CanonicalAnswer: batch.AnswerKeys[itemID],
				EntrySource:     k12.MistakeEntryVerified,
			},
		}
	}
	return d.Records.CommitWeeklyArithmeticAttempt(
		ctx, command, attempt, effects, d.now())
}

func weeklyArithmeticAttemptID(batchID, itemID, key string) string {
	return "warith-attempt-" + shortDigest(batchID+"\x00"+itemID+"\x00"+key)
}

func (d Deps) resolveWeeklyArithmeticAssessment(
	ctx context.Context,
	command k12storage.WeeklyArithmeticCommand,
	item k12.WeeklyPracticeItem,
	answer, solution string,
) (WeeklyPracticeAnswerAssessment, k12storage.WeeklyArithmeticCommand, error) {
	for {
		switch command.Status {
		case "succeeded":
			var assessment WeeklyPracticeAnswerAssessment
			if err := json.Unmarshal([]byte(command.ResultJSON), &assessment); err != nil {
				return assessment, command, err
			}
			return assessment, command, nil
		case "prepared":
			stored, won, err := d.Records.ClaimWeeklyArithmeticAttempt(ctx, command, d.now())
			if err != nil {
				return WeeklyPracticeAnswerAssessment{}, command, err
			}
			command = stored
			if !won {
				continue
			}
			assessment, assessErr := d.WeeklyAssessment.AssessWeeklyPracticeAnswer(
				ctx, WeeklyPracticeAnswerRequest{
					AgentName: command.AgentName, SnapshotID: command.ScopeID,
					Item: item, StudentAnswer: answer, VerifiedSolution: solution,
				})
			if assessErr != nil {
				status := "failed"
				if errors.Is(assessErr, context.Canceled) ||
					errors.Is(assessErr, context.DeadlineExceeded) {
					status = "outcome_unknown"
				}
				_ = d.Records.MarkWeeklyArithmeticAttemptTerminal(
					context.Background(), command, status, d.now())
				return WeeklyPracticeAnswerAssessment{}, command,
					fmt.Errorf("%w: arithmetic assessment: %v", ErrSolveFailed, assessErr)
			}
			if err := validateWeeklyPracticeAssessment(assessment); err != nil {
				_ = d.Records.MarkWeeklyArithmeticAttemptTerminal(
					context.Background(), command, "failed", d.now())
				return assessment, command, err
			}
			payload, _ := json.Marshal(assessment)
			command, err = d.Records.MarkWeeklyArithmeticAttemptSucceeded(
				ctx, command, string(payload), digestValue(assessment), d.now())
			if err != nil {
				return assessment, command, err
			}
		case "sent":
			timer := time.NewTimer(2 * time.Second)
			ticker := time.NewTicker(2 * time.Millisecond)
			for command.Status == "sent" {
				select {
				case <-ctx.Done():
					timer.Stop()
					ticker.Stop()
					return WeeklyPracticeAnswerAssessment{}, command, ctx.Err()
				case <-timer.C:
					ticker.Stop()
					return WeeklyPracticeAnswerAssessment{}, command,
						fmt.Errorf("%w: arithmetic assessment reconciliation required", ErrSolveFailed)
				case <-ticker.C:
					command, _ = d.Records.GetWeeklyArithmeticCommand(ctx, command)
				}
			}
			timer.Stop()
			ticker.Stop()
		case "failed", "outcome_unknown":
			return WeeklyPracticeAnswerAssessment{}, command,
				fmt.Errorf("%w: arithmetic assessment state %s", ErrSolveFailed, command.Status)
		default:
			return WeeklyPracticeAnswerAssessment{}, command,
				fmt.Errorf("%w: arithmetic assessment state %s", ErrSolveFailed, command.Status)
		}
	}
}

func (d Deps) RefreshWeeklyTextbookTrack(
	ctx context.Context,
	agentName, planID string,
	expectedRevision int,
	key string,
) (k12.WeeklyPracticePlan, bool, bool, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(planID) == "" ||
		strings.TrimSpace(key) == "" || expectedRevision < 1 {
		return k12.WeeklyPracticePlan{}, false, false,
			fmt.Errorf("%w: invalid textbook refresh", ErrInvalidInput)
	}
	requestDigest := digestValue(struct {
		Agent, Plan string
		Revision    int
	}{agentName, planID, expectedRevision})
	plan, err := d.Records.GetWeeklyPracticePlan(ctx, agentName, planID)
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, false, err
	}
	if plan.Status != k12.WeeklyPlanDraft {
		return k12.WeeklyPracticePlan{}, false, false, records.ErrVersionConflict
	}
	if plan.Revision != expectedRevision {
		return d.Records.CommitWeeklyTextbookRefresh(
			ctx, agentName, planID, expectedRevision, key, requestDigest,
			plan, false, 0, "", d.now())
	}
	progress, err := d.GetCurriculumProgress(ctx, agentName, "math")
	if err != nil || progress == nil {
		return k12.WeeklyPracticePlan{}, false, false, records.ErrIllegalTransition
	}
	index := -1
	for i := range plan.Tracks {
		if plan.Tracks[i].PlanSection == k12.WeeklySectionTextbookConsolidation {
			index = i
		}
	}
	if index < 0 || plan.Tracks[index].Status == k12.WeeklyTrackDisabled {
		return k12.WeeklyPracticePlan{}, false, false, records.ErrIllegalTransition
	}
	stale := plan.CurriculumProgressRevision != nil &&
		progress.Revision > *plan.CurriculumProgressRevision
	failed := plan.Tracks[index].Status == k12.WeeklyTrackFailed
	if !stale && !failed {
		return k12.WeeklyPracticePlan{}, false, false, records.ErrIllegalTransition
	}
	request := WeeklyPracticeCandidateRequest{
		AgentName: agentName, PlanSection: k12.WeeklySectionTextbookConsolidation,
		MaxItems: 4, Progress: *progress,
	}
	if failed && !stale {
		checkpoint, checkpointErr := d.Records.GetWeeklyTrackCheckpoint(
			ctx, agentName, planID, plan.Revision)
		if checkpointErr != nil {
			return k12.WeeklyPracticePlan{}, false, false, checkpointErr
		}
		if err := json.Unmarshal([]byte(checkpoint), &request); err != nil {
			return k12.WeeklyPracticePlan{}, false, false, err
		}
	}
	budget := max(0, 600-len(plan.Tracks[0].Items)*60)
	nextTrack, nextKeys, _ := d.weeklySupplementTrack(
		ctx, request.AgentName, request.PlanSection, true, &request.Progress,
		request.MaxItems, request.ArithmeticMinutes, budget)
	next := plan
	next.Tracks = append([]k12.WeeklyPracticeTrack(nil), plan.Tracks...)
	next.Tracks[index] = nextTrack
	for _, old := range plan.Tracks[index].Items {
		delete(next.AnswerKeys, old.ItemID)
	}
	for itemID, answer := range nextKeys {
		next.AnswerKeys[itemID] = answer
	}
	createdRevision := stale
	if createdRevision {
		next.Revision++
		revision := progress.Revision
		next.CurriculumProgressRevision = &revision
	}
	next.UpdatedAt = d.now()
	next.SourceDigest = digestValue(struct {
		PlanID   string
		Revision int
		Track    k12.WeeklyPracticeTrack
	}{next.PlanID, next.Revision, nextTrack})
	checkpointJSON, _ := json.Marshal(request)
	return d.Records.CommitWeeklyTextbookRefresh(
		ctx, agentName, planID, expectedRevision, key, requestDigest,
		next, createdRevision, 0, string(checkpointJSON), d.now())
}
