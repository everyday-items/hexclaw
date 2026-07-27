package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func (d Deps) projectWeeklyManualRecommendations(
	ctx context.Context,
	plan k12.WeeklyPracticePlan,
) (k12.WeeklyPracticePlan, error) {
	progress, err := d.GetCurriculumProgress(ctx, plan.AgentName, "math")
	if err != nil {
		return k12.WeeklyPracticePlan{}, err
	}
	textbookCount, err := d.Records.GetWeeklyManualPracticePreference(
		ctx, plan.AgentName, k12.WeeklySectionTextbookConsolidation)
	if errors.Is(err, records.ErrNotFound) {
		textbookCount, err = 5, nil
	}
	if err != nil {
		return k12.WeeklyPracticePlan{}, err
	}
	if textbookCount < 1 || textbookCount > 10 {
		textbookCount = 5
	}
	arithmeticCount, err := d.Records.GetWeeklyManualPracticePreference(
		ctx, plan.AgentName, k12.WeeklySectionArithmeticWarmup)
	if errors.Is(err, records.ErrNotFound) {
		arithmeticCount, err = 10, nil
	}
	if err != nil {
		return k12.WeeklyPracticePlan{}, err
	}
	if arithmeticCount < 1 || arithmeticCount > 20 {
		arithmeticCount = 10
	}
	syncAvailability := k12.WeeklyManualTrackAvailable
	arithmeticAvailability := k12.WeeklyManualTrackAvailable
	if plan.Status != k12.WeeklyPlanDraft {
		syncAvailability = k12.WeeklyManualTrackFailedTerminal
		arithmeticAvailability = k12.WeeklyManualTrackFailedTerminal
	} else if progress == nil || progress.Revision <= 0 ||
		progress.EvidenceSource != "parent_confirmed" {
		syncAvailability = k12.WeeklyManualTrackSetupRequired
		arithmeticAvailability = k12.WeeklyManualTrackSetupRequired
	}
	for index := range plan.Tracks {
		track := &plan.Tracks[index]
		switch track.PlanSection {
		case k12.WeeklySectionTextbookConsolidation:
			if syncAvailability == k12.WeeklyManualTrackSetupRequired {
				track.FailureMessage = "curriculum progress setup required"
			} else if track.Status == k12.WeeklyTrackFailed {
				syncAvailability = k12.WeeklyManualTrackFailedRetryable
			}
		case k12.WeeklySectionArithmeticWarmup:
			if arithmeticAvailability == k12.WeeklyManualTrackSetupRequired {
				track.FailureMessage = "curriculum progress setup required"
			}
			if track.ArithmeticBatch == nil {
				continue
			}
			switch track.ArithmeticBatch.State {
			case k12.WeeklyArithmeticPreparing:
				arithmeticAvailability = k12.WeeklyManualTrackProcessing
			case k12.WeeklyArithmeticFailedRetryable:
				arithmeticAvailability = k12.WeeklyManualTrackFailedRetryable
			case k12.WeeklyArithmeticFailedTerminal:
				arithmeticAvailability = k12.WeeklyManualTrackFailedTerminal
			}
		}
	}
	plan.ManualTrackRecommendations = k12.WeeklyManualTrackRecommendations{
		TextbookConsolidation: k12.WeeklyManualTrackRecommendation{
			Availability: syncAvailability, SelectedItemCount: textbookCount,
			RecommendedItemCount: 5, MinItemCount: 1, MaxItemCount: 10,
		},
		ArithmeticWarmup: k12.WeeklyManualTrackRecommendation{
			Availability: arithmeticAvailability, SelectedItemCount: arithmeticCount,
			RecommendedItemCount: 10, MinItemCount: 1, MaxItemCount: 20,
		},
	}
	return plan, nil
}

func (d Deps) PrepareWeeklyTextbookTrack(
	ctx context.Context,
	agentName, planID string,
	expectedRevision, itemCount int,
	key string,
) (k12.WeeklyPracticePlan, bool, error) {
	agentName, planID, key = strings.TrimSpace(agentName), strings.TrimSpace(planID),
		strings.TrimSpace(key)
	if agentName == "" || planID == "" || key == "" || expectedRevision < 1 ||
		itemCount < 1 || itemCount > 10 {
		return k12.WeeklyPracticePlan{}, false,
			fmt.Errorf("%w: invalid textbook prepare", ErrInvalidInput)
	}
	requestDigest := digestValue(struct {
		Agent, Plan         string
		Revision, ItemCount int
	}{agentName, planID, expectedRevision, itemCount})
	plan, err := d.Records.GetWeeklyPracticePlan(ctx, agentName, planID)
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	if plan.Status != k12.WeeklyPlanDraft {
		return k12.WeeklyPracticePlan{}, false, records.ErrVersionConflict
	}
	if plan.Revision != expectedRevision {
		stored, replay, _, commitErr := d.Records.CommitWeeklyTextbookRefresh(
			ctx, agentName, planID, expectedRevision, key, requestDigest,
			plan, false, 0, "", d.now())
		if commitErr != nil {
			return k12.WeeklyPracticePlan{}, false, commitErr
		}
		projected, projectErr := d.projectWeeklyArithmetic(ctx, stored)
		return projected, replay, projectErr
	}
	progress, err := d.GetCurriculumProgress(ctx, agentName, "math")
	if err != nil || progress == nil || progress.Revision <= 0 ||
		progress.EvidenceSource != "parent_confirmed" {
		return k12.WeeklyPracticePlan{}, false, records.ErrIllegalTransition
	}
	index := -1
	for i := range plan.Tracks {
		if plan.Tracks[i].PlanSection == k12.WeeklySectionTextbookConsolidation {
			index = i
			break
		}
	}
	if index < 0 {
		return k12.WeeklyPracticePlan{}, false, records.ErrIllegalTransition
	}
	budget := 600
	if len(plan.Tracks) > 0 {
		budget = max(0, 600-len(plan.Tracks[0].Items)*60)
	}
	nextTrack, nextKeys, _ := d.weeklySupplementTrack(
		ctx, agentName, k12.WeeklySectionTextbookConsolidation,
		true, progress, itemCount, 0, budget)
	if nextTrack.Status != k12.WeeklyTrackReady {
		return k12.WeeklyPracticePlan{}, false,
			fmt.Errorf("%w: %s", ErrSolveFailed, nextTrack.FailureMessage)
	}
	next := plan
	next.Tracks = append([]k12.WeeklyPracticeTrack(nil), plan.Tracks...)
	next.Tracks[index] = nextTrack
	next.AnswerKeys = make(map[string]string, len(plan.AnswerKeys)+len(nextKeys))
	for itemID, answer := range plan.AnswerKeys {
		next.AnswerKeys[itemID] = answer
	}
	for _, old := range plan.Tracks[index].Items {
		delete(next.AnswerKeys, old.ItemID)
	}
	for itemID, answer := range nextKeys {
		next.AnswerKeys[itemID] = answer
	}
	next.Revision++
	revision := progress.Revision
	next.CurriculumProgressRevision = &revision
	next.UpdatedAt = d.now()
	next.SourceDigest = digestValue(struct {
		PlanID    string
		ItemCount int
		Track     k12.WeeklyPracticeTrack
	}{next.PlanID, itemCount, nextTrack})
	checkpointJSON, _ := json.Marshal(WeeklyPracticeCandidateRequest{
		AgentName: agentName, PlanSection: k12.WeeklySectionTextbookConsolidation,
		MaxItems: itemCount, Progress: *progress,
	})
	stored, replay, _, err := d.Records.CommitWeeklyTextbookRefresh(
		ctx, agentName, planID, expectedRevision, key, requestDigest,
		next, true, itemCount, string(checkpointJSON), d.now())
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	projected, projectErr := d.projectWeeklyArithmetic(ctx, stored)
	return projected, replay, projectErr
}
