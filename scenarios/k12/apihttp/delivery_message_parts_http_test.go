package apihttp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func apiMessagePartDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestDeliveryMessagePartsHTTPExposesCanonicalIdentityWithoutProviderResource(t *testing.T) {
	runtime, handler := newCreativeWorkDeliveryHTTPFixture(t, &httpBatchTransport{})
	target := k12.DeliveryTarget{
		Platform: "dingtalk", InstanceID: "bot-1", ChatID: "staff-1", Label: "钉钉",
	}
	batch, created, err := runtime.Records.PrepareDeliveryBatch(context.Background(), k12.DeliveryBatch{
		BatchID: "http-message-parts", AgentName: "mingming",
		ObjectKind: "creative_work", ObjectID: "work-1",
		DedupeKey: "http-message-parts-dedupe", ContentDigest: apiMessagePartDigest("content"),
		Receipts: []k12.DeliveryReceipt{
			{
				DeliveryID: "http-message-markdown", PartKind: messagecontent.PartMarkdown,
				PartOrdinal: 1, PartDigest: apiMessagePartDigest("markdown"),
				BindingID: "agent-rule:1", Target: target, DedupeKey: "http-message-markdown-dedupe",
				PayloadDigest: apiMessagePartDigest("markdown-payload"),
				PayloadJSON:   `{"msgtype":"markdown"}`, RenderJSON: `{}`,
			},
			{
				DeliveryID: "http-message-image", PartKind: messagecontent.PartArtifact,
				PartMIME: "image/png", PartOrdinal: 2, PartDigest: apiMessagePartDigest("image"),
				BindingID: "agent-rule:1", Target: target, DedupeKey: "http-message-image-dedupe",
				PayloadDigest: apiMessagePartDigest("image-payload"),
				PayloadJSON:   `{"msgtype":"image"}`, RenderJSON: `{}`,
			},
		},
	})
	if err != nil || !created {
		t.Fatalf("freeze delivery message parts: created=%v batch=%+v err=%v", created, batch, err)
	}
	const providerResource = "provider-resource-secret"
	if _, err := runtime.Records.SaveDeliveryPreparedResource(
		context.Background(), "mingming", "http-message-image", providerResource,
	); err != nil {
		t.Fatalf("persist provider resource: %v", err)
	}

	recorder, response := do(
		t, handler, http.MethodGet,
		"/delivery-batches/http-message-parts?agent=mingming", "",
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("get delivery message parts: status=%d body=%v", recorder.Code, response)
	}
	children, _ := response["receipts"].([]any)
	if len(children) != 2 {
		t.Fatalf("delivery message parts=%v want two", response["receipts"])
	}
	markdown, _ := children[0].(map[string]any)
	artifact, _ := children[1].(map[string]any)
	if markdown["part_kind"] != string(messagecontent.PartMarkdown) ||
		markdown["part_ordinal"] != float64(1) || markdown["part_digest"] == "" {
		t.Fatalf("markdown part identity missing: %v", markdown)
	}
	if artifact["part_kind"] != string(messagecontent.PartArtifact) ||
		artifact["part_mime"] != "image/png" || artifact["part_ordinal"] != float64(2) ||
		artifact["part_digest"] == "" {
		t.Fatalf("artifact part identity missing: %v", artifact)
	}
	if _, leaked := artifact["prepared_resource_id"]; leaked {
		t.Fatalf("API exposed provider resource field: %v", artifact)
	}
	if strings.Contains(recorder.Body.String(), providerResource) {
		t.Fatalf("API exposed provider resource value: %s", recorder.Body.String())
	}

	retryRecorder, _ := do(
		t, handler, http.MethodPost,
		"/delivery-receipts/http-message-image/retry", `{"agent":"mingming"}`,
	)
	if retryRecorder.Code != http.StatusConflict {
		t.Fatalf("batch child retry must use batch endpoint, status=%d", retryRecorder.Code)
	}
}
