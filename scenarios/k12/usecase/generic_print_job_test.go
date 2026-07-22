package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestGenericPrintUsecaseFreezesCanonicalArtifactAndDoesNotMutateSource(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	setID := seedPaperBasket(t, d, ctx, "xiaoming")
	before, err := d.GetPracticeSet(ctx, "xiaoming", setID)
	if err != nil {
		t.Fatal(err)
	}
	prepared, replay, err := d.PrepareGenericPrint(ctx, usecase.PrepareGenericPrintRequest{
		AgentName: "xiaoming", IdempotencyKey: "history-reprint-1",
		SourceKind: k12.PrintSourcePracticeQuestion, SourceRef: "practice-set:" + setID + ":question",
		Title: "历史题目卷", CanonicalMarkdown: "# 历史题目卷\n\n1. 解方程",
	})
	if err != nil || replay || prepared.Job.Status != k12.PrintJobPreparing || prepared.Artifact.SourceDigest == "" {
		t.Fatalf("prepared=%+v replay=%v err=%v", prepared, replay, err)
	}
	paper, err := d.RenderGenericPrintArtifact(ctx, "xiaoming", prepared.Job.PrintJobID)
	if err != nil || paper.Markdown != "# 历史题目卷\n\n1. 解方程" || paper.SourceDigest != prepared.Artifact.SourceDigest {
		t.Fatalf("paper=%+v err=%v", paper, err)
	}
	if _, err := d.RecordGenericPrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
		Status: k12.PrintJobDialogOpen,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RecordGenericPrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
		Status: k12.PrintJobPrinted, NativeJobID: "native-1", NativeReceiptID: "receipt-1",
		PrinterSnapshot: `{"printer":"Office","paper":"A4"}`,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := d.GetPracticeSet(ctx, "xiaoming", setID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Record.Status != before.Record.Status || after.Record.Version != before.Record.Version || after.Record.Fields != before.Record.Fields {
		t.Fatalf("generic print mutated source PracticeSet: before=%+v after=%+v", before.Record, after.Record)
	}
}

func TestGenericPrintUsecaseRejectsEmptyPrinterSnapshot(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	prepared, _, err := d.PrepareGenericPrint(ctx, usecase.PrepareGenericPrintRequest{
		AgentName: "xiaoming", IdempotencyKey: "empty-snapshot",
		SourceKind: k12.PrintSourcePrepCard, SourceRef: "submission:s1",
		Title: "辅导要点", CanonicalMarkdown: "# 辅导要点",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RecordGenericPrintEvent(ctx, "xiaoming", prepared.Job.PrintJobID, usecase.PracticePrintEvent{
		Status: k12.PrintJobPrinted, NativeJobID: "native-1", NativeReceiptID: "receipt-1",
		PrinterSnapshot: `{}`,
	}); err == nil {
		t.Fatal("printed must carry a non-empty printer snapshot")
	}
}
