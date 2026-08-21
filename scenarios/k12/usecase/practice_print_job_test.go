package usecase_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// DD-004/DD-023：Desktop 打印是 prepare → 原生对话框 → receipt commit 两阶段协议。
// prepare 必须预占稳定 paper_no 和冻结同源卷面，但绝不能提前固化/清空待打印篮。
func TestPracticePrintPrepareReservesFormalPaperWithoutFinalizing(t *testing.T) {
	d := newDataDeps(t)
	d.Renderer = &v45PDFRenderer{data: []byte("%PDF-1.7\npractice-frozen")}
	ctx := context.Background()
	id := seedPaperBasket(t, d, ctx, "xiaoming")

	prepared, replay, err := d.PreparePracticePrint(ctx, "xiaoming", id, "print-click-001", k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if replay {
		t.Fatal("first prepare must not be a replay")
	}
	if prepared.Job.Status != k12.PrintJobPreparing || prepared.Job.PaperNo == "" || !strings.HasPrefix(prepared.Job.PaperNo, "P-") {
		t.Fatalf("prepare must return formal paper number and preparing job: %+v", prepared.Job)
	}
	if prepared.Job.QuestionArtifactID == "" || prepared.Job.AnswerArtifactID == "" || prepared.Job.SourceDigest == "" {
		t.Fatalf("prepare must freeze same-source question/answer artifacts: %+v", prepared.Job)
	}
	set, err := d.GetPracticeSet(ctx, "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	if set.Record.Status != k12.PracticeStatusDraft || set.Fields.PaperNo != "" || set.Fields.FinalizedAt != 0 || set.Fields.QuestionArtifact != "" {
		t.Fatalf("prepare must leave PracticeSet entirely unfinalized: status=%s fields=%+v", set.Record.Status, set.Fields)
	}

	question, err := d.RenderPracticePrintJobPaper(ctx, "xiaoming", prepared.Job.PrintJobID, k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := d.RenderPracticePrintJobPaper(ctx, "xiaoming", prepared.Job.PrintJobID, k12.PaperKindAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if question.PaperNo != prepared.Job.PaperNo || answer.PaperNo != prepared.Job.PaperNo ||
		question.SourceDigest != answer.SourceDigest || question.SourceDigest != prepared.Job.SourceDigest {
		t.Fatalf("prepared papers must share paper_no/source: q=%+v a=%+v job=%+v", question, answer, prepared.Job)
	}
	if strings.Contains(question.Markdown, "x = 16") || !strings.Contains(answer.Markdown, "x = 16") {
		t.Fatal("prepared question/answer separation drifted")
	}

	again, replay, err := d.PreparePracticePrint(ctx, "xiaoming", id, "print-click-001", k12.PaperKindQuestion)
	if err != nil || !replay {
		t.Fatalf("same prepare must replay: replay=%v err=%v", replay, err)
	}
	if again.Job.PrintJobID != prepared.Job.PrintJobID || again.Job.PaperNo != prepared.Job.PaperNo || again.Job.SourceDigest != prepared.Job.SourceDigest {
		t.Fatalf("prepare replay changed stable reservation: first=%+v replay=%+v", prepared.Job, again.Job)
	}
}

func TestPracticePrintPaperFreezesCanonicalArtifact(t *testing.T) {
	d := newDataDeps(t)
	renderer := &v45PDFRenderer{data: []byte("%PDF-1.7\npractice-frozen")}
	d.Renderer = renderer
	ctx := context.Background()
	id := seedPaperBasket(t, d, ctx, "xiaoming")

	prepared, _, err := d.PreparePracticePrint(ctx, "xiaoming", id, "canonical-practice-print", k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	paper, err := d.RenderPracticePrintJobPaper(ctx, "xiaoming", prepared.Job.PrintJobID, k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}

	artifact, err := d.Records.GetPrintArtifact(ctx, "xiaoming", paper.ArtifactID)
	if err != nil {
		t.Fatalf("practice paper must persist its canonical artifact: %v", err)
	}
	if artifact.ArtifactID != prepared.Job.ArtifactID || artifact.CanonicalMarkdown != paper.Markdown {
		t.Fatalf("canonical artifact drifted: artifact=%+v paper=%+v job=%+v", artifact, paper, prepared.Job)
	}
	render, err := d.Records.GetPrintArtifactRender(ctx, "xiaoming", paper.ArtifactID)
	if err != nil {
		t.Fatalf("practice paper must persist its frozen PDF: %v", err)
	}
	if !bytes.Equal(render.Payload, []byte("%PDF-1.7\npractice-frozen")) || render.ByteDigest == "" {
		t.Fatalf("unexpected frozen PDF: %+v", render)
	}
	answer, err := d.RenderPracticePrintJobPaper(ctx, "xiaoming", prepared.Job.PrintJobID, k12.PaperKindAnswer)
	if err != nil {
		t.Fatal(err)
	}
	answerArtifact, err := d.Records.GetPrintArtifact(ctx, "xiaoming", answer.ArtifactID)
	if err != nil || answerArtifact.SourceKind != k12.PrintSourcePracticeAnswer || answerArtifact.CanonicalMarkdown != answer.Markdown {
		t.Fatalf("answer artifact is not canonical: artifact=%+v paper=%+v err=%v", answerArtifact, answer, err)
	}
	again, err := d.RenderPracticePrintJobPaper(ctx, "xiaoming", prepared.Job.PrintJobID, k12.PaperKindQuestion)
	if err != nil || again.ArtifactID != paper.ArtifactID || again.Markdown != paper.Markdown {
		t.Fatalf("replayed paper drifted: first=%+v replay=%+v err=%v", paper, again, err)
	}
	if renderer.callCount() != 2 {
		t.Fatalf("replayed canonical artifacts rendered %d times", renderer.callCount())
	}
}

func TestPracticePrintConcurrentPrepareHasUniqueStablePaperNumbers(t *testing.T) {
	d := newFileBackedDeps(t)
	ctx := context.Background()
	const n = 16
	setIDs := make([]string, n)
	for i := range setIDs {
		id, created, err := d.CreatePracticeSet(ctx, "xiaoming", "s", k12.PracticeSetFields{
			SourceKind: k12.PracticeSourceCustom,
			Title:      fmt.Sprintf("并发打印卷-%02d", i),
			Items: []k12.PracticeItem{{
				ItemID: "q", Subject: "数学", QuestionMarkdown: fmt.Sprintf("%d+1=?", i),
				ExpectedAnswerMarkdown: fmt.Sprintf("%d", i+1), VerificationStatus: k12.PracticeItemVerified,
				VerificationEvidence: "独立验算",
			}},
		})
		if err != nil || !created {
			t.Fatalf("seed set %d: created=%v err=%v", i, created, err)
		}
		setIDs[i] = id
	}

	start := make(chan struct{})
	jobs := make(chan k12.PracticePrintJob, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i, setID := range setIDs {
		wg.Add(1)
		go func(i int, setID string) {
			defer wg.Done()
			<-start
			view, _, err := d.PreparePracticePrint(ctx, "xiaoming", setID, fmt.Sprintf("click-%02d", i), k12.PaperKindQuestion)
			if err != nil {
				errs <- err
				return
			}
			jobs <- view.Job
		}(i, setID)
	}
	close(start)
	wg.Wait()
	close(jobs)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent prepare: %v", err)
	}
	seen := make(map[string]string, n)
	for job := range jobs {
		if prior, exists := seen[job.PaperNo]; exists {
			t.Fatalf("Tutor-local paper_no collision %s: %s and %s", job.PaperNo, prior, job.PrintJobID)
		}
		seen[job.PaperNo] = job.PrintJobID
	}
	if len(seen) != n {
		t.Fatalf("prepared jobs=%d want=%d", len(seen), n)
	}
	for _, setID := range setIDs {
		set, err := d.GetPracticeSet(ctx, "xiaoming", setID)
		if err != nil || set.Record.Status != k12.PracticeStatusDraft || set.Fields.PaperNo != "" {
			t.Fatalf("concurrent prepare finalized %s: set=%+v err=%v", setID, set, err)
		}
	}
}

func TestPracticePrintConcurrentSameClickCreatesOneJob(t *testing.T) {
	d := newFileBackedDeps(t)
	ctx := context.Background()
	id, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceCustom, Title: "同一点击并发卷",
		Items: []k12.PracticeItem{{ItemID: "q", Subject: "数学", QuestionMarkdown: "8+9=?",
			ExpectedAnswerMarkdown: "17", VerificationStatus: k12.PracticeItemVerified,
			VerificationEvidence: "独立验算"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
	start := make(chan struct{})
	jobs := make(chan k12.PracticePrintJob, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, _, err := d.PreparePracticePrint(ctx, "xiaoming", id, "one-physical-click", k12.PaperKindQuestion)
			if err != nil {
				errs <- err
				return
			}
			jobs <- v.Job
		}()
	}
	close(start)
	wg.Wait()
	close(jobs)
	close(errs)
	for err := range errs {
		t.Fatalf("same-click prepare: %v", err)
	}
	var first k12.PracticePrintJob
	count := 0
	for job := range jobs {
		if count == 0 {
			first = job
		} else if job.PrintJobID != first.PrintJobID || job.PaperNo != first.PaperNo || job.SourceDigest != first.SourceDigest {
			t.Fatalf("same click forked jobs: first=%+v other=%+v", first, job)
		}
		count++
	}
	if count != n {
		t.Fatalf("responses=%d want=%d", count, n)
	}
	var rows int
	if err := d.Records.DB().QueryRow(`SELECT COUNT(*) FROM k12_print_jobs WHERE agent_name='xiaoming'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("durable jobs=%d err=%v, want 1", rows, err)
	}
}

func TestPracticePrintReservationDoesNotCollideWithLegacyFinalize(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	newSet := func(title, q string) string {
		t.Helper()
		id, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", k12.PracticeSetFields{
			SourceKind: k12.PracticeSourceCustom, Title: title,
			Items: []k12.PracticeItem{{ItemID: "q", Subject: "数学", QuestionMarkdown: q,
				ExpectedAnswerMarkdown: "答案", VerificationStatus: k12.PracticeItemVerified,
				VerificationEvidence: "独立验算"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	reservedID := newSet("新 Desktop 预占", "题一")
	legacyID := newSet("旧客户端固化", "题二")
	prepared, _, err := d.PreparePracticePrint(ctx, "xiaoming", reservedID, "new-desktop", k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _, err := d.FinalizeBasket(ctx, "xiaoming", legacyID, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Fields.PaperNo == prepared.Job.PaperNo {
		t.Fatalf("legacy finalize reused reserved paper_no %s", legacy.Fields.PaperNo)
	}
}

func TestPracticePrintCommitRollsBackBothSidesAndRecoversAfterCrashPoint(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id := seedPaperBasket(t, d, ctx, "xiaoming")
	prepared, _, err := d.PreparePracticePrint(ctx, "xiaoming", id, "crash-before-receipt", k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a process restart after phase one: the same command resolves the
	// persisted job instead of allocating another paper number.
	recovered, replay, err := d.PreparePracticePrint(ctx, "xiaoming", id, "crash-before-receipt", k12.PaperKindQuestion)
	if err != nil || !replay || recovered.Job.PrintJobID != prepared.Job.PrintJobID || recovered.Job.PaperNo != prepared.Job.PaperNo {
		t.Fatalf("prepare crash recovery drifted: before=%+v recovered=%+v replay=%v err=%v", prepared.Job, recovered.Job, replay, err)
	}
	if _, err := d.RecordPracticePrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
		Status: k12.PrintJobDialogOpen,
	}); err != nil {
		t.Fatal(err)
	}

	// Inject a crash/failure exactly after the PracticeSet UPDATE would run and
	// before the PrintJob receipt UPDATE. SQLite must roll the set mutation back.
	if _, err := d.Records.DB().Exec(`CREATE TRIGGER inject_print_receipt_failure
		BEFORE UPDATE OF status ON k12_print_jobs
		WHEN NEW.status='printed'
		BEGIN SELECT RAISE(ABORT,'injected receipt checkpoint crash'); END;`); err != nil {
		t.Fatal(err)
	}
	event := usecase.PracticePrintEvent{
		Status: k12.PrintJobPrinted, NativeJobID: "native-crash", NativeReceiptID: "receipt-crash",
		PrinterSnapshot: `{"printer":"Office","paper":"A4","copies":1}`,
	}
	if _, err := d.RecordPracticePrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, event); err == nil {
		t.Fatal("injected receipt checkpoint must fail")
	}
	set, _ := d.GetPracticeSet(ctx, "xiaoming", id)
	job, _ := d.GetPracticePrint(ctx, "xiaoming", prepared.Job.PrintJobID)
	if set.Record.Status != k12.PracticeStatusDraft || set.Fields.PaperNo != "" || set.Fields.FinalizedAt != 0 ||
		job.Job.Status == k12.PrintJobPrinted || job.Job.NativeReceiptID != "" {
		t.Fatalf("transaction left partial state: set=%+v job=%+v", set, job.Job)
	}
	if _, err := d.Records.DB().Exec(`DROP TRIGGER inject_print_receipt_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RecordPracticePrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, event); err != nil {
		t.Fatalf("same receipt must recover after rollback: %v", err)
	}
	set, _ = d.GetPracticeSet(ctx, "xiaoming", id)
	job, _ = d.GetPracticePrint(ctx, "xiaoming", prepared.Job.PrintJobID)
	if set.Record.Status != k12.PracticeStatusAssigned || job.Job.Status != k12.PrintJobPrinted {
		t.Fatalf("recovery did not converge atomically: set=%s job=%s", set.Record.Status, job.Job.Status)
	}
}

func TestPracticePrintCommitRequiresNativeReceiptAndIsIdempotent(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id := seedPaperBasket(t, d, ctx, "xiaoming")
	prepared, _, err := d.PreparePracticePrint(ctx, "xiaoming", id, "print-click-commit", k12.PaperKindQuestion)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := d.RecordPracticePrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
		Status: k12.PrintJobPrinted,
	}); err == nil {
		t.Fatal("printed without native receipt must be rejected")
	}
	set, _ := d.GetPracticeSet(ctx, "xiaoming", id)
	if set.Record.Status != k12.PracticeStatusDraft || set.Fields.PaperNo != "" {
		t.Fatal("invalid receipt caused partial finalization")
	}
	if _, err := d.RecordPracticePrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
		Status: k12.PrintJobDialogOpen,
	}); err != nil {
		t.Fatal(err)
	}

	printed, err := d.RecordPracticePrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
		Status:          k12.PrintJobPrinted,
		NativeJobID:     "native-job-42",
		NativeReceiptID: "native-receipt-42",
		PrinterSnapshot: `{"printer":"Office","paper":"A4","copies":1}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if printed.Job.Status != k12.PrintJobPrinted || printed.Job.NativeReceiptID != "native-receipt-42" {
		t.Fatalf("definitive native receipt did not commit PrintJob: %+v", printed.Job)
	}
	set, err = d.GetPracticeSet(ctx, "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	if set.Record.Status != k12.PracticeStatusAssigned || set.Fields.PaperNo != prepared.Job.PaperNo ||
		set.Fields.FinalizedVia != "print" || set.Fields.FinalizedAt == 0 ||
		set.Fields.QuestionArtifact != prepared.Job.QuestionArtifactID {
		t.Fatalf("native success and PracticeSet finalization must commit together: %+v", set)
	}
	version := set.Record.Version

	replayed, err := d.RecordPracticePrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
		Status:          k12.PrintJobPrinted,
		NativeJobID:     "native-job-42",
		NativeReceiptID: "native-receipt-42",
		PrinterSnapshot: `{"printer":"Office","paper":"A4","copies":1}`,
	})
	if err != nil || replayed.Job.PrintJobID != prepared.Job.PrintJobID {
		t.Fatalf("same receipt replay must be idempotent: %+v err=%v", replayed, err)
	}
	set, _ = d.GetPracticeSet(ctx, "xiaoming", id)
	if set.Record.Version != version {
		t.Fatalf("receipt replay advanced PracticeSet twice: %d -> %d", version, set.Record.Version)
	}
}

func TestPracticePrintCancelFailureRetrySameReservationUnknownDoesNotRetry(t *testing.T) {
	for _, terminal := range []string{k12.PrintJobCancelled, k12.PrintJobFailed} {
		t.Run(terminal, func(t *testing.T) {
			d := newDataDeps(t)
			ctx := context.Background()
			id := seedPaperBasket(t, d, ctx, "xiaoming")
			prepared, _, err := d.PreparePracticePrint(ctx, "xiaoming", id, "print-"+terminal, k12.PaperKindQuestion)
			if err != nil {
				t.Fatal(err)
			}
			ended, err := d.RecordPracticePrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
				Status: terminal, FailureKind: "native_" + terminal,
			})
			if err != nil || ended.Job.Status != terminal {
				t.Fatalf("record terminal=%s: job=%+v err=%v", terminal, ended.Job, err)
			}
			set, _ := d.GetPracticeSet(ctx, "xiaoming", id)
			if set.Record.Status != k12.PracticeStatusDraft || set.Fields.PaperNo != "" || set.Fields.FinalizedAt != 0 {
				t.Fatalf("%s must have zero PracticeSet finalization: %+v", terminal, set)
			}
			retried, err := d.RetryPracticePrint(ctx, "xiaoming", prepared.Job.PrintJobID)
			if err != nil {
				t.Fatal(err)
			}
			if retried.Job.Status != k12.PrintJobPreparing || retried.Job.AttemptCount != prepared.Job.AttemptCount+1 ||
				retried.Job.PrintJobID != prepared.Job.PrintJobID || retried.Job.PaperNo != prepared.Job.PaperNo ||
				retried.Job.SourceDigest != prepared.Job.SourceDigest {
				t.Fatalf("retry must reuse exact job/paper/source: before=%+v after=%+v", prepared.Job, retried.Job)
			}
		})
	}

	d := newDataDeps(t)
	ctx := context.Background()
	id := seedPaperBasket(t, d, ctx, "xiaoming")
	prepared, _, _ := d.PreparePracticePrint(ctx, "xiaoming", id, "print-unknown", k12.PaperKindQuestion)
	unknown, err := d.RecordPracticePrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
		Status: k12.PrintJobOutcomeUnknown, FailureKind: "receipt_lost",
	})
	if err != nil || unknown.Job.Status != k12.PrintJobOutcomeUnknown {
		t.Fatalf("unknown must be durable: %+v err=%v", unknown, err)
	}
	if _, err := d.RetryPracticePrint(ctx, "xiaoming", prepared.Job.PrintJobID); err == nil {
		t.Fatal("outcome_unknown must not expose ordinary retry")
	}
	set, _ := d.GetPracticeSet(ctx, "xiaoming", id)
	if set.Record.Status != k12.PracticeStatusDraft || set.Fields.FinalizedAt != 0 {
		t.Fatal("outcome_unknown was treated as print success")
	}
}
