package usecase_test

import (
	"context"
	"testing"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestInboundPhotoCoordinatorRecordsOnlyInternalTerminalFacts(t *testing.T) {
	repo := &inboundPhotoRepositoryFake{bundle: usecase.InboundPhotoBundle{
		Receipt: usecase.InboundPhotoReceipt{ReceiptID: "receipt-terminal", AgentName: "mingming"},
		Dispatch: usecase.InboundPhotoDispatch{
			DispatchID: "dispatch-terminal", ReceiptID: "receipt-terminal",
			InboundPhotoDispatchState: usecase.InboundPhotoDispatchState{
				ProcessingStatus:   k12storage.InboundPhotoImageTaskSubmitted,
				RoutingDecision:    k12storage.InboundPhotoRouteNewSubmission,
				ConfirmationStatus: k12storage.InboundPhotoConfirmationNotRequired,
				ImageTaskID:        "image-task-1",
				ReplyStatus:        k12storage.InboundPhotoReplyPending,
			},
			Version: 2,
		},
	}}
	dispatch, err := usecase.NewInboundPhotoCoordinator(repo).FailTerminal(
		context.Background(), "mingming", "receipt-terminal", 2,
		usecase.InboundPhotoTerminalStageGrading, "grading_failed_terminal",
	)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.TerminalStatus != usecase.InboundPhotoTerminalFailed ||
		dispatch.TerminalStage != usecase.InboundPhotoTerminalStageGrading ||
		dispatch.FailureKind != "grading_failed_terminal" || dispatch.Version != 3 {
		t.Fatalf("terminal coordinator transition=%+v", dispatch)
	}
}
