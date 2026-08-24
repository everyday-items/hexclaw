package k12storage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func deliveryMessagePartBatchFixture() k12.DeliveryBatch {
	target := k12.DeliveryTarget{
		Platform: "dingtalk", InstanceID: "bot-a", ChatID: "parent", Label: "dingtalk",
	}
	return k12.DeliveryBatch{
		BatchID: "batch-parts", AgentName: "mingming",
		ObjectKind: "practice_set", ObjectID: "practice-1",
		DedupeKey: "batch-parts-dedupe", ContentDigest: "sha256:batch-content",
		Receipts: []k12.DeliveryReceipt{
			{
				DeliveryID: "delivery-markdown", BindingID: "binding-a", Target: target,
				PartKind: messagecontent.PartMarkdown, PartOrdinal: 1,
				PartDigest: "sha256:markdown", DedupeKey: "part-markdown",
				PayloadDigest: "sha256:payload-markdown", PayloadJSON: `{"kind":"markdown"}`, RenderJSON: `{}`,
			},
			{
				DeliveryID: "delivery-image", BindingID: "binding-a", Target: target,
				PartKind: messagecontent.PartArtifact, PartMIME: "image/png", PartOrdinal: 2,
				PartDigest: "sha256:image", DedupeKey: "part-image",
				PayloadDigest: "sha256:payload-image", PayloadJSON: `{"kind":"artifact","mime":"image/png"}`, RenderJSON: `{}`,
			},
			{
				DeliveryID: "delivery-pdf", BindingID: "binding-a", Target: target,
				PartKind: messagecontent.PartArtifact, PartMIME: "application/pdf", PartOrdinal: 3,
				PartDigest: "sha256:pdf", DedupeKey: "part-pdf",
				PayloadDigest: "sha256:payload-pdf", PayloadJSON: `{"kind":"artifact","mime":"application/pdf"}`, RenderJSON: `{}`,
			},
		},
	}
}

func TestDeliveryMessagePartsPersistSameTargetWithIndependentOrdinals(t *testing.T) {
	store := setupDeliveryStore(t)
	batch, created, err := store.PrepareDeliveryBatch(
		context.Background(), deliveryMessagePartBatchFixture(),
	)
	if err != nil || !created || len(batch.Receipts) != 3 {
		t.Fatalf("persist target×part batch: created=%v batch=%+v err=%v", created, batch, err)
	}
	for i, receipt := range batch.Receipts {
		if receipt.BatchOrdinal != i+1 || receipt.PartOrdinal != i+1 ||
			receipt.PartKind == "" || receipt.PartDigest == "" {
			t.Fatalf("part %d did not round-trip: %+v", i, receipt)
		}
	}
}

func TestDeliveryMessagePartResourcesPersistAndIncompleteBatchFailsAtomically(t *testing.T) {
	store := setupDeliveryStore(t)
	ctx := context.Background()
	batch, _, err := store.PrepareDeliveryBatch(ctx, deliveryMessagePartBatchFixture())
	if err != nil {
		t.Fatal(err)
	}
	image := batch.Receipts[1]
	pdf := batch.Receipts[2]
	storedImage, err := store.SaveDeliveryPreparedResource(
		ctx, batch.AgentName, image.DeliveryID, "image-media-id",
	)
	if err != nil || storedImage.PreparedResourceID != "image-media-id" ||
		storedImage.Status != k12.DeliveryPending {
		t.Fatalf("persist image preflight evidence: receipt=%+v err=%v", storedImage, err)
	}
	if _, err := store.SaveDeliveryPreparedResource(
		ctx, batch.AgentName, image.DeliveryID, "different-image-media-id",
	); err == nil {
		t.Fatal("prepared resource identity must be immutable once persisted")
	}

	failed, incomplete, err := store.FailDeliveryBatchPreparationIfIncomplete(
		ctx, batch.AgentName, batch.BatchID, "media preflight failed",
	)
	if err != nil || !incomplete || failed.Status != k12.DeliveryBatchFailed {
		t.Fatalf("incomplete preflight must fail all unsent parts: incomplete=%v batch=%+v err=%v",
			incomplete, failed, err)
	}
	for _, receipt := range failed.Receipts {
		if receipt.Status != k12.DeliveryFailed || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
			t.Fatalf("preflight failure is not a provider send attempt: %+v", failed.Receipts)
		}
	}
	if failed.Receipts[1].PreparedResourceID != "image-media-id" ||
		failed.Receipts[2].PreparedResourceID != "" {
		t.Fatalf("batch failure lost successful media evidence or invented missing evidence: %+v", failed.Receipts)
	}

	storedPDF, err := store.SaveDeliveryPreparedResource(
		ctx, batch.AgentName, pdf.DeliveryID, "pdf-media-id",
	)
	if err != nil || storedPDF.PreparedResourceID != "pdf-media-id" || storedPDF.Status != k12.DeliveryFailed {
		t.Fatalf("failed batch must allow only its missing media preflight to converge: receipt=%+v err=%v", storedPDF, err)
	}
	ready, incomplete, err := store.FailDeliveryBatchPreparationIfIncomplete(
		ctx, batch.AgentName, batch.BatchID, "stale failure must not win",
	)
	if err != nil || incomplete || ready.Status != k12.DeliveryBatchFailed {
		t.Fatalf("durably complete resources must defeat a stale concurrent failure: incomplete=%v batch=%+v err=%v",
			incomplete, ready, err)
	}
	encoded, err := json.Marshal(storedPDF)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("pdf-media-id")) || bytes.Contains(encoded, []byte("prepared_resource")) {
		t.Fatalf("internal provider media reference leaked through receipt JSON: %s", encoded)
	}
}

func TestDeliveryMessagePartBatchRejectsDuplicateOrGappedOrdinals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*k12.DeliveryBatch)
	}{
		{
			name: "duplicate ordinal",
			mutate: func(batch *k12.DeliveryBatch) {
				batch.Receipts[2].PartOrdinal = 2
			},
		},
		{
			name: "gapped ordinal",
			mutate: func(batch *k12.DeliveryBatch) {
				batch.Receipts[1].PartOrdinal = 3
			},
		},
		{
			name: "artifact without MIME",
			mutate: func(batch *k12.DeliveryBatch) {
				batch.Receipts[1].PartMIME = ""
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := setupDeliveryStore(t)
			batch := deliveryMessagePartBatchFixture()
			tt.mutate(&batch)
			if _, _, err := store.PrepareDeliveryBatch(context.Background(), batch); err == nil {
				t.Fatal("invalid target×part manifest must fail before persistence")
			}
		})
	}
}
