package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type terminalProjectionClassifier struct{}

func (terminalProjectionClassifier) ClassifyImageTask(
	context.Context,
	k12usecase.ImageTaskClassificationInput,
) (k12usecase.ImageTaskClassification, error) {
	return k12usecase.ImageTaskClassification{}, nil
}

func (f *inboundPhotoCoordinatorFake) FailTerminal(
	_ context.Context, _, _ string, _ int64,
	stage k12usecase.InboundPhotoTerminalStage, failureKind string,
) (k12usecase.InboundPhotoDispatch, error) {
	f.terminalCalls++
	f.bundle.Dispatch.TerminalStatus = k12usecase.InboundPhotoTerminalFailed
	f.bundle.Dispatch.TerminalStage = stage
	f.bundle.Dispatch.FailureKind = failureKind
	f.bundle.Dispatch.Version++
	return f.bundle.Dispatch, nil
}

type terminalImageTaskFacade struct {
	*fakeK12ImageTaskFacade
	view k12usecase.ImageTaskView
}

func (f *terminalImageTaskFacade) Get(
	context.Context, string, string,
) (k12usecase.ImageTaskView, error) {
	return f.view, nil
}

func terminalImageTaskBundle() k12usecase.InboundPhotoBundle {
	bundle := inboundPhotoBundleFixture([]byte("source-photo"))
	bundle.Receipt.AgentName = "child-tutor"
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoImageTaskSubmitted
	bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteNewSubmission
	bundle.Dispatch.ImageTaskID = "dispatch-1"
	bundle.Dispatch.Version = 2
	return bundle
}

func TestDingTalkPhotoWorkerTerminatesOnlyNonRetryableImageTaskFailure(t *testing.T) {
	bundle := terminalImageTaskBundle()
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	images := &terminalImageTaskFacade{
		fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{},
		view: k12usecase.ImageTaskView{Dispatch: k12.ImageTaskDispatch{
			DispatchID: "dispatch-1", AgentName: "child-tutor",
			Status: k12.ImageTaskStatusFailed, RetrySafe: false,
			FailureKind: "model_capability_unverified",
		}},
	}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, ImageTasks: images,
	})
	done, err := runtime.advanceImageTask(context.Background(), bundle)
	if err != nil || !done {
		t.Fatalf("permanent image failure = done %v, err %v", done, err)
	}
	if coordinator.terminalCalls != 1 ||
		coordinator.bundle.Dispatch.TerminalStage != k12usecase.InboundPhotoTerminalStageImageTask ||
		coordinator.bundle.Dispatch.FailureKind != "model_capability_unverified" {
		t.Fatalf("terminal image facts=%+v calls=%d", coordinator.bundle.Dispatch, coordinator.terminalCalls)
	}
}

func TestDingTalkPhotoWorkerDoesNotMisclassifyRetryOrReconciliationStates(t *testing.T) {
	tests := []struct {
		name string
		view k12usecase.ImageTaskView
	}{
		{
			name: "retry safe image task failure",
			view: k12usecase.ImageTaskView{Dispatch: k12.ImageTaskDispatch{
				DispatchID: "dispatch-1", AgentName: "child-tutor",
				Status: k12.ImageTaskStatusFailed, RetrySafe: true,
				FailureKind: "provider_timeout",
			}},
		},
		{
			name: "image task outcome unknown",
			view: k12usecase.ImageTaskView{
				Dispatch: k12.ImageTaskDispatch{
					DispatchID: "dispatch-1", AgentName: "child-tutor",
					Status: k12.ImageTaskStatusFailed, RetrySafe: false,
					FailureKind: "classification_outcome_unknown",
				},
				ClassificationInvocationStatus: k12.ImageTaskInvocationOutcomeUnknown,
			},
		},
		{
			name: "grading failed retryable",
			view: k12usecase.ImageTaskView{
				Dispatch: k12.ImageTaskDispatch{
					DispatchID: "dispatch-1", AgentName: "child-tutor", Status: k12.ImageTaskStatusRouted,
				},
				HomeworkProjection: &k12usecase.ImageTaskHomeworkProjection{Stage: k12.GradingStageFailedRetryable},
			},
		},
		{
			name: "grading outcome unknown",
			view: k12usecase.ImageTaskView{
				Dispatch: k12.ImageTaskDispatch{
					DispatchID: "dispatch-1", AgentName: "child-tutor", Status: k12.ImageTaskStatusRouted,
				},
				HomeworkProjection: &k12usecase.ImageTaskHomeworkProjection{Stage: k12.GradingStageOutcomeUnknown},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := terminalImageTaskBundle()
			coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
			images := &terminalImageTaskFacade{
				fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{}, view: tt.view,
			}
			runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
				BaseContext: context.Background(), Inbound: coordinator, ImageTasks: images,
			})
			_, _ = runtime.advanceImageTask(context.Background(), bundle)
			if coordinator.terminalCalls != 0 {
				t.Fatalf("retry/reconciliation state was made terminal: %+v", coordinator.bundle.Dispatch)
			}
		})
	}
}

func TestDingTalkPhotoWorkerReadsPersistedClassificationOutcomeUnknownBeforeTerminalDecision(
	t *testing.T,
) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "classification-outcome-unknown.sqlite")
	db, err := sql.Open(
		"sqlite",
		dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO agents(name,display_name) VALUES(?,?)`,
		"child-tutor", "Child Tutor",
	); err != nil {
		t.Fatal(err)
	}

	records := k12storage.NewStore(db, nil)
	assetRef := "asset://child-tutor/" + strings.Repeat("a", 64) + ".png"
	coordinator := &k12usecase.ImageTaskCoordinator{
		Records:    records,
		Classifier: terminalProjectionClassifier{},
		ResolveRoute: func(
			request k12.ImageTaskRouteSnapshot,
		) (k12.ImageTaskRouteSnapshot, error) {
			request.Route = request.Provider + "/" + request.Model
			request.Capability = "vision"
			request.SelectionSource = "explicit"
			request.PolicyVersion = "image-task-routing-v1"
			request.PromptVersion = "image-task-classifier-v1"
			request.TimeoutMS = 120_000
			return request, nil
		},
		ReadAsset: func(agentName, ref string) ([]byte, error) {
			if agentName != "child-tutor" || ref != assetRef {
				t.Fatalf("unexpected asset identity agent=%q ref=%q", agentName, ref)
			}
			return []byte("persisted classification source"), nil
		},
		Now: func() int64 { return 1_000 },
		NewID: func(kind string) string {
			switch kind {
			case "dispatch":
				return "dispatch-1"
			case "classification":
				return "classification-1"
			default:
				return "unused-" + kind
			}
		},
	}
	created, first, err := coordinator.Create(ctx, k12usecase.CreateImageTaskInput{
		AgentName: "child-tutor", LearnerID: "child-tutor",
		SourceKind: k12.ImageTaskSourceIM, SourceRef: "dingtalk-inbound:receipt-1",
		SourceSessionID: "parent-chat", SourceAssetRefs: []string{assetRef},
		MessageIntent: "grade homework", AttemptGeneration: 1,
		RouteRequest: k12.ImageTaskRouteSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", SelectionSource: "explicit",
		},
	})
	if err != nil || !first {
		t.Fatalf("create image task: created=%v err=%v", first, err)
	}
	invocation, claimed, err := records.ClaimImageTaskInvocationSend(
		ctx, "child-tutor", created.Dispatch.ClassificationInvocationID,
		"provider-request-1", 1_001,
	)
	if err != nil || !claimed {
		t.Fatalf("claim classification invocation: claimed=%v err=%v", claimed, err)
	}
	if err := records.FailImageTaskInvocation(
		ctx, "child-tutor", invocation.InvocationID,
		"classification_outcome_unknown", true, false,
	); err != nil {
		t.Fatal(err)
	}

	projected, err := coordinator.Get(ctx, "child-tutor", created.Dispatch.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Dispatch.Status != k12.ImageTaskStatusFailed ||
		projected.Dispatch.RetrySafe ||
		projected.ClassificationInvocationStatus != k12.ImageTaskInvocationOutcomeUnknown {
		t.Fatalf("persisted outcome_unknown projection=%+v", projected)
	}

	bundle := terminalImageTaskBundle()
	terminal := &inboundPhotoCoordinatorFake{bundle: bundle}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: ctx, Inbound: terminal, ImageTasks: coordinator,
	})
	done, advanceErr := runtime.advanceImageTask(ctx, bundle)
	if done || advanceErr == nil {
		t.Fatalf("outcome_unknown worker state: done=%v err=%v", done, advanceErr)
	}
	if terminal.terminalCalls != 0 {
		t.Fatalf("persisted outcome_unknown was made terminal: %+v", terminal.bundle.Dispatch)
	}
}

func TestDingTalkPhotoWorkerTerminatesFailedTerminalGrading(t *testing.T) {
	bundle := terminalImageTaskBundle()
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	images := &terminalImageTaskFacade{
		fakeK12ImageTaskFacade: &fakeK12ImageTaskFacade{},
		view: k12usecase.ImageTaskView{
			Dispatch: k12.ImageTaskDispatch{
				DispatchID: "dispatch-1", AgentName: "child-tutor", Status: k12.ImageTaskStatusRouted,
			},
			HomeworkProjection: &k12usecase.ImageTaskHomeworkProjection{Stage: k12.GradingStageFailedTerminal},
		},
	}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, ImageTasks: images,
	})
	done, err := runtime.advanceImageTask(context.Background(), bundle)
	if err != nil || !done {
		t.Fatalf("terminal grading failure = done %v, err %v", done, err)
	}
	if coordinator.terminalCalls != 1 ||
		coordinator.bundle.Dispatch.TerminalStage != k12usecase.InboundPhotoTerminalStageGrading ||
		coordinator.bundle.Dispatch.FailureKind != "grading_failed_terminal" {
		t.Fatalf("terminal grading facts=%+v calls=%d", coordinator.bundle.Dispatch, coordinator.terminalCalls)
	}
}

func TestDingTalkPhotoWorkerTerminatesOnlyFailedBoundDeliveryBatch(t *testing.T) {
	for _, status := range []k12.DeliveryBatchStatus{
		k12.DeliveryBatchFailed, k12.DeliveryBatchPartialFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			bundle := inboundPhotoBundleFixture([]byte("source-photo"))
			bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoFinalArtifactReady
			bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteNewSubmission
			bundle.Dispatch.ImageTaskID = "dispatch-1"
			bundle.Dispatch.FinalArtifactID = "final-1"
			bundle.Dispatch.ReplyStatus = k12usecase.InboundPhotoReplyBound
			bundle.Dispatch.DeliveryBatchID = "batch-1"
			bundle.Dispatch.Version = 5
			coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
			target := inboundPhotoFrozenTarget(bundle).Target
			batches := &finalReplyBatchFake{existing: k12.DeliveryBatch{
				BatchID: "batch-1", AgentName: bundle.Receipt.AgentName, Status: status,
				Receipts: []k12.DeliveryReceipt{
					{DeliveryID: "delivery-1", BindingID: bundle.Receipt.BindingID, Target: target,
						PartKind: messagecontent.PartMarkdown, PartOrdinal: 1},
					{DeliveryID: "delivery-2", BindingID: bundle.Receipt.BindingID, Target: target,
						PartKind: messagecontent.PartArtifact, PartOrdinal: 2, PartMIME: "image/png"},
				},
			}}
			runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
				BaseContext: context.Background(), Inbound: coordinator, ReplyBatches: batches,
			})
			done, err := runtime.completeBoundReply(context.Background(), bundle)
			if err != nil || !done {
				t.Fatalf("terminal delivery = done %v, err %v", done, err)
			}
			wantKind := "delivery_batch_failed"
			if status == k12.DeliveryBatchPartialFailed {
				wantKind = "delivery_batch_partial_failed"
			}
			if coordinator.terminalCalls != 1 ||
				coordinator.bundle.Dispatch.TerminalStage != k12usecase.InboundPhotoTerminalStageDelivery ||
				coordinator.bundle.Dispatch.FailureKind != wantKind {
				t.Fatalf("terminal delivery facts=%+v calls=%d", coordinator.bundle.Dispatch, coordinator.terminalCalls)
			}
		})
	}
}

func TestDingTalkPhotoWorkerDoesNotTerminateOutcomeUnknownDeliveryBatch(t *testing.T) {
	bundle := inboundPhotoBundleFixture([]byte("source-photo"))
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoFinalArtifactReady
	bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteNewSubmission
	bundle.Dispatch.ImageTaskID = "dispatch-1"
	bundle.Dispatch.FinalArtifactID = "final-1"
	bundle.Dispatch.ReplyStatus = k12usecase.InboundPhotoReplyBound
	bundle.Dispatch.DeliveryBatchID = "batch-1"
	bundle.Dispatch.Version = 5
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	target := inboundPhotoFrozenTarget(bundle).Target
	batches := &finalReplyBatchFake{existing: k12.DeliveryBatch{
		BatchID: "batch-1", AgentName: bundle.Receipt.AgentName,
		Status: k12.DeliveryBatchOutcomeUnknown,
		Receipts: []k12.DeliveryReceipt{
			{DeliveryID: "delivery-1", BindingID: bundle.Receipt.BindingID, Target: target,
				PartKind: messagecontent.PartMarkdown, PartOrdinal: 1},
			{DeliveryID: "delivery-2", BindingID: bundle.Receipt.BindingID, Target: target,
				PartKind: messagecontent.PartArtifact, PartOrdinal: 2, PartMIME: "image/png"},
		},
	}}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, ReplyBatches: batches,
	})
	done, err := runtime.completeBoundReply(context.Background(), bundle)
	if err != nil || done {
		t.Fatalf("outcome unknown delivery = done %v, err %v", done, err)
	}
	if coordinator.terminalCalls != 0 {
		t.Fatalf("outcome unknown delivery was made terminal: %+v", coordinator.bundle.Dispatch)
	}
}

func TestDingTalkPhotoWorkerShortCircuitsPersistedTerminalReceipt(t *testing.T) {
	bundle := terminalImageTaskBundle()
	bundle.Dispatch.TerminalStatus = k12usecase.InboundPhotoTerminalFailed
	bundle.Dispatch.TerminalStage = k12usecase.InboundPhotoTerminalStageImageTask
	bundle.Dispatch.FailureKind = "model_capability_unverified"
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	images := &fakeK12ImageTaskFacade{}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, ImageTasks: images,
	})
	done, err := runtime.advance(context.Background(), bundle)
	if err != nil || !done {
		t.Fatalf("persisted terminal = done %v, err %v", done, err)
	}
	if images.getCalls != 0 || images.startCalls != 0 || coordinator.terminalCalls != 0 {
		t.Fatalf("persisted terminal crossed worker boundary: get=%d start=%d terminal=%d",
			images.getCalls, images.startCalls, coordinator.terminalCalls)
	}
}
