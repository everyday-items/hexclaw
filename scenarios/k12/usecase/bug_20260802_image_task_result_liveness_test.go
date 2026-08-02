package usecase

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// blockingImageTaskPhotoResultGrading models the real coordinator seam that
// holds its per-Job lock while a provider is in flight. Result must not touch
// this optional process-local reader until a durable final artifact exists.
type blockingImageTaskPhotoResultGrading struct {
	imageTaskGradingStub

	mu               sync.Mutex
	photoResultCalls int
	release          <-chan struct{}
}

func (s *blockingImageTaskPhotoResultGrading) ImageTaskHomeworkProjection(
	context.Context,
	string,
	string,
) (ImageTaskHomeworkProjection, error) {
	return ImageTaskHomeworkProjection{
		Stage: k12.GradingStageAssessing,
	}, nil
}

func (s *blockingImageTaskPhotoResultGrading) PhotoResult(string) (PhotoGradeResult, bool) {
	s.mu.Lock()
	s.photoResultCalls++
	s.mu.Unlock()
	<-s.release
	return PhotoGradeResult{Markdown: "process-local photo result"}, true
}

func (s *blockingImageTaskPhotoResultGrading) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.photoResultCalls
}

// K12-IMAGE-RESULT-LIVENESS-001: while the real provider holds a Job lock,
// ImageTask Result must project the durable non-terminal snapshot immediately.
// It must not wait for PhotoResult, whose in-memory value is not authorized
// before a durable GradingFinalArtifact exists.
func TestBUG20260802ImageTaskResultDoesNotWaitForInFlightPhotoResult(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	release := make(chan struct{})
	grading := &blockingImageTaskPhotoResultGrading{
		imageTaskGradingStub: imageTaskGradingStub{jobID: "grading-result-liveness"},
		release:              release,
	}
	coordinator.Grading = grading
	view, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !created {
		t.Fatalf("create/run image task: created=%v err=%v", created, err)
	}

	type resultCall struct {
		result ImageTaskResult
		err    error
	}
	done := make(chan resultCall, 1)
	go func() {
		result, resultErr := coordinator.Result(context.Background(), "mingming", view.Dispatch.DispatchID)
		done <- resultCall{result: result, err: resultErr}
	}()

	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Result: %v", got.err)
		}
		if got.result.Photo != nil {
			t.Fatalf("non-terminal result must not expose process-local photo: %+v", got.result.Photo)
		}
		if grading.calls() != 0 {
			t.Fatalf("non-terminal Result must not call PhotoResult: calls=%d", grading.calls())
		}
	case <-time.After(200 * time.Millisecond):
		close(release)
		released = true
		<-done
		t.Fatal("Result waited for the in-flight PhotoResult instead of returning a durable snapshot")
	}
}

// K12-IMAGE-RESULT-LIVENESS-001: the durable final artifact is the explicit
// boundary that authorizes the optional process-local projection. This
// preserves the completed result contract while the non-terminal path remains
// lock-free.
func TestBUG20260802ImageTaskResultReadsPhotoOnlyAfterDurableFinalArtifact(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	release := make(chan struct{})
	close(release)
	grading := &blockingImageTaskPhotoResultGrading{
		imageTaskGradingStub: imageTaskGradingStub{jobID: "grading-final-result-liveness"},
		release:              release,
	}
	coordinator.Grading = grading
	feedbackDeps := coordinator.WorkFeedback.(*Deps)
	persistedJob, createdJob, err := feedbackDeps.CreateGradingJob(
		context.Background(), "mingming", "session-final-result-liveness",
		CreateGradingJobInput{
			SubmissionID: "submission-final-result-liveness",
			SourceKind:   "test",
			SourceKey:    "image-task-final-result-liveness",
			ModelSnapshot: k12.GradingModelSnapshot{
				Provider:                 "hexclaw-gpt",
				Model:                    k12.RecognizingPolicyModel,
				Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
				Capability:               "vision",
				RecognizingRequestPolicy: k12.ApprovedRecognizingRequestPolicy(),
			},
		},
	)
	if err != nil || !createdJob {
		t.Fatalf("persist grading job fixture: created=%v err=%v", createdJob, err)
	}
	grading.jobID = persistedJob.Record.RecordID
	view, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !created {
		t.Fatalf("create/run image task: created=%v err=%v", created, err)
	}

	_, replay, err := coordinator.Records.CommitGradingFinalArtifact(
		context.Background(),
		k12.GradingFinalArtifact{
			ArtifactID:                "artifact-result-liveness",
			AgentName:                 "mingming",
			JobID:                     grading.resolvedJobID(),
			StructureVersion:          k12.GradingFinalArtifactStructureVersion,
			CoverageStatus:            k12.GradingFinalArtifactCoverageComplete,
			TotalCount:                1,
			PublishedCount:            1,
			OrderedCurrentDigestsJSON: `["receipt-result-liveness"]`,
			CanonicalMarkdown:         "# final grading result",
			ArtifactDigest:            strings.Repeat("a", 64),
			SummaryInvocationID:       "summary-result-liveness",
			CreatedAt:                 1000,
			UpdatedAt:                 1000,
		},
	)
	if err != nil || replay {
		t.Fatalf("commit durable final artifact: replay=%v err=%v", replay, err)
	}

	got, err := coordinator.Result(context.Background(), "mingming", view.Dispatch.DispatchID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got.FinalArtifact == nil || got.Photo == nil || got.Photo.Markdown != "process-local photo result" {
		t.Fatalf("durable final artifact must authorize the completed projection: %+v", got)
	}
	if grading.calls() != 1 {
		t.Fatalf("completed Result must read PhotoResult exactly once: calls=%d", grading.calls())
	}
}
