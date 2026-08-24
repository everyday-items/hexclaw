package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestDingTalkPhotoCompositionDoesNotReturnImmediateAttachmentReply(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate composition test source")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	legacyImmediateReply := regexp.MustCompile(
		`(?s)if\s+reply\s*,\s*handled\s*,\s*err\s*:=\s*maybeHandleK12DingtalkPhoto\([^;]+;\s*handled\s*\{\s*return\s+reply\s*,\s*err`,
	)
	if legacyImmediateReply.Match(source) {
		t.Fatal("K12 DingTalk photo still returns one immediate adapter.Reply with its image attachment; the generic adapter will use ordinary reply Send/SendOTO instead of a durable two-part DeliveryBatch")
	}
}

type photoReplyBatchCall struct {
	agentName  string
	objectKind string
	objectID   string
	message    k12usecase.DeliveryMessage
	targets    []k12usecase.ResolvedDeliveryTarget
}

type photoReplyBatchPortFake struct {
	prepareCalls []photoReplyBatchCall
	queryCalls   []string
	byObjectID   map[string]k12.DeliveryBatch
	queryResult  k12.DeliveryBatch
}

func (f *photoReplyBatchPortFake) PrepareAndSendMessageBatchForTargets(
	_ context.Context,
	agentName, objectKind, objectID string,
	message k12usecase.DeliveryMessage,
	targets []k12usecase.ResolvedDeliveryTarget,
) (k12.DeliveryBatch, bool, error) {
	f.prepareCalls = append(f.prepareCalls, photoReplyBatchCall{
		agentName: agentName, objectKind: objectKind, objectID: objectID,
		message: message, targets: append([]k12usecase.ResolvedDeliveryTarget(nil), targets...),
	})
	if batch, ok := f.byObjectID[objectID]; ok {
		return batch, false, nil
	}
	target := targets[0]
	batch := k12.DeliveryBatch{
		BatchID: "batch-fixed", AgentName: agentName, ObjectKind: objectKind,
		ObjectID: objectID, Status: k12.DeliveryBatchDelivered,
		Receipts: []k12.DeliveryReceipt{
			{
				DeliveryID: "delivery-markdown", BatchID: "batch-fixed", BatchOrdinal: 1,
				PartKind: messagecontent.PartMarkdown, PartOrdinal: 1,
				BindingID: target.BindingID, Target: target.Target,
				Status: k12.DeliveryDelivered, ExternalMessageID: "dt-markdown-1",
			},
			{
				DeliveryID: "delivery-image", BatchID: "batch-fixed", BatchOrdinal: 2,
				PartKind: messagecontent.PartArtifact, PartMIME: message.Attachments[0].MIME,
				PartOrdinal: 2, BindingID: target.BindingID, Target: target.Target,
				Status: k12.DeliveryDelivered, ExternalMessageID: "dt-image-2",
			},
		},
	}
	f.byObjectID[objectID] = batch
	return batch, true, nil
}

func (f *photoReplyBatchPortFake) QueryDeliveryBatch(
	_ context.Context,
	agentName, batchID string,
) (k12.DeliveryBatch, error) {
	f.queryCalls = append(f.queryCalls, agentName+"\x00"+batchID)
	return f.queryResult, nil
}

func photoReplyCommandFixture() k12DingtalkPhotoReplyCommand {
	return k12DingtalkPhotoReplyCommand{
		AgentName:           "mingming",
		InboundReceiptID:    "inbound-receipt-1",
		FinalArtifactID:     "final-artifact-1",
		FinalArtifactDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Target: k12usecase.ResolvedDeliveryTarget{
			BindingID: "binding-1",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "robot-1", ChatID: "parent-1",
			},
		},
		Message: k12usecase.DeliveryMessage{
			Content: "## 作业批改\n\n14 道正确 / 2 道过程问题",
			Attachments: []k12usecase.DeliveryAttachment{{
				Name: "批改后的作业.png", MIME: "image/png", Data: []byte("immutable annotated image"),
			}},
		},
	}
}

func TestDingTalkPhotoReplyCoordinatorFreezesExactTwoPartsAndReplaysOriginalBatch(t *testing.T) {
	port := &photoReplyBatchPortFake{byObjectID: make(map[string]k12.DeliveryBatch)}
	coordinator := newK12DingtalkPhotoReplyCoordinator(port)
	command := photoReplyCommandFixture()

	first, created, err := coordinator.Deliver(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first final digest did not create its delivery batch")
	}
	replayed, created, err := coordinator.Deliver(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if created || replayed.BatchID != first.BatchID {
		t.Fatalf("same inbound/final digest changed batch identity: first=%+v replay=%+v", first, replayed)
	}
	if len(port.prepareCalls) != 2 || len(port.queryCalls) != 0 {
		t.Fatalf("initial/replay calls prepare=%d query=%d", len(port.prepareCalls), len(port.queryCalls))
	}
	call := port.prepareCalls[0]
	if call.agentName != command.AgentName || call.objectKind != k12DingtalkPhotoReplyObjectKind ||
		len(call.targets) != 1 || call.targets[0] != command.Target {
		t.Fatalf("reply did not freeze the inbound direct target exactly once: %+v", call)
	}
	if call.objectID == "" || call.objectID == command.InboundReceiptID ||
		call.objectID == command.FinalArtifactID || call.objectID == command.FinalArtifactDigest {
		t.Fatalf("delivery object identity exposed an internal ID or digest: %q", call.objectID)
	}
	if len(call.message.Attachments) != 1 || len(first.Receipts) != 2 {
		t.Fatalf("reply exact-set is not markdown + one annotated image: message=%+v batch=%+v", call.message, first)
	}
	markdown, image := first.Receipts[0], first.Receipts[1]
	if markdown.PartKind != messagecontent.PartMarkdown || markdown.PartOrdinal != 1 ||
		image.PartKind != messagecontent.PartArtifact || image.PartOrdinal != 2 ||
		image.PartMIME != "image/png" || markdown.ExternalMessageID == "" || image.ExternalMessageID == "" ||
		markdown.DeliveryID == image.DeliveryID {
		t.Fatalf("delivery exact-set/external IDs are incomplete: %+v", first.Receipts)
	}
}

func TestDingTalkPhotoReplyCoordinatorRestartQueriesBoundBatchOnly(t *testing.T) {
	want := k12.DeliveryBatch{
		BatchID: "batch-existing", AgentName: "mingming", Status: k12.DeliveryBatchDelivered,
		Receipts: []k12.DeliveryReceipt{
			{DeliveryID: "delivery-markdown", ExternalMessageID: "dt-markdown-1", Status: k12.DeliveryDelivered},
			{DeliveryID: "delivery-image", ExternalMessageID: "dt-image-2", Status: k12.DeliveryDelivered},
		},
	}
	port := &photoReplyBatchPortFake{
		byObjectID: make(map[string]k12.DeliveryBatch), queryResult: want,
	}
	coordinator := newK12DingtalkPhotoReplyCoordinator(port)
	command := photoReplyCommandFixture()
	command.DeliveryBatchID = want.BatchID
	// 重启路径故意不给最终产物字节；若实现重新准备或上传，就无法通过此测试。
	command.Message = k12usecase.DeliveryMessage{}

	got, created, err := coordinator.Deliver(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if created || got.BatchID != want.BatchID {
		t.Fatalf("restart query result=%+v created=%v", got, created)
	}
	if len(port.prepareCalls) != 0 || len(port.queryCalls) != 1 ||
		port.queryCalls[0] != "mingming\x00batch-existing" {
		t.Fatalf("restart must query only: prepare=%d query=%v", len(port.prepareCalls), port.queryCalls)
	}
	if got.Receipts[0].ExternalMessageID != "dt-markdown-1" ||
		got.Receipts[1].ExternalMessageID != "dt-image-2" {
		t.Fatalf("restart changed provider message IDs: %+v", got.Receipts)
	}
}
