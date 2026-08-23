package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/webhook"
)

func TestBUG20260727002_TextWebhookWithoutConceptFactsProducesGeneralGuidance(t *testing.T) {
	runtime := newK12WebhookRuntime(t)
	snapshot := func(requested k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		if requested.Provider != "" || requested.Model != "" {
			return k12.NormalizeGradingModelSnapshot(requested), nil
		}
		return k12.GradingModelSnapshot{
			Provider: "test", Model: "test-model", Capability: "vision",
		}, nil
	}
	grading := usecase.NewGradingOrchestrator(
		runtime.Deps,
		snapshot,
		usecase.WithGradingRunDir(t.TempDir()),
	)
	app := k12WebhookApplication{deps: runtime.Deps, grading: grading, snapshot: snapshot}
	event := webhook.K12Dispatch{
		ReceiptID: "receipt-general-guidance",
		BindingID: "binding-general-guidance",
		EventID:   "delivery-general-guidance",
		EventType: webhook.K12EventSubmissionRequested,
		AgentID:   "kid-agent",
		LearnerID: "kid-learner",
		Payload: json.RawMessage(
			`{"text":"12 和 18 的最大公约数是多少？","subject":"数学","source_session":"parent-chat"}`,
		),
	}

	result, err := app.handle(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	jobID := strings.TrimPrefix(result.Reference, "grading_job:")
	if jobID == result.Reference || jobID == "" {
		t.Fatalf("text webhook reference=%q", result.Reference)
	}
	typed, err := runtime.Deps.Records.GetProblemAttemptSnapshot(
		context.Background(), "kid-agent", "webhook-receipt:"+event.ReceiptID,
	)
	if err != nil || len(typed.Problems) != 1 {
		t.Fatalf("typed Problem snapshot=%+v err=%v", typed, err)
	}
	if len(typed.Problems[0].ConceptIDs) != 0 {
		t.Fatalf("text webhook guessed concept facts before confirmation: %+v", typed.Problems[0].ConceptIDs)
	}
	if _, ok, err := grading.ConfirmPersistedTextGradingJob(
		context.Background(),
		"kid-agent",
		jobID,
		usecase.ConfirmPhotoGradingInput{
			Corrections: []usecase.GradingQuestionCorrection{{
				ProblemID: typed.Problems[0].ProblemID,
				Confirmed: true,
			}},
		},
	); err != nil || !ok {
		t.Fatalf("confirm text webhook ok=%v err=%v", ok, err)
	}

	var artifact k12.GradingFinalArtifact
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		artifact, err = runtime.Deps.Records.GetGradingFinalArtifactByJob(
			context.Background(), "kid-agent", jobID,
		)
		if err == nil {
			break
		}
		if !errors.Is(err, records.ErrNotFound) {
			t.Fatalf("load final artifact: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		job, _ := runtime.Deps.GetGradingJob(context.Background(), "kid-agent", jobID)
		t.Fatalf("text webhook did not converge to a final artifact: status=%s err=%v", job.Record.Status, err)
	}
	if artifact.CoverageStatus != k12.GradingFinalArtifactCoverageStatus("general_guidance") {
		t.Fatalf("coverage=%q want general_guidance", artifact.CoverageStatus)
	}
	if artifact.SummaryInvocationID != "" {
		t.Fatalf("general guidance started a full TutoringTips summary: %q", artifact.SummaryInvocationID)
	}
	if !strings.Contains(artifact.CanonicalMarkdown, "No verified textbook grounding is available.") {
		t.Fatalf("general guidance omitted the grounding boundary: %q", artifact.CanonicalMarkdown)
	}
	if strings.Contains(artifact.CanonicalMarkdown, usecase.TutoringTipsSourceTextbook) {
		t.Fatalf("general guidance fabricated a textbook source: %q", artifact.CanonicalMarkdown)
	}

	reloaded, err := runtime.Deps.Records.GetProblemAttemptSnapshot(
		context.Background(), "kid-agent", "webhook-receipt:"+event.ReceiptID,
	)
	if err != nil || len(reloaded.Problems) != 1 || len(reloaded.Problems[0].ConceptIDs) != 0 {
		t.Fatalf("finalization rewrote frozen concept facts: snapshot=%+v err=%v", reloaded, err)
	}

	restartedGrading := usecase.NewGradingOrchestrator(
		runtime.Deps,
		snapshot,
		usecase.WithGradingRunDir(t.TempDir()),
	)
	restartedApp := k12WebhookApplication{
		deps: runtime.Deps, grading: restartedGrading, snapshot: snapshot,
	}
	replayed, err := restartedApp.handle(context.Background(), event)
	if err != nil {
		t.Fatalf("replay after application restart: %v", err)
	}
	if replayed.Reference != result.Reference || replayed.Status != webhook.K12ReceiptSucceeded {
		t.Fatalf("replay result=%+v want stable reference=%q", replayed, result.Reference)
	}
	replayedArtifact, err := runtime.Deps.Records.GetGradingFinalArtifactByJob(
		context.Background(), "kid-agent", jobID,
	)
	if err != nil || replayedArtifact.ArtifactDigest != artifact.ArtifactDigest ||
		replayedArtifact.SummaryInvocationID != "" ||
		!strings.Contains(replayedArtifact.CanonicalMarkdown, "No verified textbook grounding is available.") {
		t.Fatalf("replay changed durable general guidance: artifact=%+v err=%v", replayedArtifact, err)
	}
}
