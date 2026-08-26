package k12storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestInboundPhotoPermanentFailureIsDurableImmutableAndNotRecoverable(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "terminal.sqlite")
	db, store := openInboundPhotoStore(t, dbPath)
	seedInboundAgent(t, db, "mingming")
	bundle, _, err := store.AdmitInboundPhoto(
		ctx, inboundAdmission("msg-terminal", []byte("permanent failure image")),
	)
	if err != nil {
		t.Fatal(err)
	}
	next := bundle.Dispatch.State()
	next.ProcessingStatus = k12storage.InboundPhotoImageTaskSubmitted
	next.RoutingDecision = k12storage.InboundPhotoRouteNewSubmission
	next.ImageTaskID = "image-task-1"
	submitted, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, bundle.Dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	next = submitted.State()
	next.TerminalStatus = k12storage.InboundPhotoTerminalFailed
	next.TerminalStage = k12storage.InboundPhotoTerminalStageImageTask
	next.FailureKind = "model_capability_unverified"
	failed, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, submitted.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.TerminalStatus != k12storage.InboundPhotoTerminalFailed ||
		failed.TerminalStage != k12storage.InboundPhotoTerminalStageImageTask ||
		failed.FailureKind != "model_capability_unverified" ||
		failed.ProcessingStatus != k12storage.InboundPhotoImageTaskSubmitted ||
		failed.ImageTaskID != "image-task-1" {
		t.Fatalf("terminal state=%+v", failed)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, store = openInboundPhotoStore(t, dbPath)
	defer db.Close()
	stored, err := store.GetInboundPhoto(ctx, "mingming", bundle.Receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Dispatch.State() != failed.State() || stored.Dispatch.Version != failed.Version {
		t.Fatalf("terminal state did not survive restart: stored=%+v failed=%+v", stored.Dispatch, failed)
	}
	recoverable, err := store.ListRecoverableInboundPhotos(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 0 {
		t.Fatalf("terminal receipt remained recoverable: %+v", recoverable)
	}
	if _, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, failed.Version, failed.State(),
	); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("terminal replay err=%v", err)
	}
	stable, err := store.GetInboundPhoto(ctx, "mingming", bundle.Receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if stable.Dispatch.Version != failed.Version {
		t.Fatalf("terminal replay changed version=%d want=%d", stable.Dispatch.Version, failed.Version)
	}
}

func TestInboundPhotoTerminalFenceRejectsUnstructuredFailureKind(t *testing.T) {
	ctx := context.Background()
	db, store := openInboundPhotoStore(t, filepath.Join(t.TempDir(), "invalid-terminal.sqlite"))
	defer db.Close()
	seedInboundAgent(t, db, "mingming")
	bundle, _, err := store.AdmitInboundPhoto(
		ctx, inboundAdmission("msg-invalid-terminal", []byte("image")),
	)
	if err != nil {
		t.Fatal(err)
	}
	next := bundle.Dispatch.State()
	next.ProcessingStatus = k12storage.InboundPhotoImageTaskSubmitted
	next.RoutingDecision = k12storage.InboundPhotoRouteNewSubmission
	next.ImageTaskID = "image-task-1"
	submitted, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, bundle.Dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	next = submitted.State()
	next.TerminalStatus = k12storage.InboundPhotoTerminalFailed
	next.TerminalStage = k12storage.InboundPhotoTerminalStageImageTask
	next.FailureKind = "Provider failed!"
	if _, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, submitted.Version, next,
	); err == nil {
		t.Fatal("unstructured failure kind was accepted")
	}
}

func TestInboundPhotoDeliveryFailureRequiresAndPreservesBoundBatch(t *testing.T) {
	ctx := context.Background()
	db, store := openInboundPhotoStore(t, filepath.Join(t.TempDir(), "delivery-terminal.sqlite"))
	defer db.Close()
	seedInboundAgent(t, db, "mingming")
	bundle, _, err := store.AdmitInboundPhoto(
		ctx, inboundAdmission("msg-delivery-terminal", []byte("image")),
	)
	if err != nil {
		t.Fatal(err)
	}
	invalid := bundle.Dispatch.State()
	invalid.TerminalStatus = k12storage.InboundPhotoTerminalFailed
	invalid.TerminalStage = k12storage.InboundPhotoTerminalStageDelivery
	invalid.FailureKind = "delivery_batch_failed"
	if _, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, bundle.Dispatch.Version, invalid,
	); err == nil {
		t.Fatal("delivery failure without a bound batch was accepted")
	}

	next := bundle.Dispatch.State()
	next.ProcessingStatus = k12storage.InboundPhotoImageTaskSubmitted
	next.RoutingDecision = k12storage.InboundPhotoRouteNewSubmission
	next.ImageTaskID = "image-task-1"
	dispatch, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, bundle.Dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	next = dispatch.State()
	next.ProcessingStatus = k12storage.InboundPhotoFinalArtifactReady
	next.FinalArtifactID = "artifact-1"
	next.ReplyStatus = k12storage.InboundPhotoReplyReady
	dispatch, err = store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	next = dispatch.State()
	next.ReplyStatus = k12storage.InboundPhotoReplyBound
	next.DeliveryBatchID = "batch-1"
	dispatch, err = store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	next = dispatch.State()
	next.TerminalStatus = k12storage.InboundPhotoTerminalFailed
	next.TerminalStage = k12storage.InboundPhotoTerminalStageDelivery
	next.FailureKind = "delivery_batch_partial_failed"
	failed, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.ReplyStatus != k12storage.InboundPhotoReplyBound ||
		failed.DeliveryBatchID != "batch-1" ||
		failed.TerminalStage != k12storage.InboundPhotoTerminalStageDelivery {
		t.Fatalf("bound delivery checkpoint changed at terminal transition: %+v", failed)
	}
}
