package usecase

import (
	"bytes"
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestGradingFinalizerFreezesAnnotatedPageAssetBeforeFinalArtifactCommit(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	ctx := context.Background()
	fixture := prepareFinalSummaryCrashFixture(t)
	repository := &PageAssetRepository{Records: fixture.orchestrator.deps.Records}
	original := validPNGFixture(t, "final-artifact-original")
	annotated := validPNGFixture(t, "final-artifact-annotated")
	originalReady, err := repository.Persist(
		ctx, "guardian-annotated", "mingming", original,
	)
	if err != nil {
		t.Fatalf("persist original PageAsset: %v", err)
	}

	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible answers"},
		Confidence:     1,
	}}
	coordinator := &ImageTaskCoordinator{
		Records: fixture.orchestrator.deps.Records, PageAssets: repository,
		Classifier: classifier, ResolveRoute: imageTaskRouteForTest,
		Now: func() int64 { return 1_000 },
		NewID: func(kind string) string {
			if kind == "dispatch" {
				return "dispatch-final-annotated"
			}
			return "classification-final-annotated"
		},
	}
	input := testCreateImageTaskInput()
	input.OwnerScope = "guardian-annotated"
	input.SourceRef = "message-final-annotated"
	input.SourceAssetRefs = []string{originalReady.Metadata.PageAssetID}
	prepared, created, err := coordinator.Create(ctx, input)
	if err != nil || !created {
		t.Fatalf("prepare source dispatch: created=%v err=%v", created, err)
	}

	fixture.job.Fields.SourceKind = "image_task"
	fixture.job.Fields.IdempotencyKey = k12.BuildGradingIdempotencyKey(
		"image_task", prepared.Dispatch.DispatchID, fixture.job.Fields.ConfirmedVersion,
	)
	fixture.run.result = &PhotoGradeResult{
		TaskIntent:     PhotoTaskCompletedHomework,
		ResultSurface:  PhotoSurfaceAnnotatedHomework,
		AnnotatedImage: &RenderedPhoto{Data: annotated, MIME: "image/png"},
	}
	if _, err := fixture.orchestrator.deps.Records.MarkModelInvocationSucceededWithResult(
		ctx,
		fixture.invocation.AgentName,
		fixture.invocation.InvocationID,
		modelInvocationResultDigest(fixture.tips),
		fixture.resultJSON,
		"",
	); err != nil {
		t.Fatalf("persist summary result: %v", err)
	}

	artifact, err := fixture.orchestrator.finalizeGradingPage(
		ctx, fixture.run, fixture.job,
	)
	if err != nil {
		t.Fatalf("finalize annotated grading artifact: %v", err)
	}
	if !artifact.HasAnnotatedAsset() ||
		artifact.AnnotatedAssetOwnerScope != input.OwnerScope ||
		artifact.OriginalSourceDigest != originalReady.Metadata.ContentDigest ||
		artifact.ArtifactDigest != k12.ComputeGradingFinalArtifactDigest(artifact) {
		t.Fatalf("final artifact did not freeze annotated identity: %+v", artifact)
	}
	opened, err := fixture.orchestrator.deps.Records.OpenGradingFinalAnnotatedAsset(
		ctx, artifact.AgentName, artifact.ArtifactID,
	)
	if err != nil || opened.MIME != "image/png" || !bytes.Equal(opened.Data, annotated) {
		t.Fatalf("open frozen annotated bytes: opened=%+v err=%v", opened, err)
	}

	fixture.run.result = nil
	replayed, err := fixture.restartedFinalizer().finalizeGradingPage(
		ctx, fixture.run, fixture.job,
	)
	if err != nil || replayed.ArtifactID != artifact.ArtifactID ||
		replayed.AnnotatedAssetID != artifact.AnnotatedAssetID {
		t.Fatalf("restart replay must not depend on PhotoResult: replayed=%+v err=%v", replayed, err)
	}
}
