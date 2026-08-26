package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/records"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type inboundPhotoRestartCheckpointFake struct {
	wantStage k12DingtalkPhotoRestartCheckpointStage
	err       error
	seen      []k12DingtalkPhotoRestartCheckpoint
}

func (f *inboundPhotoRestartCheckpointFake) Reach(
	_ context.Context,
	checkpoint k12DingtalkPhotoRestartCheckpoint,
) error {
	f.seen = append(f.seen, checkpoint)
	if checkpoint.Stage == f.wantStage {
		return f.err
	}
	return nil
}

func TestDingTalkPhotoRestartCheckpointStopsAtAdmissionBeforeImageTask(t *testing.T) {
	stop := errors.New("stop at admission checkpoint")
	checkpoint := &inboundPhotoRestartCheckpointFake{
		wantStage: k12DingtalkPhotoRestartCheckpointAdmissionCommitted,
		err:       stop,
	}
	bundle := inboundPhotoBundleFixture([]byte("source-photo"))
	images := &fakeK12ImageTaskFacade{}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: &inboundPhotoCoordinatorFake{bundle: bundle},
		ImageTasks: images, RestartCheckpoint: checkpoint,
	})

	done, err := runtime.advance(context.Background(), bundle)
	if done || !errors.Is(err, stop) {
		t.Fatalf("admission checkpoint = done %v, err %v", done, err)
	}
	if len(images.events) != 0 || len(checkpoint.seen) != 1 {
		t.Fatalf("admission checkpoint crossed ImageTask boundary: events=%v checkpoints=%+v",
			images.events, checkpoint.seen)
	}
	if got := checkpoint.seen[0]; got.Stage != checkpoint.wantStage ||
		got.Bundle.Dispatch.ProcessingStatus != k12usecase.InboundPhotoAdmitted ||
		got.Bundle.Dispatch.ImageTaskID != "" {
		t.Fatalf("admission checkpoint snapshot=%+v", got)
	}
}

func TestDingTalkPhotoRestartCheckpointStopsAfterModelBeforeArtifactBinding(t *testing.T) {
	stop := errors.New("stop after grading model")
	checkpoint := &inboundPhotoRestartCheckpointFake{
		wantStage: k12DingtalkPhotoRestartCheckpointGradingModelCompleted,
		err:       stop,
	}
	source := []byte("source-photo")
	bundle := inboundPhotoBundleFixture(source)
	bundle.Receipt.AgentName = "child-tutor"
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoImageTaskSubmitted
	bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteNewSubmission
	bundle.Dispatch.ImageTaskID = "dispatch-1"
	bundle.Dispatch.Version = 3
	artifact, annotated := finalArtifactFixture(source)
	artifact.AgentName = "child-tutor"
	artifact.AnnotatedAssetID = "asset://child-tutor/" + artifact.AnnotatedDigest + ".png"
	artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
	annotated.AssetID = artifact.AnnotatedAssetID
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	images := &fakeK12ImageTaskFacade{
		confirmCalls: 1, dispatchStatus: k12.ImageTaskStatusRouted,
		result: k12usecase.ImageTaskResult{FinalArtifact: &artifact},
	}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, ImageTasks: images,
		Artifacts:         finalArtifactReaderFake{artifact: artifact, asset: annotated},
		RestartCheckpoint: checkpoint,
	})

	done, err := runtime.advanceImageTask(context.Background(), bundle)
	if done || !errors.Is(err, stop) {
		t.Fatalf("grading checkpoint = done %v, err %v", done, err)
	}
	if coordinator.recordedFinalArtifactID != "" || len(checkpoint.seen) != 1 {
		t.Fatalf("grading checkpoint crossed artifact binding: bound=%q checkpoints=%+v",
			coordinator.recordedFinalArtifactID, checkpoint.seen)
	}
	if got := checkpoint.seen[0]; got.Stage != checkpoint.wantStage ||
		got.FinalArtifactID != artifact.ArtifactID ||
		got.Bundle.Dispatch.ProcessingStatus != k12usecase.InboundPhotoImageTaskSubmitted ||
		got.Bundle.Dispatch.FinalArtifactID != "" {
		t.Fatalf("grading checkpoint snapshot=%+v", got)
	}
}

func TestDingTalkPhotoRestartCheckpointStopsAfterPracticeReturnModelBeforeArtifactBinding(t *testing.T) {
	stop := errors.New("stop after practice-return grading model")
	checkpoint := &inboundPhotoRestartCheckpointFake{
		wantStage: k12DingtalkPhotoRestartCheckpointGradingModelCompleted,
		err:       stop,
	}
	source := []byte("source-photo")
	bundle := inboundPhotoBundleFixture(source)
	bundle.Receipt.AgentName = "child-tutor"
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoImageTaskSubmitted
	bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteRegrade
	bundle.Dispatch.ImageTaskID = "dispatch-1"
	bundle.Dispatch.Version = 3
	artifact, annotated := finalArtifactFixture(source)
	artifact.AgentName = "child-tutor"
	artifact.ArtifactID = "practice-final-1"
	artifact.AnnotatedAssetID = "asset://child-tutor/" + artifact.AnnotatedDigest + ".png"
	artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
	annotated.AssetID = artifact.AnnotatedAssetID
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	returns := &inboundPhotoPracticeReturnFake{
		bound: true,
		state: k12InboundPhotoPracticeReturnState{
			PracticeSetID: "set-1", ReturnID: "return-1", FinalArtifactID: artifact.ArtifactID,
		},
	}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, PracticeReturns: returns,
		Artifacts:         finalArtifactReaderFake{artifact: artifact, asset: annotated},
		RestartCheckpoint: checkpoint,
	})

	done, err := runtime.advancePracticeReturn(
		context.Background(), bundle, inboundPhotoRoutingView(""), "set-1",
	)
	if done || !errors.Is(err, stop) {
		t.Fatalf("practice-return grading checkpoint = done %v, err %v", done, err)
	}
	if coordinator.recordedFinalArtifactID != "" || len(checkpoint.seen) != 1 {
		t.Fatalf("practice-return checkpoint crossed artifact binding: bound=%q checkpoints=%+v",
			coordinator.recordedFinalArtifactID, checkpoint.seen)
	}
	if got := checkpoint.seen[0]; got.Stage != checkpoint.wantStage ||
		got.FinalArtifactID != artifact.ArtifactID ||
		got.Bundle.Dispatch.FinalArtifactID != "" {
		t.Fatalf("practice-return grading checkpoint snapshot=%+v", got)
	}
}

func TestDingTalkPhotoRestartCheckpointStopsBeforeDeliverySend(t *testing.T) {
	stop := errors.New("stop before delivery")
	checkpoint := &inboundPhotoRestartCheckpointFake{
		wantStage: k12DingtalkPhotoRestartCheckpointBeforeDeliverySend,
		err:       stop,
	}
	source := []byte("source-photo")
	bundle := inboundPhotoBundleFixture(source)
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoFinalArtifactReady
	bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteNewSubmission
	bundle.Dispatch.FinalArtifactID = "final-1"
	bundle.Dispatch.ReplyStatus = k12usecase.InboundPhotoReplyReady
	bundle.Dispatch.Version = 4
	artifact, annotated := finalArtifactFixture(source)
	batches := &finalReplyBatchFake{}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: &inboundPhotoCoordinatorFake{bundle: bundle},
		Artifacts:    finalArtifactReaderFake{artifact: artifact, asset: annotated},
		ReplyBatches: batches, RestartCheckpoint: checkpoint,
	})

	done, err := runtime.advance(context.Background(), bundle)
	if done || !errors.Is(err, stop) {
		t.Fatalf("before-delivery checkpoint = done %v, err %v", done, err)
	}
	if batches.prepareCalls != 0 || batches.queryCalls != 0 || len(checkpoint.seen) != 1 {
		t.Fatalf("before-delivery checkpoint crossed send boundary: prepare=%d query=%d checkpoints=%+v",
			batches.prepareCalls, batches.queryCalls, checkpoint.seen)
	}
}

func TestDingTalkPhotoRestartCheckpointStopsAfterDeliveredCAS(t *testing.T) {
	stop := errors.New("stop after delivery")
	checkpoint := &inboundPhotoRestartCheckpointFake{
		wantStage: k12DingtalkPhotoRestartCheckpointAfterDeliverySend,
		err:       stop,
	}
	source := []byte("source-photo")
	bundle := inboundPhotoBundleFixture(source)
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoFinalArtifactReady
	bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteNewSubmission
	bundle.Dispatch.FinalArtifactID = "final-1"
	bundle.Dispatch.ReplyStatus = k12usecase.InboundPhotoReplyBound
	bundle.Dispatch.DeliveryBatchID = "batch-1"
	bundle.Dispatch.Version = 5
	target := k12.DeliveryTarget{Platform: "dingtalk", InstanceID: "family-bot", ChatID: "parent-1"}
	batches := &finalReplyBatchFake{existing: k12.DeliveryBatch{
		BatchID: "batch-1", AgentName: "student", Status: k12.DeliveryBatchDelivered,
		Receipts: []k12.DeliveryReceipt{
			{DeliveryID: "delivery-1", BindingID: "agent-rule:1", Target: target,
				PartKind: messagecontent.PartMarkdown, PartOrdinal: 1},
			{DeliveryID: "delivery-2", BindingID: "agent-rule:1", Target: target,
				PartKind: messagecontent.PartArtifact, PartOrdinal: 2, PartMIME: "image/png"},
		},
	}}
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator, ReplyBatches: batches,
		RestartCheckpoint: checkpoint,
	})

	done, err := runtime.advanceFinalReply(context.Background(), bundle)
	if done || !errors.Is(err, stop) {
		t.Fatalf("after-delivery checkpoint = done %v, err %v", done, err)
	}
	if !coordinator.completed || batches.queryCalls != 1 || len(checkpoint.seen) != 1 {
		t.Fatalf("after-delivery checkpoint did not follow delivered CAS: completed=%v query=%d checkpoints=%+v",
			coordinator.completed, batches.queryCalls, checkpoint.seen)
	}
	if got := checkpoint.seen[0]; got.Stage != checkpoint.wantStage ||
		got.Bundle.Dispatch.ReplyStatus != k12usecase.InboundPhotoReplyDelivered ||
		got.Bundle.Dispatch.DeliveryBatchID != "batch-1" {
		t.Fatalf("after-delivery checkpoint snapshot=%+v", got)
	}
}

type freshDeliveryRestartCheckpointFake struct {
	*finalReplyBatchFake
}

func (*freshDeliveryRestartCheckpointFake) GetDeliveryBatchForMessageIdentity(
	context.Context, string, string, string, string,
	[]k12usecase.DeliveryAttachmentIdentity,
) (k12.DeliveryBatch, error) {
	return k12.DeliveryBatch{}, records.ErrNotFound
}

func TestDingTalkPhotoRestartCheckpointStopsAfterFreshDeliveryCAS(t *testing.T) {
	stop := errors.New("stop after fresh delivery")
	checkpoint := &inboundPhotoRestartCheckpointFake{
		wantStage: k12DingtalkPhotoRestartCheckpointAfterDeliverySend,
		err:       stop,
	}
	source := []byte("source-photo")
	bundle := inboundPhotoBundleFixture(source)
	bundle.Dispatch.ProcessingStatus = k12usecase.InboundPhotoFinalArtifactReady
	bundle.Dispatch.RoutingDecision = k12usecase.InboundPhotoRouteNewSubmission
	bundle.Dispatch.FinalArtifactID = "final-1"
	bundle.Dispatch.ReplyStatus = k12usecase.InboundPhotoReplyReady
	bundle.Dispatch.Version = 4
	artifact, annotated := finalArtifactFixture(source)
	target := k12.DeliveryTarget{
		Platform: "dingtalk", InstanceID: "family-bot", ChatID: "parent-1",
	}
	batches := &freshDeliveryRestartCheckpointFake{finalReplyBatchFake: &finalReplyBatchFake{
		existing: k12.DeliveryBatch{
			BatchID: "batch-1", AgentName: "student", Status: k12.DeliveryBatchDelivered,
			Receipts: []k12.DeliveryReceipt{
				{DeliveryID: "delivery-1", BindingID: "agent-rule:1", Target: target,
					PartKind: messagecontent.PartMarkdown, PartOrdinal: 1},
				{DeliveryID: "delivery-2", BindingID: "agent-rule:1", Target: target,
					PartKind: messagecontent.PartArtifact, PartOrdinal: 2, PartMIME: "image/png"},
			},
		},
	}}
	coordinator := &inboundPhotoCoordinatorFake{bundle: bundle}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(), Inbound: coordinator,
		Artifacts:    finalArtifactReaderFake{artifact: artifact, asset: annotated},
		ReplyBatches: batches, RestartCheckpoint: checkpoint,
	})

	done, err := runtime.advanceFinalReply(context.Background(), bundle)
	if done || !errors.Is(err, stop) {
		t.Fatalf("fresh after-delivery checkpoint = done %v, err %v", done, err)
	}
	if batches.prepareCalls != 1 || !coordinator.completed ||
		coordinator.boundBatchID != "batch-1" || len(checkpoint.seen) != 1 {
		t.Fatalf("fresh after-delivery checkpoint boundary: prepare=%d completed=%v bound=%q checkpoints=%+v",
			batches.prepareCalls, coordinator.completed, coordinator.boundBatchID, checkpoint.seen)
	}
	if got := checkpoint.seen[0]; got.Stage != checkpoint.wantStage ||
		got.Bundle.Dispatch.ReplyStatus != k12usecase.InboundPhotoReplyDelivered ||
		got.Bundle.Dispatch.DeliveryBatchID != "batch-1" {
		t.Fatalf("fresh after-delivery checkpoint snapshot=%+v", got)
	}
}

func TestDingTalkPhotoRestartCheckpointEnvironmentWritesPrivateSanitizedReceiptAndWaits(t *testing.T) {
	bundle := inboundPhotoBundleFixture([]byte("private-child-photo"))
	identityDigest := k12DingtalkPhotoRestartCheckpointIdentityDigest(bundle.Receipt.Identity)
	receiptPath := filepath.Join(t.TempDir(), "reached.json")
	t.Setenv(k12DingtalkPhotoRestartCheckpointEnableEnv, "1")
	t.Setenv(k12DingtalkPhotoRestartCheckpointStageEnv,
		string(k12DingtalkPhotoRestartCheckpointAdmissionCommitted))
	t.Setenv(k12DingtalkPhotoRestartCheckpointIdentityEnv, identityDigest)
	t.Setenv(k12DingtalkPhotoRestartCheckpointReceiptEnv, receiptPath)
	checkpoint := newK12DingtalkPhotoRestartCheckpointFromEnvironment()
	if checkpoint == nil {
		t.Fatal("explicit restart checkpoint environment produced nil port")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- checkpoint.Reach(ctx, k12DingtalkPhotoRestartCheckpoint{
			Stage: k12DingtalkPhotoRestartCheckpointAdmissionCommitted, Bundle: bundle,
		})
	}()
	for attempts := 0; attempts < 100; attempts++ {
		if _, err := os.Stat(receiptPath); err == nil {
			break
		}
		if attempts == 99 {
			t.Fatal("restart checkpoint did not publish reached receipt")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("restart checkpoint returned before cancellation: %v", err)
	default:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("restart checkpoint cancellation err=%v", err)
	}
	info, err := os.Stat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("reached receipt mode=%o want=600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{
		bundle.Receipt.Identity.InstanceID, bundle.Receipt.Identity.ChatID,
		bundle.Receipt.Identity.ProviderMessageID, bundle.Receipt.ReceiptID,
		bundle.Dispatch.DispatchID, string(bundle.Asset.Bytes),
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("reached receipt leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, identityDigest) ||
		!strings.Contains(text, string(k12DingtalkPhotoRestartCheckpointAdmissionCommitted)) {
		t.Fatalf("reached receipt missed stage/identity digest: %s", text)
	}
}

func TestDingTalkPhotoRestartCheckpointEnvironmentIsDisabledByDefault(t *testing.T) {
	t.Setenv(k12DingtalkPhotoRestartCheckpointEnableEnv, "")
	t.Setenv(k12DingtalkPhotoRestartCheckpointStageEnv,
		string(k12DingtalkPhotoRestartCheckpointAdmissionCommitted))
	t.Setenv(k12DingtalkPhotoRestartCheckpointIdentityEnv, strings.Repeat("a", 64))
	receiptPath := filepath.Join(t.TempDir(), "must-not-exist.json")
	t.Setenv(k12DingtalkPhotoRestartCheckpointReceiptEnv, receiptPath)

	if checkpoint := newK12DingtalkPhotoRestartCheckpointFromEnvironment(); checkpoint != nil {
		t.Fatalf("disabled restart checkpoint port=%T want nil", checkpoint)
	}
	runtime := newK12DingtalkPhotoInboundRuntime(k12DingtalkPhotoInboundRuntimeConfig{
		BaseContext: context.Background(),
	})
	if runtime.restartCheckpoint != nil {
		t.Fatalf("default runtime restart checkpoint=%T want nil", runtime.restartCheckpoint)
	}
	if err := runtime.reachRestartCheckpoint(
		context.Background(), k12DingtalkPhotoRestartCheckpointAdmissionCommitted,
		inboundPhotoBundleFixture([]byte("source-photo")), "",
	); err != nil {
		t.Fatalf("disabled restart checkpoint blocked or failed: %v", err)
	}
	if _, err := os.Stat(receiptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled restart checkpoint created receipt: %v", err)
	}
}
