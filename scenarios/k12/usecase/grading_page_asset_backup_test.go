package usecase

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

type freshSharedPageRecognizer struct{}

func (freshSharedPageRecognizer) Recognize(
	context.Context,
	[]byte,
) ([]RecognizedQuestion, error) {
	return []RecognizedQuestion{{
		RawTranscription: "7+8=?", CanonicalMarkdown: "7+8=?",
		AnswerState: AnswerStateBlank, Subject: "数学",
	}}, nil
}

func TestPhotoGradingPersistsOwnerScopedPageAssetAndBackupExactSet(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	d, store := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	d.PageAssets = assetstore.PageStore{}
	d.Recognizer = &countingRecognizer{questions: []RecognizedQuestion{{
		RawTranscription: "8÷2=?", CanonicalMarkdown: "8\\div2=?",
		AnswerState: AnswerStateBlank, Subject: "数学",
	}}}
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, orchestratorSnapshotResolver))
	ctx := context.Background()
	image := []byte("\x89PNG\r\n\x1a\ncanonical-homework-page")

	var firstAssetID string
	for i, owner := range []string{"mingming", "mingming", "eval-agent"} {
		job, _, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
			Photo:      PhotoGradeRequest{AgentName: owner, Grade: "五年级上", SourceSession: fmt.Sprintf("session-%d", i), Image: image},
			SourceKind: "desktop", SourceKey: fmt.Sprintf("page-asset-%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.RunGradingJob(ctx, job.Record.RecordID); err != nil {
			t.Fatal(err)
		}
		typed, err := store.GetProblemAttemptSnapshot(ctx, owner, job.Fields.SubmissionID)
		if err != nil || len(typed.Problems) != 1 {
			t.Fatalf("owner=%s typed=%+v err=%v", owner, typed, err)
		}
		assetID := typed.Problems[0].PageAssetID
		assetOwner, ok := assetstore.OwnerOf(assetID)
		if !ok || assetOwner != owner {
			t.Fatalf("owner=%s page asset=%q parsed owner=%q", owner, assetID, assetOwner)
		}
		if i == 0 {
			firstAssetID = assetID
		} else if owner == "mingming" && assetID != firstAssetID {
			t.Fatalf("same owner/content must reuse one content ID: first=%q retry=%q", firstAssetID, assetID)
		} else if owner != "mingming" && assetID == firstAssetID {
			t.Fatalf("same content must remain isolated across owners: %q", assetID)
		}
	}

	bak, err := d.Backup(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(bak.ProblemAttempts) != 2 || len(bak.Assets) != 1 ||
		bak.ProblemAttempts[0].Problems[0].PageAssetID != bak.Assets[0].AssetID ||
		bak.ProblemAttempts[1].Problems[0].PageAssetID != bak.Assets[0].AssetID ||
		bak.ProblemAttempts[0].Problems[0].SubmissionID ==
			bak.ProblemAttempts[1].Problems[0].SubmissionID ||
		bak.Assets[0].AssetID != firstAssetID {
		t.Fatalf("backup page exact-set invalid: problems=%+v assets=%+v", bak.ProblemAttempts, bak.Assets)
	}
	if err := VerifyHexbak(bak); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.pageAssetLocks) != 0 {
		t.Fatalf("completed page-asset commands leaked keyed locks: %d", len(o.pageAssetLocks))
	}
}

func TestPhotoGradingPageAssetFailureAndProblemWriteFailureNeverReportSuccess(t *testing.T) {
	t.Run("asset write fails", func(t *testing.T) {
		d, store := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
		d.PageAssets = failingPageStore{}
		d.Recognizer = &countingRecognizer{questions: []RecognizedQuestion{{
			RawTranscription: "2+2=?", CanonicalMarkdown: "2+2=?", AnswerState: AnswerStateBlank, Subject: "数学",
		}}}
		o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, orchestratorSnapshotResolver))
		job, _, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
			Photo:      PhotoGradeRequest{AgentName: "mingming", Grade: "五年级上", Image: []byte("image")},
			SourceKind: "desktop", SourceKey: "asset-write-fails",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.RunGradingJob(context.Background(), job.Record.RecordID); err == nil {
			t.Fatal("page asset failure must surface")
		}
		if _, err := store.GetProblemAttemptSnapshot(context.Background(), "mingming", job.Fields.SubmissionID); !errors.Is(err, records.ErrNotFound) {
			t.Fatalf("asset failure leaked typed success: %v", err)
		}
	})

	t.Run("typed write fails and new blob is compensated", func(t *testing.T) {
		t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
		d, store := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
		d.PageAssets = assetstore.PageStore{}
		d.Recognizer = &countingRecognizer{questions: []RecognizedQuestion{{
			RawTranscription: "3+3=?", CanonicalMarkdown: "3+3=?", AnswerState: AnswerStateBlank, Subject: "数学",
		}}}
		if _, err := store.DB().Exec(`CREATE TRIGGER reject_canonical_page_problem
			BEFORE INSERT ON k12_problems
			BEGIN SELECT RAISE(ABORT, 'injected problem write failure'); END`); err != nil {
			t.Fatal(err)
		}
		o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, orchestratorSnapshotResolver))
		image := []byte("\x89PNG\r\n\x1a\ncompensated-page")
		assetID, _, _, err := assetstore.Describe("mingming", image)
		if err != nil {
			t.Fatal(err)
		}
		job, _, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
			Photo:      PhotoGradeRequest{AgentName: "mingming", Grade: "五年级上", Image: image},
			SourceKind: "desktop", SourceKey: "typed-write-fails",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.RunGradingJob(context.Background(), job.Record.RecordID); err == nil {
			t.Fatal("typed write failure must surface")
		}
		if _, err := assetstore.PathFromID(assetID); err == nil {
			t.Fatalf("failed typed write left unreferenced new page blob %q", assetID)
		}
		if _, err := store.GetProblemAttemptSnapshot(context.Background(), "mingming", job.Fields.SubmissionID); !errors.Is(err, records.ErrNotFound) {
			t.Fatalf("typed failure leaked facts: %v", err)
		}
	})
}

func TestPhotoGradingSameImageConcurrentTypedFailureCannotDeleteSuccessfulSharedAsset(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	d, store := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	d.PageAssets = assetstore.PageStore{}
	d.Recognizer = freshSharedPageRecognizer{}
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, orchestratorSnapshotResolver))
	ctx := context.Background()
	image := []byte("\x89PNG\r\n\x1a\nconcurrent-shared-page")

	start := func(sourceKey string) GradingJobView {
		t.Helper()
		job, created, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
			Photo: PhotoGradeRequest{
				AgentName: "mingming", Grade: "五年级上",
				SourceSession: sourceKey, Image: image,
			},
			SourceKind: "desktop",
			SourceKey:  sourceKey,
		})
		if err != nil || !created {
			t.Fatalf("start %s: created=%v err=%v", sourceKey, created, err)
		}
		return job
	}
	failingJob := start("concurrent-failing-source")
	successfulJob := start("concurrent-successful-source")

	if _, err := store.DB().Exec(fmt.Sprintf(`CREATE TRIGGER reject_one_submission_problem
		BEFORE INSERT ON k12_problems
		WHEN NEW.submission_id = '%s'
		BEGIN SELECT RAISE(ABORT, 'injected one-submission failure'); END`,
		failingJob.Fields.SubmissionID,
	)); err != nil {
		t.Fatal(err)
	}

	type runResult struct {
		jobID string
		err   error
	}
	results := make(chan runResult, 2)
	for _, job := range []GradingJobView{failingJob, successfulJob} {
		job := job
		go func() {
			_, err := o.RunGradingJob(ctx, job.Record.RecordID)
			results <- runResult{jobID: job.Record.RecordID, err: err}
		}()
	}
	got := map[string]error{}
	for range 2 {
		result := <-results
		got[result.jobID] = result.err
	}
	if got[failingJob.Record.RecordID] == nil {
		t.Fatal("injected typed failure unexpectedly succeeded")
	}
	if got[successfulJob.Record.RecordID] != nil {
		t.Fatalf("independent same-image submission failed: %v", got[successfulJob.Record.RecordID])
	}
	if _, err := store.GetProblemAttemptSnapshot(
		ctx,
		"mingming",
		failingJob.Fields.SubmissionID,
	); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("failed submission leaked typed facts: %v", err)
	}
	successFacts, err := store.GetProblemAttemptSnapshot(
		ctx,
		"mingming",
		successfulJob.Fields.SubmissionID,
	)
	if err != nil || len(successFacts.Problems) != 1 {
		t.Fatalf("successful submission facts missing: %+v err=%v", successFacts, err)
	}
	assetID := successFacts.Problems[0].PageAssetID
	if _, err := assetstore.PathFromID(assetID); err != nil {
		t.Fatalf("failed submission compensation deleted successful shared asset %q: %v", assetID, err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.pageAssetLocks) != 0 {
		t.Fatalf("concurrent page-asset commands leaked keyed locks: %d", len(o.pageAssetLocks))
	}
}

func TestHexbakV5AllowsSignedVirtualPageIdentityWithoutFabricatingBlob(t *testing.T) {
	bak := &Hexbak{
		Version: HexbakVersion, AgentName: "mingming", ExportedAt: 100,
		ProblemAttempts: []k12.ProblemAttemptSnapshot{
			v5ProblemAttemptSnapshot("mingming", "webhook-receipt:receipt-1", "page-0123456789abcdefabcd"),
		},
	}
	if err := SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	if err := VerifyHexbak(bak); err != nil {
		t.Fatalf("trusted text/legacy virtual page must remain restorable without a fake blob: %v", err)
	}
	if len(bak.Assets) != 0 {
		t.Fatalf("virtual page fabricated archive bytes: %+v", bak.Assets)
	}
	migrated, err := MigrateHexbakOwner(bak, "target-child")
	if err != nil {
		t.Fatalf("virtual page restore-as migration: %v", err)
	}
	if migrated.ProblemAttempts[0].Problems[0].PageAssetID != "page-0123456789abcdefabcd" ||
		migrated.ProblemAttempts[0].Problems[0].AgentName != "target-child" || len(migrated.Assets) != 0 {
		t.Fatalf("virtual page migration fabricated/rewrote Blob identity: %+v", migrated)
	}
	if err := VerifyHexbak(migrated); err != nil {
		t.Fatal(err)
	}
}

type failingPageStore struct{}

func (failingPageStore) Ensure(string, []byte) (string, bool, error) {
	return "", false, errors.New("injected page asset failure")
}

func (failingPageStore) Remove(string, string) (bool, error) { return false, nil }
