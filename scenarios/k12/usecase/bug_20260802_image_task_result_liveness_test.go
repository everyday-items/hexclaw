package usecase

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// blockingImageTaskPhotoResultGrading 模拟真实协调器接缝：Provider 执行期间持有
// 每个 Job 的锁，Result 不得访问这个可选的进程内读取器。
type blockingImageTaskPhotoResultGrading struct {
	imageTaskGradingStub

	mu               sync.Mutex
	photoResultCalls int
	release          <-chan struct{}
	projection       ImageTaskHomeworkProjection
}

func (s *blockingImageTaskPhotoResultGrading) ImageTaskHomeworkProjection(
	context.Context,
	string,
	string,
) (ImageTaskHomeworkProjection, error) {
	if s.projection.Stage != "" {
		return s.projection, nil
	}
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

// K12-IMAGE-RESULT-LIVENESS-001：完成态结果只读取最终产物冻结的不可变图片和
// Markdown；即使任务完成，公开读取路径也不得访问进程内 PhotoResult。
func TestBUG20260802ImageTaskResultReadsOnlyDurableFinalArtifact(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
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
	if err := coordinator.Records.PutProblemAttemptSnapshot(
		context.Background(),
		k12.ProblemAttemptSnapshot{
			Problems: []k12.Problem{{
				ProblemID: "problem-final-result-liveness", AgentName: "mingming",
				SubmissionID: persistedJob.Fields.SubmissionID, PageAssetID: "page-final-result-liveness",
				Ordinal: 1, ProblemKind: k12.ProblemKindStandalone,
				StemRaw: "1+1=", StemMarkdown: "1+1=", CanonicalVersion: 1,
				CreatedAt: 1000, UpdatedAt: 1000,
			}},
			Attempts: []k12.Attempt{{
				AttemptID: "attempt-final-result-liveness", AgentName: "mingming",
				SubmissionID: persistedJob.Fields.SubmissionID, ProblemID: "problem-final-result-liveness",
				AnswerState: "present", AnswerRaw: "2", AnswerMarkdown: "2",
				ConfirmedVersion: 1, InputDigest: "sha256:confirmed-input",
				CreatedAt: 1000, UpdatedAt: 1000,
			}},
		},
	); err != nil {
		t.Fatalf("persist problem/attempt fixture: %v", err)
	}
	itemInvocation, itemCreated, err := coordinator.Records.PrepareGradingItemInvocation(
		context.Background(),
		k12.GradingItemInvocation{
			InvocationID: "grade-final-result-liveness", AgentName: "mingming",
			JobID: grading.resolvedJobID(), ProblemID: "problem-final-result-liveness",
			AttemptID: "attempt-final-result-liveness", Operation: k12.GradingItemOperationGrade,
			OperationAttempt: 1, RequestDigest: "sha256:grade-request",
			InputRevision: 1, InputDigest: "sha256:confirmed-input",
			RouteSnapshot: persistedJob.Fields.ModelSnapshot, CreatedAt: 1000,
		},
	)
	if err != nil || !itemCreated {
		t.Fatalf("prepare grading item invocation: created=%v err=%v", itemCreated, err)
	}
	itemInvocation, err = coordinator.Records.MarkGradingItemInvocationSent(
		context.Background(), itemInvocation.AgentName, itemInvocation.InvocationID,
	)
	if err != nil {
		t.Fatalf("mark grading item invocation sent: %v", err)
	}
	itemInvocation, err = coordinator.Records.MarkGradingItemInvocationSucceeded(
		context.Background(), itemInvocation.AgentName, itemInvocation.InvocationID,
		"sha256:grade-result", `{"verdict":"correct"}`,
	)
	if err != nil {
		t.Fatalf("complete grading item invocation: %v", err)
	}
	repository := &PageAssetRepository{Records: coordinator.Records}
	annotatedBytes := validPNGFixture(t, "image-task-final-result-liveness")
	annotated, err := repository.Persist(
		context.Background(), "guardian-result-liveness", "mingming", annotatedBytes,
	)
	if err != nil {
		t.Fatalf("persist durable annotated PageAsset: %v", err)
	}

	artifact := k12.GradingFinalArtifact{
		AgentName: "mingming", JobID: grading.resolvedJobID(),
		StructureVersion: k12.GradingFinalArtifactStructureVersion,
		CoverageStatus:   k12.GradingFinalArtifactCoverageComplete,
		TotalCount:       1, PublishedCount: 1,
		OrderedCurrentDigestsJSON: `["receipt-result-liveness"]`,
		CanonicalMarkdown:         "# durable final grading result",
		SummaryInvocationID:       "summary-result-liveness",
		AnnotatedAssetOwnerScope:  "guardian-result-liveness",
		AnnotatedAssetID:          annotated.Metadata.PageAssetID,
		AnnotatedMIME:             annotated.Metadata.MediaType,
		AnnotatedDigest:           annotated.Metadata.ContentDigest,
		OriginalSourceDigest:      annotated.Metadata.ContentDigest,
		CreatedAt:                 1000,
		UpdatedAt:                 1000,
	}
	artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
	_, replay, err := coordinator.Records.CommitGradingFinalArtifact(
		context.Background(),
		artifact,
		0,
	)
	if err != nil || replay {
		t.Fatalf("commit durable final artifact: replay=%v err=%v", replay, err)
	}
	wantGroundingReceipts := []GroundingEvidenceReceipt{groundingRecoveryReceipt()}
	grading.projection = ImageTaskHomeworkProjection{
		Stage:                     k12.GradingStageCompleted,
		GroundingEvidenceReceipts: wantGroundingReceipts,
	}

	got, err := coordinator.Result(context.Background(), "mingming", view.Dispatch.DispatchID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got.FinalArtifact == nil || got.Photo == nil ||
		got.Photo.Markdown != artifact.CanonicalMarkdown ||
		got.Photo.AnnotatedImage == nil ||
		got.Photo.AnnotatedImage.MIME != annotated.Metadata.MediaType ||
		!bytes.Equal(got.Photo.AnnotatedImage.Data, annotatedBytes) {
		t.Fatalf("completed result did not read the durable final artifact: %+v", got)
	}
	if grading.calls() != 0 {
		t.Fatalf("completed Result must not read process-local PhotoResult: calls=%d", grading.calls())
	}
	if !reflect.DeepEqual(got.GroundingEvidenceReceipts, wantGroundingReceipts) {
		t.Fatalf("completed Result grounding receipts=%+v want %+v",
			got.GroundingEvidenceReceipts, wantGroundingReceipts)
	}
	var gradeReceipt, annotationReceipt *ImageTaskOperationReceipt
	for i := range got.OperationReceipts {
		switch got.OperationReceipts[i].Operation {
		case string(k12.GradingItemOperationGrade):
			gradeReceipt = &got.OperationReceipts[i]
		case "annotation":
			annotationReceipt = &got.OperationReceipts[i]
		}
	}
	if gradeReceipt == nil || gradeReceipt.InvocationID != itemInvocation.InvocationID ||
		gradeReceipt.CanonicalInputDigest != view.Dispatch.SourceDigest ||
		gradeReceipt.ResultDigest != itemInvocation.ResultDigest {
		t.Fatalf("grade receipt missing or canonical digest drifted: %+v", got.OperationReceipts)
	}
	if annotationReceipt == nil ||
		annotationReceipt.CanonicalInputDigest != view.Dispatch.SourceDigest ||
		annotationReceipt.ResultDigest != "sha256:"+artifact.AnnotatedDigest {
		t.Fatalf("annotation receipt missing or canonical digest drifted: %+v", got.OperationReceipts)
	}
	if removed, removeErr := assetstore.Remove(
		"mingming", artifact.AnnotatedAssetID,
	); removeErr != nil || !removed {
		t.Fatalf("remove durable annotated bytes: removed=%v err=%v", removed, removeErr)
	}
	if _, err := coordinator.Result(
		context.Background(), "mingming", view.Dispatch.DispatchID,
	); !errors.Is(err, k12storage.ErrGradingFinalAnnotatedAssetUnavailable) {
		t.Fatalf("completed Result must fail closed when annotated bytes are missing: %v", err)
	}
}
