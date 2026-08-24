package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type inboundPhotoRepositoryFake struct {
	bundle             usecase.InboundPhotoBundle
	lastLookupIdentity usecase.InboundPhotoIdentity
}

func (f *inboundPhotoRepositoryFake) AdmitInboundPhoto(
	context.Context, usecase.InboundPhotoAdmission,
) (usecase.InboundPhotoBundle, bool, error) {
	return f.bundle, true, nil
}

func (f *inboundPhotoRepositoryFake) GetInboundPhoto(
	_ context.Context, agentName, receiptID string,
) (usecase.InboundPhotoBundle, error) {
	if f.bundle.Receipt.AgentName != agentName || f.bundle.Receipt.ReceiptID != receiptID {
		return usecase.InboundPhotoBundle{}, records.ErrNotFound
	}
	return f.bundle, nil
}

func (f *inboundPhotoRepositoryFake) GetInboundPhotoByIdentity(
	_ context.Context, identity usecase.InboundPhotoIdentity,
) (usecase.InboundPhotoBundle, error) {
	f.lastLookupIdentity = identity
	if identity != f.bundle.Receipt.Identity {
		return usecase.InboundPhotoBundle{}, records.ErrNotFound
	}
	return f.bundle, nil
}

func (f *inboundPhotoRepositoryFake) CompareAndSwapInboundPhotoDispatch(
	_ context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	next usecase.InboundPhotoDispatchState,
) (usecase.InboundPhotoDispatch, error) {
	if agentName != f.bundle.Receipt.AgentName || receiptID != f.bundle.Receipt.ReceiptID {
		return usecase.InboundPhotoDispatch{}, records.ErrNotFound
	}
	if f.bundle.Dispatch.Version != expectedVersion {
		return usecase.InboundPhotoDispatch{}, records.ErrVersionConflict
	}
	f.bundle.Dispatch.ProcessingStatus = next.ProcessingStatus
	f.bundle.Dispatch.RoutingDecision = next.RoutingDecision
	f.bundle.Dispatch.ConfirmationStatus = next.ConfirmationStatus
	f.bundle.Dispatch.ImageTaskID = next.ImageTaskID
	f.bundle.Dispatch.FinalArtifactID = next.FinalArtifactID
	f.bundle.Dispatch.ReplyStatus = next.ReplyStatus
	f.bundle.Dispatch.DeliveryBatchID = next.DeliveryBatchID
	f.bundle.Dispatch.Version++
	return f.bundle.Dispatch, nil
}

func (f *inboundPhotoRepositoryFake) ListRecoverableInboundPhotos(
	context.Context, int,
) ([]usecase.InboundPhotoBundle, error) {
	return []usecase.InboundPhotoBundle{f.bundle}, nil
}

func TestInboundPhotoCoordinatorAdvancesRestartOwnedObligationsByCAS(t *testing.T) {
	identity := usecase.InboundPhotoIdentity{
		Platform: "dingtalk", InstanceID: "robot-a", ChatID: "chat-a", ProviderMessageID: "msg-a",
	}
	repo := &inboundPhotoRepositoryFake{bundle: usecase.InboundPhotoBundle{
		Receipt: usecase.InboundPhotoReceipt{
			ReceiptID: "receipt-1", AgentName: "mingming", Identity: identity,
		},
		Dispatch: usecase.InboundPhotoDispatch{
			DispatchID: "dispatch-1",
			ReceiptID:  "receipt-1",
			InboundPhotoDispatchState: usecase.InboundPhotoDispatchState{
				ProcessingStatus:   k12storage.InboundPhotoAdmitted,
				RoutingDecision:    k12storage.InboundPhotoRoutePending,
				ConfirmationStatus: k12storage.InboundPhotoConfirmationNotRequired,
				ReplyStatus:        k12storage.InboundPhotoReplyPending,
			},
		},
	}}
	coordinator := usecase.NewInboundPhotoCoordinator(repo)
	ctx := context.Background()
	resumed, err := coordinator.ResumeByIdentity(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Receipt.ReceiptID != "receipt-1" || repo.lastLookupIdentity != identity {
		t.Fatalf("provider identity lookup was not forwarded exactly: resumed=%+v lookup=%+v", resumed, repo.lastLookupIdentity)
	}

	dispatch, err := coordinator.RecordImageTask(ctx, "mingming", "receipt-1", 0, "image-task-1")
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.ProcessingStatus != k12storage.InboundPhotoImageTaskSubmitted || dispatch.ImageTaskID != "image-task-1" {
		t.Fatalf("image task transition=%+v", dispatch)
	}
	dispatch, err = coordinator.RequestRoutingConfirmation(ctx, "mingming", "receipt-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.RoutingDecision != k12storage.InboundPhotoRouteAskedUser ||
		dispatch.ConfirmationStatus != k12storage.InboundPhotoConfirmationWaiting {
		t.Fatalf("confirmation request transition=%+v", dispatch)
	}
	dispatch, err = coordinator.ConfirmRouting(
		ctx, "mingming", "receipt-1", 2, usecase.InboundPhotoRouteRegrade,
	)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.RoutingDecision != k12storage.InboundPhotoRouteRegrade ||
		dispatch.ConfirmationStatus != k12storage.InboundPhotoConfirmationConfirmed {
		t.Fatalf("routing confirmation transition=%+v", dispatch)
	}
	dispatch, err = coordinator.RecordFinalArtifact(ctx, "mingming", "receipt-1", 3, "artifact-1")
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.ProcessingStatus != k12storage.InboundPhotoFinalArtifactReady ||
		dispatch.ReplyStatus != k12storage.InboundPhotoReplyReady {
		t.Fatalf("final artifact transition=%+v", dispatch)
	}
	dispatch, err = coordinator.BindReplyBatch(ctx, "mingming", "receipt-1", 4, "batch-1")
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.ReplyStatus != k12storage.InboundPhotoReplyBound || dispatch.DeliveryBatchID != "batch-1" {
		t.Fatalf("reply binding transition=%+v", dispatch)
	}
	dispatch, err = coordinator.CompleteReply(ctx, "mingming", "receipt-1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.ReplyStatus != k12storage.InboundPhotoReplyDelivered {
		t.Fatalf("reply completion transition=%+v", dispatch)
	}
	if _, err := coordinator.CompleteReply(ctx, "mingming", "receipt-1", 5); !errors.Is(err, records.ErrVersionConflict) {
		t.Fatalf("stale coordinator CAS err=%v", err)
	}
}

func TestInboundPhotoCoordinatorRejectsInvalidConfirmedRouteBeforeStoreMutation(t *testing.T) {
	repo := &inboundPhotoRepositoryFake{bundle: usecase.InboundPhotoBundle{
		Receipt: usecase.InboundPhotoReceipt{ReceiptID: "receipt-2", AgentName: "mingming"},
		Dispatch: usecase.InboundPhotoDispatch{
			DispatchID: "dispatch-2",
			ReceiptID:  "receipt-2",
			InboundPhotoDispatchState: usecase.InboundPhotoDispatchState{
				ProcessingStatus:   k12storage.InboundPhotoAdmitted,
				RoutingDecision:    k12storage.InboundPhotoRouteAskedUser,
				ConfirmationStatus: k12storage.InboundPhotoConfirmationWaiting,
				ReplyStatus:        k12storage.InboundPhotoReplyPending,
			},
			Version: 4,
		},
	}}
	coordinator := usecase.NewInboundPhotoCoordinator(repo)
	if _, err := coordinator.ConfirmRouting(
		context.Background(), "mingming", "receipt-2", 4, usecase.InboundPhotoRouteAskedUser,
	); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("invalid confirmed route err=%v", err)
	}
	if repo.bundle.Dispatch.Version != 4 {
		t.Fatal("invalid confirmed route mutated the repository")
	}
}
