package usecase_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type v45PDFRenderer struct {
	mu          sync.Mutex
	calls       int
	data        []byte
	contentType string
	err         error
}

func (r *v45PDFRenderer) Render(context.Context, string, string) ([]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	contentType := r.contentType
	if contentType == "" {
		contentType = "application/pdf"
	}
	return append([]byte(nil), r.data...), contentType, r.err
}

func (r *v45PDFRenderer) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestPrintableArtifactV45IsSharedByArtifactOnlyAndPrint(t *testing.T) {
	d := newDataDeps(t)
	renderer := &v45PDFRenderer{data: []byte("%PDF-1.7\none-frozen-payload")}
	d.Renderer = renderer
	ctx := context.Background()
	req := usecase.PreparePrintableArtifactRequest{
		AgentName: "xiaoming", SourceKind: k12.PrintSourceTutoringTips,
		SourceRef: "submission:v45", Title: "这份作业的辅导要点",
		CanonicalMarkdown: "# 辅导要点\n\n小数点对齐",
	}
	artifactOnly, replay, err := d.PreparePrintableArtifact(ctx, req)
	if err != nil || replay {
		t.Fatalf("artifact-only=%+v replay=%v err=%v", artifactOnly, replay, err)
	}
	jobKey := "v45-print-after-export"
	jobSum := sha256.Sum256([]byte("xiaoming\x00" + jobKey))
	if _, err := d.GetGenericPrint(ctx, "xiaoming", "gprint-"+hex.EncodeToString(jobSum[:12])); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("artifact-only prepare must create zero PrintJobs: %v", err)
	}
	printView, _, err := d.PrepareGenericPrint(ctx, usecase.PrepareGenericPrintRequest{
		AgentName: req.AgentName, IdempotencyKey: jobKey, SourceKind: req.SourceKind,
		SourceRef: req.SourceRef, Title: req.Title, CanonicalMarkdown: req.CanonicalMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	if printView.Artifact.ArtifactID != artifactOnly.Artifact.ArtifactID ||
		printView.Render.ByteDigest != artifactOnly.Render.ByteDigest ||
		!bytes.Equal(printView.Render.Payload, artifactOnly.Render.Payload) {
		t.Fatalf("print and artifact-only diverged: print=%+v artifact=%+v", printView, artifactOnly)
	}
	if renderer.callCount() != 1 {
		t.Fatalf("same artifact rendered %d times", renderer.callCount())
	}
	paper, err := d.RenderGenericPrintArtifact(ctx, "xiaoming", printView.Job.PrintJobID)
	if err != nil || !bytes.Equal(paper.PDF, artifactOnly.Render.Payload) ||
		paper.ByteDigest != artifactOnly.Render.ByteDigest {
		t.Fatalf("paper=%+v err=%v", paper, err)
	}
}

func TestPrintableArtifactV45MissingRendererFailsBeforePrintJob(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	key := "v45-missing-renderer"
	_, _, err := d.PrepareGenericPrint(ctx, usecase.PrepareGenericPrintRequest{
		AgentName: "xiaoming", IdempotencyKey: key, SourceKind: k12.PrintSourceTutoringTips,
		SourceRef: "submission:no-renderer", Title: "辅导要点", CanonicalMarkdown: "# 辅导要点",
	})
	if !errors.Is(err, usecase.ErrRenderUnavailable) {
		t.Fatalf("missing renderer error=%v", err)
	}
	jobSum := sha256.Sum256([]byte("xiaoming\x00" + key))
	if _, err := d.GetGenericPrint(ctx, "xiaoming", "gprint-"+hex.EncodeToString(jobSum[:12])); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("failed PDF prepare created a PrintJob: %v", err)
	}
}
