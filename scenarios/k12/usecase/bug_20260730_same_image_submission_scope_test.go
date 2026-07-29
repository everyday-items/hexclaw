package usecase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type changingSameImageRecognizer struct {
	calls int
}

func (r *changingSameImageRecognizer) Recognize(
	context.Context,
	[]byte,
) ([]RecognizedQuestion, error) {
	r.calls++
	return []RecognizedQuestion{{
		Question:      fmt.Sprintf("4.5%s2=", []string{"×", "乘"}[(r.calls-1)%2]),
		Subject:       "数学",
		AnswerState:   AnswerStateBlank,
		StudentAnswer: "",
	}}, nil
}

func TestBUG20260730002SameImageDifferentSourcesUseIsolatedSubmissionScope(t *testing.T) {
	ctx := context.Background()
	recognizer := &changingSameImageRecognizer{}
	orchestrator := newOrchestrator(t, recognizer, nil, nil)

	start := func(sourceKey string) GradingJobView {
		t.Helper()
		view, created, err := orchestrator.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
			Photo:      orchestratorPhotoRequest(),
			SourceKind: "desktop",
			SourceKey:  sourceKey,
		})
		if err != nil || !created {
			t.Fatalf("start %s: created=%v err=%v", sourceKey, created, err)
		}
		return view
	}
	runToConfirmation := func(view GradingJobView) GradingJobView {
		t.Helper()
		got, err := orchestrator.RunGradingJob(ctx, view.Record.RecordID)
		if err != nil {
			t.Fatalf("run %s: %v", view.Record.RecordID, err)
		}
		if got.Record.Status != k12.GradingStageAwaitingConfirmation {
			t.Fatalf("run %s stage=%s, want awaiting_confirmation", view.Record.RecordID, got.Record.Status)
		}
		return got
	}

	first := runToConfirmation(start("same-image-source-a"))
	second := runToConfirmation(start("same-image-source-b"))

	if first.Fields.SubmissionID == second.Fields.SubmissionID {
		t.Fatalf(
			"independent source commands sharing image bytes reused submission scope %q",
			first.Fields.SubmissionID,
		)
	}
	firstFacts, err := orchestrator.deps.Records.GetProblemAttemptSnapshot(
		ctx,
		"mingming",
		first.Fields.SubmissionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondFacts, err := orchestrator.deps.Records.GetProblemAttemptSnapshot(
		ctx,
		"mingming",
		second.Fields.SubmissionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstFacts.Problems) != 1 || len(secondFacts.Problems) != 1 ||
		len(firstFacts.Attempts) != 1 || len(secondFacts.Attempts) != 1 {
		t.Fatalf(
			"independent submissions leaked problem/attempt exact-sets: first=%d/%d second=%d/%d",
			len(firstFacts.Problems),
			len(firstFacts.Attempts),
			len(secondFacts.Problems),
			len(secondFacts.Attempts),
		)
	}
	if firstFacts.Problems[0].ProblemID == secondFacts.Problems[0].ProblemID {
		t.Fatalf("independent submissions reused problem_id %q", firstFacts.Problems[0].ProblemID)
	}
	if firstFacts.Attempts[0].AttemptID == secondFacts.Attempts[0].AttemptID {
		t.Fatalf("independent submissions reused attempt_id %q", firstFacts.Attempts[0].AttemptID)
	}
	if firstFacts.Problems[0].StemRaw == secondFacts.Problems[0].StemRaw {
		t.Fatalf("test fixture did not preserve the two valid recognition variants")
	}

	replayed, created, err := orchestrator.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo:      orchestratorPhotoRequest(),
		SourceKind: "desktop",
		SourceKey:  "same-image-source-a",
	})
	if err != nil || created || replayed.Record.RecordID != first.Record.RecordID {
		t.Fatalf(
			"same source replay drifted: created=%v got=%s want=%s err=%v",
			created,
			replayed.Record.RecordID,
			first.Record.RecordID,
			err,
		)
	}
	if recognizer.calls != 2 {
		t.Fatalf("idempotent replay repeated recognition: calls=%d want=2", recognizer.calls)
	}
}

func TestBUG20260730002PhotoSubmissionIdentityKeepsLegacyRecoveryAndIntegrity(t *testing.T) {
	image := []byte("same-real-image-bytes")
	first := scopedPhotoSubmissionID(image, "desktop", "source-a")
	replayed := scopedPhotoSubmissionID(image, "desktop", "source-a")
	second := scopedPhotoSubmissionID(image, "desktop", "source-b")

	if first != replayed {
		t.Fatalf("same source command changed submission identity: %q != %q", first, replayed)
	}
	if first == second {
		t.Fatalf("different source commands reused submission identity %q", first)
	}
	ambiguousLeft := scopedPhotoSubmissionID(image, "desktop", "x\x00y")
	ambiguousRight := scopedPhotoSubmissionID(image, "desktop\x00x", "y")
	if ambiguousLeft == ambiguousRight {
		t.Fatalf(
			"delimiter-ambiguous source tuples reused submission identity %q",
			ambiguousLeft,
		)
	}
	if !photoSubmissionMatchesImage(first, image) {
		t.Fatalf("v2 identity rejected its original image: %q", first)
	}
	if !photoSubmissionMatchesImage(legacyPhotoSubmissionID(image), image) {
		t.Fatal("legacy photo-sha1 identity can no longer recover")
	}
	if photoSubmissionMatchesImage(first, []byte("tampered-image")) {
		t.Fatal("v2 identity accepted tampered image bytes")
	}
	if photoSubmissionMatchesImage(legacyPhotoSubmissionID(image), []byte("tampered-image")) {
		t.Fatal("legacy identity accepted tampered image bytes")
	}
	if !photoSubmissionMatchesRequest(
		legacyPhotoSubmissionID(image),
		image,
		"desktop",
		"source-a",
	) {
		t.Fatal("legacy idempotency replay no longer matches its original image")
	}
}

func TestBUG20260730002RunDirRecoversLegacyAndV2ButRejectsTamperedImage(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		submissionID func([]byte) string
	}{
		{
			name:         "legacy photo-sha1",
			submissionID: legacyPhotoSubmissionID,
		},
		{
			name: "source-scoped v2",
			submissionID: func(image []byte) string {
				return scopedPhotoSubmissionID(image, "desktop", "recovery-source")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			d := recoveryDeps(t, &countingRecognizer{}, nil, nil)
			request := orchestratorPhotoRequest()
			submissionID := tc.submissionID(request.Image)
			job, created, err := d.CreateGradingJob(
				ctx,
				request.AgentName,
				request.SourceSession,
				CreateGradingJobInput{
					SubmissionID:                submissionID,
					SourceKind:                  "desktop",
					SourceKey:                   "recovery-source",
					ModelSnapshot:               orchestratorSnapshot(),
					MaterializesProblemAttempts: true,
				},
			)
			if err != nil || !created {
				t.Fatalf("seed %s job: created=%v err=%v", tc.name, created, err)
			}

			writer := newRecoverableOrchestrator(t, d, dir)
			replayed, replayCreated, err := writer.StartPhotoGradingJob(
				ctx,
				StartPhotoGradingInput{
					Photo:      request,
					SourceKind: "desktop",
					SourceKey:  "recovery-source",
				},
			)
			if err != nil || replayCreated || replayed.Record.RecordID != job.Record.RecordID {
				t.Fatalf(
					"replay %s persisted job: created=%v got=%s want=%s err=%v",
					tc.name,
					replayCreated,
					replayed.Record.RecordID,
					job.Record.RecordID,
					err,
				)
			}

			reader := newRecoverableOrchestrator(t, d, dir)
			recovered, err := reader.ensureRun(ctx, job.Record.RecordID)
			if err != nil {
				t.Fatalf("recover %s run: %v", tc.name, err)
			}
			if string(recovered.req.Image) != string(request.Image) {
				t.Fatalf("recover %s image bytes drifted", tc.name)
			}

			if err := os.WriteFile(
				writer.runPath(job.Record.RecordID, "image.bin"),
				[]byte("tampered-image"),
				0o600,
			); err != nil {
				t.Fatalf("tamper %s run image: %v", tc.name, err)
			}
			tamperReader := newRecoverableOrchestrator(t, d, dir)
			if _, err := tamperReader.ensureRun(ctx, job.Record.RecordID); err == nil {
				t.Fatalf("%s run accepted tampered image bytes", tc.name)
			}
		})
	}
}

func TestBUG20260730002ReservedSourceKindRejectedBeforeLookupOrModelResolution(t *testing.T) {
	ctx := context.Background()
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	resolverCalls := 0
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		d,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			resolverCalls++
			return orchestratorSnapshot(), nil
		},
	))

	for _, sourceKind := range []string{"desktop|evil", "desktop\x00evil", "desktop\nevil"} {
		_, created, err := orchestrator.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
			Photo:      orchestratorPhotoRequest(),
			SourceKind: sourceKind,
			SourceKey:  "reserved-kind-must-not-persist",
		})
		if !errors.Is(err, ErrInvalidInput) || created {
			t.Fatalf("reserved source kind %q: created=%v err=%v", sourceKind, created, err)
		}
	}
	if resolverCalls != 0 {
		t.Fatalf("reserved source kind reached mutable model resolver: calls=%d", resolverCalls)
	}
	jobs, err := d.ListGradingJobs(ctx, "mingming", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("reserved source kind persisted %d grading jobs", len(jobs))
	}
}
