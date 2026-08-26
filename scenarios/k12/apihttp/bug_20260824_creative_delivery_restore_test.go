package apihttp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type creativeDeliveryRestoreTransport struct {
	httpBatchTransport
	resolveCalls int
	prepareCalls int
	sendCalls    int
	queryCalls   int
}

func (f *creativeDeliveryRestoreTransport) ResolveTextTargets(
	context.Context,
	string,
) ([]usecase.ResolvedDeliveryTarget, error) {
	f.resolveCalls++
	return append([]usecase.ResolvedDeliveryTarget(nil), f.targets...), nil
}

func (f *creativeDeliveryRestoreTransport) PrepareTextForTargets(
	ctx context.Context,
	content string,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	f.prepareCalls++
	return f.httpBatchTransport.PrepareTextForTargets(ctx, content, targets)
}

func (f *creativeDeliveryRestoreTransport) PrepareMessageForTargets(
	ctx context.Context,
	message usecase.DeliveryMessage,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	f.prepareCalls++
	return f.httpBatchTransport.PrepareMessageForTargets(ctx, message, targets)
}

func (f *creativeDeliveryRestoreTransport) SendPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.sendCalls++
	f.sends = append(f.sends, receipt)
	return usecase.DeliveryTransportAck{
		Status: k12.DeliveryDelivered, ExternalMessageID: "unexpected-restore-send",
	}, nil
}

func (f *creativeDeliveryRestoreTransport) QueryPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.queryCalls++
	f.queries = append(f.queries, receipt)
	return usecase.DeliveryTransportAck{
		Status: k12.DeliveryDelivered, ExternalMessageID: "unexpected-restore-query",
	}, nil
}

func restoreDeliveryDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func restoreCreativeMessageDigest(
	t *testing.T,
	view usecase.CreativeWorkView,
) string {
	t.Helper()
	if view.GenerationState.Initial == nil || view.GenerationState.Latest == nil ||
		view.GenerationState.Latest.Feedback == nil {
		t.Fatal("ready creative work must expose its source and latest feedback")
	}
	source := view.GenerationState.Initial.Source
	displayName := strings.TrimSpace(source.DisplayName)
	if displayName == "" {
		displayName = view.Fields.DisplayName
	}
	workTitle := strings.TrimSpace(source.WorkTitle)
	if workTitle == "" {
		workTitle = view.Fields.WorkTitle
	}
	parts := []string{displayName}
	if workTitle != "" && workTitle != displayName {
		parts = append(parts, workTitle)
	}
	if content := strings.TrimSpace(source.ContentMarkdown); content != "" {
		parts = append(parts, content)
	}
	parts = append(parts, view.GenerationState.Latest.Feedback.ProjectionMarkdown)
	content := strings.Join(parts, "\n\n")
	if strings.TrimSpace(source.SourceAssetID) == "" {
		return restoreDeliveryDigest(content)
	}

	assetAgent, file, err := assetstore.Parse(source.SourceAssetID)
	if err != nil || assetAgent != view.Record.AgentName {
		t.Fatalf("parse current creative asset identity: agent=%q err=%v", assetAgent, err)
	}
	dot := strings.LastIndexByte(file, '.')
	if dot <= 0 || file[dot:] != ".png" {
		t.Fatalf("unexpected creative asset identity %q", source.SourceAssetID)
	}
	identityPayload, err := json.Marshal(struct {
		Content     string `json:"content"`
		Attachments []struct {
			Name   string `json:"name"`
			MIME   string `json:"mime"`
			Digest string `json:"digest"`
		} `json:"attachments"`
	}{
		Content: content,
		Attachments: []struct {
			Name   string `json:"name"`
			MIME   string `json:"mime"`
			Digest string `json:"digest"`
		}{{
			Name: displayName + ".png", MIME: "image/png",
			Digest: "sha256:" + file[:dot],
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return restoreDeliveryDigest(string(identityPayload))
}

func seedPendingCreativeDeliveryBatch(
	t *testing.T,
	runtime *assembly.K12,
	workID, messageDigest string,
) string {
	t.Helper()
	batchID := "restore-batch-" + workID
	payload := `{"markdown":"frozen creative work"}`
	payloadDigest := restoreDeliveryDigest(payload)
	batchDedupe := restoreDeliveryDigest(strings.Join(
		[]string{"mingming", "creative_work", workID, messageDigest}, "\x00",
	))
	childDedupe := restoreDeliveryDigest(strings.Join(
		[]string{"mingming", "creative_work", workID, "agent-rule:restore", payloadDigest}, "\x00",
	))
	batch, created, err := runtime.Records.PrepareDeliveryBatch(
		context.Background(),
		k12.DeliveryBatch{
			BatchID: batchID, AgentName: "mingming", ObjectKind: "creative_work",
			ObjectID: workID, DedupeKey: batchDedupe, ContentDigest: messageDigest,
			Receipts: []k12.DeliveryReceipt{{
				DeliveryID: "restore-delivery-" + workID,
				BindingID:  "agent-rule:restore",
				Target: k12.DeliveryTarget{
					Platform: "dingtalk", InstanceID: "bound-bot", ChatID: "bound-parent",
				},
				DedupeKey: childDedupe, PayloadDigest: payloadDigest,
				PayloadJSON: payload, RenderJSON: `{}`,
			}},
		},
	)
	if err != nil || !created || batch.Status != k12.DeliveryBatchPending {
		t.Fatalf("seed pending creative batch: created=%v status=%q err=%v", created, batch.Status, err)
	}
	return batchID
}

func completeChangedCreativeFeedback(
	t *testing.T,
	runtime *assembly.K12,
	workID string,
) {
	t.Helper()
	generation, created, err := runtime.Records.PrepareWorkFeedbackGeneration(
		context.Background(), "mingming", workID,
		"creative-restore-feedback-change", "request:creative-restore-feedback-change",
	)
	if err != nil || !created {
		t.Fatalf("prepare changed creative feedback: created=%v err=%v", created, err)
	}
	feedback := k12.WorkFeedback{
		FeedbackID: "feedback-restore-changed", VersionID: generation.GenerationID,
		FeedbackType: k12.WorkTypeArt,
		EvidenceRefs: []string{"asset-ref:ready-source"},
		Observations: []k12.WorkFeedbackObservation{{
			Dimension: "visible_detail", Evidence: "画面中新增加的叶片细节清晰可见",
		}},
		SourceSnapshot: k12.WorkFeedbackSourceSnapshot{
			Source: k12.FeedbackSourceAI, MethodRef: "creative-restore@2", Capability: "text+vision",
		},
		Limitations: "仅依据当前版本提交的可见画面进行观察，不评价能力高低",
		Suggestions: []string{"下次可以继续补充远处景物的层次"},
	}
	feedback.ProjectionMarkdown = k12.ProjectWorkFeedbackMarkdown(feedback)
	if _, err := runtime.Records.CompleteWorkFeedbackGeneration(
		context.Background(), "mingming", generation.GenerationID, feedback,
	); err != nil {
		t.Fatalf("complete changed creative feedback: %v", err)
	}
}

func creativeWorkInList(
	t *testing.T,
	body map[string]any,
	workID string,
) map[string]any {
	t.Helper()
	items, _ := body["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["work_id"] == workID {
			return item
		}
	}
	t.Fatalf("creative work %q missing from list: %v", workID, body)
	return nil
}

func assertNoCreativeRestoreBoundaryCalls(
	t *testing.T,
	delivery *creativeDeliveryRestoreTransport,
) {
	t.Helper()
	if delivery.resolveCalls != 0 || delivery.prepareCalls != 0 ||
		delivery.sendCalls != 0 || delivery.queryCalls != 0 ||
		len(delivery.content) != 0 || len(delivery.sends) != 0 || len(delivery.queries) != 0 ||
		len(delivery.envelopePreflights) != 0 || len(delivery.envelopeSends) != 0 ||
		len(delivery.envelopeQueries) != 0 {
		t.Fatalf(
			"read-only creative restore crossed delivery boundary: resolve=%d prepare=%d send=%d query=%d content=%d sends=%d queries=%d envelope_preflights=%d envelope_sends=%d envelope_queries=%d",
			delivery.resolveCalls, delivery.prepareCalls, delivery.sendCalls, delivery.queryCalls,
			len(delivery.content), len(delivery.sends), len(delivery.queries),
			len(delivery.envelopePreflights), len(delivery.envelopeSends), len(delivery.envelopeQueries),
		)
	}
}

func TestBUGK12CreativeDeliveryRestore20260824(t *testing.T) {
	delivery := &creativeDeliveryRestoreTransport{
		httpBatchTransport: httpBatchTransport{targets: httpBatchTargets()},
	}
	runtime, _ := newCreativeWorkDeliveryHTTPFixture(t, delivery)
	workID := seedReadyCreativeWork(t, runtime, k12.WorkTypeArt, k12.CreativeWorkSourceSnapshot{
		WorkType: k12.WorkTypeArt, DisplayName: "美术作品", WorkTitle: "雨后的家",
		SourceAssetID: assetstore.IDPrefix + "mingming/" + strings.Repeat("a", 64) + ".png",
	})
	view, err := runtime.Deps.GetCreativeWork(context.Background(), "mingming", workID)
	if err != nil {
		t.Fatal(err)
	}
	batchID := seedPendingCreativeDeliveryBatch(
		t, runtime, workID, restoreCreativeMessageDigest(t, view),
	)

	// 新 handler 只依赖持久化状态，模拟应用重建后重新读取作品页。
	restoredHandler := apihttp.NewHandler(apihttp.Runtime{
		Views: runtime.Registry.Views, Records: runtime.Records, Deps: runtime.Deps,
	})
	detailRec, detail := do(
		t, restoredHandler, http.MethodGet, "/creative-works/"+workID+"?agent=mingming", "",
	)
	if detailRec.Code != http.StatusOK || detail["delivery_batch_id"] != batchID {
		t.Errorf(
			"restored detail delivery_batch_id=%v status=%d want %q: %v",
			detail["delivery_batch_id"], detailRec.Code, batchID, detail,
		)
	}
	listRec, list := do(
		t, restoredHandler, http.MethodGet, "/creative-works?agent=mingming", "",
	)
	listed := creativeWorkInList(t, list, workID)
	if listRec.Code != http.StatusOK || listed["delivery_batch_id"] != batchID {
		t.Errorf(
			"restored list delivery_batch_id=%v status=%d want %q: %v",
			listed["delivery_batch_id"], listRec.Code, batchID, listed,
		)
	}
	assertNoCreativeRestoreBoundaryCalls(t, delivery)

	completeChangedCreativeFeedback(t, runtime, workID)
	changedRec, changed := do(
		t, restoredHandler, http.MethodGet, "/creative-works/"+workID+"?agent=mingming", "",
	)
	if changedRec.Code != http.StatusOK {
		t.Fatalf("changed creative detail status=%d body=%v", changedRec.Code, changed)
	}
	if stale, exists := changed["delivery_batch_id"]; exists && stale != "" {
		t.Errorf("changed creative detail exposed stale delivery batch %v", stale)
	}
	_, changedList := do(
		t, restoredHandler, http.MethodGet, "/creative-works?agent=mingming", "",
	)
	changedListed := creativeWorkInList(t, changedList, workID)
	if stale, exists := changedListed["delivery_batch_id"]; exists && stale != "" {
		t.Errorf("changed creative list exposed stale delivery batch %v", stale)
	}
	assertNoCreativeRestoreBoundaryCalls(t, delivery)
}
