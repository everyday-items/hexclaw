package usecase

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestREGK12RecognitionPlanVersion20260808001StartPhotoSelectsServerOwnedPlanForNewPage(t *testing.T) {
	trustedV1 := frozenWiringBudget()
	trustedV1.StageSeconds.Recognizing = 120
	trustedV2 := recognitionLayoutInitialV2Budget()

	for _, test := range []struct {
		name           string
		trusted        k12.GradingBudgetSnapshot
		callerAttempt  k12.GradingBudgetSnapshot
		image          []byte
		wantVersion    int
		wantRecognize  int64
		wantV2Controls bool
	}{
		{
			name:    "trusted v1 keeps dense page on v1",
			trusted: trustedV1, callerAttempt: trustedV2,
			image:       planSelectorPagePNG(t, 800, 1200),
			wantVersion: k12.RecognitionPlanVersionV1, wantRecognize: 120,
		},
		{
			name:    "trusted v2 selects ordinary page v1",
			trusted: trustedV2, callerAttempt: trustedV2,
			image:       planSelectorPagePNG(t, 640, 640),
			wantVersion: k12.RecognitionPlanVersionV1, wantRecognize: 120,
		},
		{
			name:    "trusted v2 selects dense page v2",
			trusted: trustedV2, callerAttempt: trustedV1,
			image:         planSelectorPagePNG(t, 800, 1200),
			wantVersion:   k12.RecognitionPlanVersionV2,
			wantRecognize: trustedV2.StageSeconds.Recognizing, wantV2Controls: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
			deps.GradingBudgetSnapshot = test.trusted
			orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
				deps,
				orchestratorSnapshotResolver,
			))
			job, created, err := orchestrator.StartPhotoGradingJob(
				context.Background(),
				StartPhotoGradingInput{
					Photo: PhotoGradeRequest{
						AgentName: "mingming", Grade: "五年级上",
						SourceSession: "plan-selector", Image: test.image,
					},
					SourceKind: "desktop", SourceKey: "plan-selector-" + test.name,
					BudgetSnapshot: test.callerAttempt,
				},
			)
			if err != nil || !created {
				t.Fatalf("StartPhotoGradingJob created=%v err=%v", created, err)
			}
			selected := job.Fields.BudgetSnapshot
			if selected.RecognitionPlanVersion != test.wantVersion ||
				selected.StageSeconds.Recognizing != test.wantRecognize {
				t.Fatalf("selected recognition policy=%+v want version=%d recognizing=%d", selected, test.wantVersion, test.wantRecognize)
			}
			if test.wantV2Controls {
				if selected.RecognizingBuckets != trustedV2.RecognizingBuckets ||
					selected.PhysicalCallCapMillis != 120000 ||
					selected.WorkerHardCap != 2 || selected.EffectiveConcurrency != 1 {
					t.Fatalf("selected dense v2 controls=%+v", selected)
				}
			} else if !selected.RecognizingBuckets.IsZero() ||
				selected.PhysicalCallCapMillis != 0 || selected.WorkerHardCap != 0 ||
				selected.EffectiveConcurrency != 0 {
				t.Fatalf("selected v1 retained v2-only controls: %+v", selected)
			}
		})
	}
}

func TestREGK12RecognitionPlanVersion20260808001IdempotentReplayKeepsFrozenPhotoPlan(t *testing.T) {
	deps, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	deps.GradingBudgetSnapshot = recognitionLayoutInitialV2Budget()
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		orchestratorSnapshotResolver,
	))
	input := StartPhotoGradingInput{
		Photo: PhotoGradeRequest{
			AgentName: "mingming", Grade: "五年级上", SourceSession: "plan-replay",
			Image: planSelectorPagePNG(t, 800, 1200),
		},
		SourceKind: "desktop", SourceKey: "plan-selector-idempotent",
		BudgetSnapshot: frozenWiringBudget(),
	}
	first, created, err := orchestrator.StartPhotoGradingJob(context.Background(), input)
	if err != nil || !created || first.Fields.BudgetSnapshot.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 {
		t.Fatalf("first dense plan: created=%v plan=%d err=%v", created, first.Fields.BudgetSnapshot.RecognitionPlanVersion, err)
	}

	trustedV1 := frozenWiringBudget()
	trustedV1.StageSeconds.Recognizing = 120
	orchestrator.deps.GradingBudgetSnapshot = trustedV1
	replayed, created, err := orchestrator.StartPhotoGradingJob(context.Background(), input)
	if err != nil || created || replayed.Record.RecordID != first.Record.RecordID ||
		replayed.Fields.BudgetSnapshot.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 {
		t.Fatalf("idempotent replay changed frozen dense plan: created=%v plan=%d err=%v", created, replayed.Fields.BudgetSnapshot.RecognitionPlanVersion, err)
	}
}

func TestREGK12RecognitionPlanVersion20260808001DirectCreateKeepsGlobalPolicyWithoutPhotoSelection(t *testing.T) {
	deps, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	deps.GradingBudgetSnapshot = recognitionLayoutInitialV2Budget()
	job, created, err := deps.CreateGradingJob(
		context.Background(),
		"mingming",
		"direct-create",
		CreateGradingJobInput{
			SubmissionID: "direct-create-no-photo-selector",
			SourceKind:   "desktop", SourceKey: "direct-create-no-photo-selector",
			ModelSnapshot:               orchestratorSnapshot(),
			BudgetSnapshot:              frozenWiringBudget(),
			MaterializesProblemAttempts: true,
		},
	)
	if err != nil || !created ||
		job.Fields.BudgetSnapshot.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 {
		t.Fatalf("direct CreateGradingJob changed established global policy behavior: created=%v policy=%+v err=%v", created, job.Fields.BudgetSnapshot, err)
	}
}

func planSelectorPagePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatalf("encode plan selector fixture %dx%d: %v", width, height, err)
	}
	return encoded.Bytes()
}
