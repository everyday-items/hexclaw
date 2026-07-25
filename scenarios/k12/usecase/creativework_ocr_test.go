package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

type writingOCRResult struct {
	raw string
	err error
}

type fakeWritingOCR struct {
	results []writingOCRResult
	calls   int
}

func (f *fakeWritingOCR) RecognizeWriting(_ context.Context, image []byte) (string, error) {
	f.calls++
	if len(image) == 0 {
		return "", errors.New("missing image")
	}
	idx := f.calls - 1
	if idx >= len(f.results) {
		idx = len(f.results) - 1
	}
	return f.results[idx].raw, f.results[idx].err
}

func creativeWorkOCRAsset(t *testing.T, agent string) string {
	t.Helper()
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	// 1x1 PNG; assetstore validates the real image magic before OCR can run.
	raw := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde,
	}
	id, err := assetstore.Save(agent, raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCreativeWorkOCRLifecyclePersistsRawAndVersionsCanonicalConfirmation(t *testing.T) {
	d := newDataDeps(t, "xiaoming", "xiaohong")
	d.CreativeWorkOCR = &fakeWritingOCR{results: []writingOCRResult{{raw: "春天的校园\n柳枝象绿色丝带。"}}}
	assetID := creativeWorkOCRAsset(t, "xiaoming")
	ctx := context.Background()

	job, err := d.CreateCreativeWorkOCR(ctx, "xiaoming", assetID, "add-work-1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != k12.CreativeWorkOCRAwaitingConfirmation || !strings.Contains(job.OCRRaw, "柳枝象") {
		t.Fatalf("OCR should persist awaiting-confirmation raw evidence: %#v", job)
	}
	if _, err := d.GetCreativeWorkOCR(ctx, "xiaohong", job.JobID); err == nil {
		t.Fatal("another Tutor must not read this OCR job")
	}

	confirmedV1, err := d.ConfirmCreativeWorkOCR(ctx, "xiaoming", job.JobID, "春天的校园\n柳枝像绿色丝带。")
	if err != nil {
		t.Fatal(err)
	}
	if confirmedV1.Status != k12.CreativeWorkOCRConfirmed || confirmedV1.ConfirmedVersion != 1 || confirmedV1.ConfirmedDigest == "" {
		t.Fatalf("first confirmation should freeze v1 and digest: %#v", confirmedV1)
	}
	confirmedV2, err := d.ConfirmCreativeWorkOCR(ctx, "xiaoming", job.JobID, "春天的校园\n柳枝像一条绿色丝带。")
	if err != nil {
		t.Fatal(err)
	}
	if confirmedV2.ConfirmedVersion != 2 || confirmedV2.ConfirmedDigest == confirmedV1.ConfirmedDigest {
		t.Fatalf("edited canonical text needs a new version/digest: v1=%#v v2=%#v", confirmedV1, confirmedV2)
	}
	if confirmedV2.OCRRaw != job.OCRRaw {
		t.Fatalf("parent correction must never overwrite raw OCR: got %q want %q", confirmedV2.OCRRaw, job.OCRRaw)
	}

	// Same request id is idempotent and must not invoke the model again.
	again, err := d.CreateCreativeWorkOCR(ctx, "xiaoming", assetID, "add-work-1")
	if err != nil || again.JobID != job.JobID {
		t.Fatalf("idempotent create returned %#v err=%v", again, err)
	}
	if calls := d.CreativeWorkOCR.(*fakeWritingOCR).calls; calls != 1 {
		t.Fatalf("idempotent create must not repeat OCR, calls=%d", calls)
	}
}

func TestCreativeWorkOCRFailureSupportsSameJobRetryAndManualPaste(t *testing.T) {
	d := newDataDeps(t)
	ocr := &fakeWritingOCR{results: []writingOCRResult{
		{err: errors.New("vision timeout")},
		{raw: "重试识别成功"},
		{err: errors.New("still unavailable")},
	}}
	d.CreativeWorkOCR = ocr
	assetID := creativeWorkOCRAsset(t, "xiaoming")
	ctx := context.Background()

	failed, err := d.CreateCreativeWorkOCR(ctx, "xiaoming", assetID, "retry-same-job")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != k12.CreativeWorkOCRFailed || failed.AttemptCount != 1 {
		t.Fatalf("failed OCR must be a durable retryable state: %#v", failed)
	}
	retried, err := d.RetryCreativeWorkOCR(ctx, "xiaoming", failed.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.JobID != failed.JobID || retried.Status != k12.CreativeWorkOCRAwaitingConfirmation || retried.AttemptCount != 2 {
		t.Fatalf("retry must reuse the original job: %#v", retried)
	}

	failedManual, err := d.CreateCreativeWorkOCR(ctx, "xiaoming", assetID, "manual-paste")
	if err != nil || failedManual.Status != k12.CreativeWorkOCRFailed {
		t.Fatalf("second job should fail for manual-paste path: %#v err=%v", failedManual, err)
	}
	manual, err := d.ConfirmCreativeWorkOCR(ctx, "xiaoming", failedManual.JobID, "家长从原稿手工粘贴的正文")
	if err != nil {
		t.Fatal(err)
	}
	if manual.Status != k12.CreativeWorkOCRConfirmed || manual.OCRRaw != "" || manual.ConfirmedDigest == "" {
		t.Fatalf("manual paste must create a confirmed canonical version without inventing OCR raw: %#v", manual)
	}
}

func TestWritingPhotoRequiresCurrentConfirmedOCRBeforeSaveAndFeedbackEvidenceUsesIt(t *testing.T) {
	d := newDataDeps(t)
	ocr := &fakeWritingOCR{results: []writingOCRResult{{raw: "柳枝象绿色丝带"}}}
	d.CreativeWorkOCR = ocr
	assetID := creativeWorkOCRAsset(t, "xiaoming")
	ctx := context.Background()
	job, err := d.CreateCreativeWorkOCR(ctx, "xiaoming", assetID, "save-gate")
	if err != nil {
		t.Fatal(err)
	}

	base := k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting,
		Title:    "《春天的校园》",
		Task:     "观察春景",
		Versions: []k12.CreativeWorkVersion{{
			SourceAssetID:   assetID,
			ContentMarkdown: "柳枝像绿色丝带",
			OCRJobID:        job.JobID,
		}},
	}
	if _, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", base); err == nil {
		t.Fatal("an unconfirmed OCR job must not be saved as a writing version")
	}
	confirmed, err := d.ConfirmCreativeWorkOCR(ctx, "xiaoming", job.JobID, "柳枝像绿色丝带")
	if err != nil {
		t.Fatal(err)
	}
	base.Versions[0].OCRRaw = confirmed.OCRRaw
	base.Versions[0].OCRVersion = confirmed.ConfirmedVersion
	base.Versions[0].OCRConfirmedDigest = confirmed.ConfirmedDigest
	base.Versions[0].ContentConfirmedAt = confirmed.ConfirmedAt
	id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", base)
	if err != nil {
		t.Fatalf("confirmed OCR snapshot should save: %v", err)
	}

	gen := &fakeWorkFeedbackSolver{feedback: "这句话的比喻很清楚；建议补充柳枝随风移动的细节。"}
	d.Solver = gen
	view, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	feedback := view.Fields.Versions[0].StructuredFeedback
	if feedback == nil {
		t.Fatal("expected structured feedback")
	}
	wantPrefix := "ocr-confirmed:" + job.JobID + ":v1:sha256:"
	found := false
	for _, ref := range feedback.EvidenceRefs {
		found = found || strings.HasPrefix(ref, wantPrefix)
	}
	if !found {
		t.Fatalf("feedback evidence must point at the confirmed OCR version, refs=%v", feedback.EvidenceRefs)
	}
}

func TestWritingPhotoCannotReuseAnOlderConfirmedDigest(t *testing.T) {
	d := newDataDeps(t)
	d.CreativeWorkOCR = &fakeWritingOCR{results: []writingOCRResult{{raw: "第一版 OCR 原文"}}}
	assetID := creativeWorkOCRAsset(t, "xiaoming")
	ctx := context.Background()
	job, err := d.CreateCreativeWorkOCR(ctx, "xiaoming", assetID, "stale-digest")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := d.ConfirmCreativeWorkOCR(ctx, "xiaoming", job.JobID, "家长确认第一版")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConfirmCreativeWorkOCR(ctx, "xiaoming", job.JobID, "家长确认第二版"); err != nil {
		t.Fatal(err)
	}
	_, _, err = d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting,
		Title:    "旧摘要不得复用",
		Task:     "写景",
		Versions: []k12.CreativeWorkVersion{{
			SourceAssetID:      assetID,
			ContentMarkdown:    v1.ConfirmedContent,
			OCRJobID:           v1.JobID,
			OCRVersion:         v1.ConfirmedVersion,
			OCRConfirmedDigest: v1.ConfirmedDigest,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("older confirmation digest must be stale after v2, err=%v", err)
	}
}

func TestGenerateWritingFeedbackRejectsPhotoVersionWithoutConfirmedOCREvidence(t *testing.T) {
	d := newDataDeps(t)
	gen := &fakeWorkFeedbackSolver{feedback: "观察具体；建议补充一个细节。"}
	d.Solver = gen
	rec, err := k12.NewCreativeWorkRecord("xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting,
		Title:    "旁路旧数据",
		Task:     "写景",
		Versions: []k12.CreativeWorkVersion{{
			SourceAssetID:   "legacy-photo-path",
			ContentMarkdown: "未经家长确认的 OCR 文本",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Records.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GenerateWorkFeedback(context.Background(), "xiaoming", rec.RecordID); err == nil {
		t.Fatal("a writing photo without confirmed OCR evidence must not enter feedback generation")
	}
	if gen.calls != 0 {
		t.Fatalf("confirmation gate must run before the model, calls=%d", gen.calls)
	}
}

func TestWritingPhotoRevisionAlsoRequiresConfirmedOCRSnapshot(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	workID := newWritingWork(t, d, "xiaoming")
	generateCreativeWorkFeedbackForTest(t, &d, workID, "原稿切题；建议修改一处细节。")
	assetID := creativeWorkOCRAsset(t, "xiaoming")
	if _, err := d.SubmitRevision(ctx, "xiaoming", workID, "修改稿", assetID); err == nil {
		t.Fatal("writing photo revision must not bypass OCR confirmation")
	}
	d.CreativeWorkOCR = &fakeWritingOCR{results: []writingOCRResult{{raw: "修改稿 OCR 原文"}}}
	job, err := d.CreateCreativeWorkOCR(ctx, "xiaoming", assetID, "revision-ocr")
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := d.ConfirmCreativeWorkOCR(ctx, "xiaoming", job.JobID, "家长修正后的修改稿")
	if err != nil {
		t.Fatal(err)
	}
	view, err := d.SubmitRevisionWithOCR(ctx, "xiaoming", workID, k12.CreativeWorkVersion{
		SourceAssetID:      assetID,
		ContentMarkdown:    confirmed.ConfirmedContent,
		OCRJobID:           confirmed.JobID,
		OCRVersion:         confirmed.ConfirmedVersion,
		OCRConfirmedDigest: confirmed.ConfirmedDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	last := view.Fields.Versions[len(view.Fields.Versions)-1]
	if last.OCRRaw != "修改稿 OCR 原文" || last.ContentMarkdown != "家长修正后的修改稿" {
		t.Fatalf("revision snapshot not persisted: %#v", last)
	}
}
