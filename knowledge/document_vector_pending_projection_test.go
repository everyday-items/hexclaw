package knowledge

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestListDocumentVectorProjectionsKeepsAcceptedUploadPendingUntilTextReady(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	body := "accepted but not extracted yet"
	accepted, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: "pending-vector-after-accept",
		Filename:       "pending.txt",
		MediaType:      "text/plain",
		SizeBytes:      int64(len(body)),
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.TextIndexState != TextIndexPending || accepted.VectorIndexState != VectorIndexPending {
		t.Fatalf("accepted projection=%+v", accepted)
	}

	projections, err := h.repo.ListDocumentVectorProjections(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	projection, ok := projections[accepted.DocumentID]
	if !ok {
		t.Fatalf("accepted document %q missing from vector projections: %+v", accepted.DocumentID, projections)
	}
	if projection.VectorIndexState != VectorIndexPending || projection.JobID != "" ||
		projection.JobState != "" || projection.LastError != "" {
		t.Fatalf("text-pending upload was reported as terminal vector state: %+v", projection)
	}

	documents, err := h.store.List(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].ID != accepted.DocumentID || documents[0].Status != "processing" {
		t.Fatalf("accepted document repository projection=%+v", documents)
	}
}
