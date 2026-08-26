package usecase_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type messagePartTransport struct {
	targets                []usecase.ResolvedDeliveryTarget
	resolveCalls           int
	prepareCalls           int
	preflightCalls         map[string]int
	preflightFailures      map[string]error
	sendPlans              map[string][]usecase.DeliveryTransportAck
	envelopeSendPlans      map[string][]usecase.DeliveryTransportAck
	envelopeQueryPlans     map[string][]usecase.DeliveryTransportAck
	envelopePreflightErr   error
	envelopePreflightHook  func([]k12.DeliveryReceipt)
	envelopeSendHook       func()
	envelopeSendSawLiveCtx bool
	sends                  []k12.DeliveryReceipt
	queries                []k12.DeliveryReceipt
	envelopePreflights     [][]k12.DeliveryReceipt
	envelopeSends          [][]k12.DeliveryReceipt
	envelopeQueries        [][]k12.DeliveryReceipt
	events                 []string
}

func newMessagePartTransport() *messagePartTransport {
	return &messagePartTransport{
		targets:            batchTargets(),
		preflightCalls:     make(map[string]int),
		preflightFailures:  make(map[string]error),
		sendPlans:          make(map[string][]usecase.DeliveryTransportAck),
		envelopeSendPlans:  make(map[string][]usecase.DeliveryTransportAck),
		envelopeQueryPlans: make(map[string][]usecase.DeliveryTransportAck),
	}
}

func (f *messagePartTransport) ResolveTextTargets(
	_ context.Context,
	_ string,
) ([]usecase.ResolvedDeliveryTarget, error) {
	f.resolveCalls++
	return append([]usecase.ResolvedDeliveryTarget(nil), f.targets...), nil
}

func (f *messagePartTransport) PrepareTextForTargets(
	_ context.Context,
	content string,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	f.prepareCalls++
	out := make([]usecase.PreparedTextDelivery, 0, len(targets))
	for _, target := range targets {
		payload := mustPartPayload(map[string]any{
			"kind": "markdown", "binding": target.BindingID, "content": content,
		})
		out = append(out, preparedPart(target, messagecontent.PartMarkdown, "", 1, partTestDigest(strings.TrimSpace(content)), payload))
	}
	return out, nil
}

func (f *messagePartTransport) PrepareMessageForTargets(
	_ context.Context,
	message usecase.DeliveryMessage,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	f.prepareCalls++
	attachments := make([]channel.Attachment, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		attachments = append(attachments, channel.Attachment{
			Name: attachment.Name, MIME: attachment.MIME, Data: append([]byte(nil), attachment.Data...),
		})
	}
	canonical, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12, "zh-CN", message.Content, message.Content, "", attachments,
	)
	if err != nil {
		return nil, err
	}
	parts, err := canonical.DeliveryParts()
	if err != nil {
		return nil, err
	}
	renderJSON, err := json.Marshal(canonical.RenderManifest)
	if err != nil {
		return nil, err
	}
	out := make([]usecase.PreparedTextDelivery, 0, len(targets)*len(parts))
	for _, target := range targets {
		for _, part := range parts {
			payloadProjection := make(map[string]any)
			canonicalJSON, err := json.Marshal(part)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(canonicalJSON, &payloadProjection); err != nil {
				return nil, err
			}
			switch part.Kind {
			case messagecontent.PartMarkdown:
				payloadProjection["content"] = part.Text
			case messagecontent.PartArtifact:
				if part.Attachment != nil {
					payloadProjection["name"] = part.Attachment.Name
					payloadProjection["data"] = part.Attachment.Data
				}
			}
			payloadJSON, err := json.Marshal(payloadProjection)
			if err != nil {
				return nil, err
			}
			out = append(out, usecase.PreparedTextDelivery{
				BindingID: target.BindingID, Target: target.Target,
				PartKind: part.Kind, PartMIME: part.MIME, PartOrdinal: part.Ordinal,
				PartDigest: part.Digest, PayloadJSON: string(payloadJSON), RenderJSON: string(renderJSON),
			})
		}
	}
	return out, nil
}

func (*messagePartTransport) PrepareText(
	context.Context,
	string,
	string,
) (usecase.PreparedTextDelivery, error) {
	return usecase.PreparedTextDelivery{}, errors.New("legacy singleton preparation must not be used")
}

func (f *messagePartTransport) PrepareDeliveryPartResource(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (string, error) {
	key := messagePartKey(receipt)
	f.preflightCalls[key]++
	f.events = append(f.events, "preflight:"+key)
	if err := f.preflightFailures[key]; err != nil {
		return "", err
	}
	return "resource:" + key, nil
}

func (f *messagePartTransport) SendPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	key := messagePartKey(receipt)
	f.sends = append(f.sends, receipt)
	f.events = append(f.events, "send:"+key)
	if plan := f.sendPlans[key]; len(plan) > 0 {
		ack := plan[0]
		f.sendPlans[key] = plan[1:]
		return ack, ack.Err
	}
	return usecase.DeliveryTransportAck{
		Status:            k12.DeliveryDelivered,
		ExternalMessageID: fmt.Sprintf("external:%s:%d", key, countMessagePartSends(f.sends, key)),
	}, nil
}

func (f *messagePartTransport) QueryPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.queries = append(f.queries, receipt)
	return usecase.DeliveryTransportAck{
		Status: k12.DeliveryDelivered, ExternalMessageID: receipt.ExternalMessageID,
	}, nil
}

func (f *messagePartTransport) PreflightPreparedEnvelope(
	_ context.Context,
	receipts []k12.DeliveryReceipt,
) error {
	frozen := append([]k12.DeliveryReceipt(nil), receipts...)
	f.envelopePreflights = append(f.envelopePreflights, frozen)
	if f.envelopePreflightHook != nil {
		f.envelopePreflightHook(frozen)
	}
	return f.envelopePreflightErr
}

// SendPreparedEnvelope 记录同一目标的一次物理 envelope 调用，并按测试计划返回 provider 回执。
func (f *messagePartTransport) SendPreparedEnvelope(
	ctx context.Context,
	receipts []k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	frozen := append([]k12.DeliveryReceipt(nil), receipts...)
	f.envelopeSends = append(f.envelopeSends, frozen)
	key := messageEnvelopeKey(receipts)
	f.events = append(f.events, "envelope-send:"+key)
	f.envelopeSendSawLiveCtx = ctx.Err() == nil
	if f.envelopeSendHook != nil {
		hook := f.envelopeSendHook
		f.envelopeSendHook = nil
		hook()
	}
	if plan := f.envelopeSendPlans[key]; len(plan) > 0 {
		ack := plan[0]
		f.envelopeSendPlans[key] = plan[1:]
		return ack, ack.Err
	}
	return usecase.DeliveryTransportAck{
		Status:            k12.DeliveryDelivered,
		ExternalMessageID: fmt.Sprintf("external:envelope:%s:%d", key, len(f.envelopeSends)),
	}, nil
}

func (f *messagePartTransport) QueryPreparedEnvelope(
	_ context.Context,
	receipts []k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	frozen := append([]k12.DeliveryReceipt(nil), receipts...)
	f.envelopeQueries = append(f.envelopeQueries, frozen)
	key := messageEnvelopeKey(receipts)
	if plan := f.envelopeQueryPlans[key]; len(plan) > 0 {
		ack := plan[0]
		f.envelopeQueryPlans[key] = plan[1:]
		return ack, ack.Err
	}
	externalID := ""
	if len(receipts) > 0 {
		externalID = receipts[0].ExternalMessageID
	}
	return usecase.DeliveryTransportAck{
		Status: k12.DeliveryDelivered, ExternalMessageID: externalID,
	}, nil
}

func preparedPart(
	target usecase.ResolvedDeliveryTarget,
	kind messagecontent.PartKind,
	mime string,
	ordinal int,
	digest string,
	payload string,
) usecase.PreparedTextDelivery {
	return usecase.PreparedTextDelivery{
		BindingID:   target.BindingID,
		Target:      target.Target,
		PartKind:    kind,
		PartMIME:    mime,
		PartOrdinal: ordinal,
		PartDigest:  digest,
		PayloadJSON: payload,
		RenderJSON:  `{"render_id":"render-target-parts"}`,
	}
}

func mustPartPayload(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(payload)
}

func partTestDigest(value string) string {
	return partTestDigestBytes([]byte(value))
}

func partTestDigestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func rewriteCreativePartPayload(
	t *testing.T,
	d usecase.Deps,
	batch k12.DeliveryBatch,
	partIndex int,
	mutate func(payload map[string]any),
) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(batch.Receipts[partIndex].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	mutate(payload)
	changed, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Records.DB().Exec(
		`UPDATE k12_delivery_receipts SET payload_json=?,payload_digest=? WHERE agent_name=? AND delivery_id=?`,
		string(changed), partTestDigest(string(changed)), batch.AgentName, batch.Receipts[partIndex].DeliveryID,
	); err != nil {
		t.Fatal(err)
	}
}

func creativePayloadAttachment(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	attachment, ok := payload["attachment"].(map[string]any)
	if !ok {
		t.Fatalf("canonical creative payload has no attachment object: %+v", payload)
	}
	return attachment
}

func messageBatchDedupeForTest(
	t *testing.T,
	agentName, objectKind, objectID string,
	message usecase.DeliveryMessage,
) string {
	t.Helper()
	identities := make([]usecase.DeliveryAttachmentIdentity, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		identities = append(identities, usecase.DeliveryAttachmentIdentity{
			Name: attachment.Name, MIME: attachment.MIME,
			ContentDigest: partTestDigestBytes(attachment.Data),
		})
	}
	contentDigest := deliveryContentDigestForTest(t, message.Content, identities)
	return partTestDigest(strings.Join([]string{
		strings.TrimSpace(agentName), strings.TrimSpace(objectKind), strings.TrimSpace(objectID), contentDigest,
	}, "\x00"))
}

func deliveryContentDigestForTest(
	t *testing.T,
	content string,
	attachments []usecase.DeliveryAttachmentIdentity,
) string {
	t.Helper()
	content = strings.TrimSpace(content)
	if len(attachments) == 0 {
		return partTestDigest(content)
	}
	type attachmentIdentity struct {
		Name   string `json:"name"`
		MIME   string `json:"mime"`
		Digest string `json:"digest"`
	}
	normalized := make([]attachmentIdentity, 0, len(attachments))
	for _, attachment := range attachments {
		normalized = append(normalized, attachmentIdentity{
			Name: strings.TrimSpace(attachment.Name), MIME: strings.ToLower(strings.TrimSpace(attachment.MIME)),
			Digest: strings.ToLower(strings.TrimSpace(attachment.ContentDigest)),
		})
	}
	payload, err := json.Marshal(struct {
		Content     string               `json:"content"`
		Attachments []attachmentIdentity `json:"attachments"`
	}{Content: content, Attachments: normalized})
	if err != nil {
		t.Fatal(err)
	}
	return partTestDigest(string(payload))
}

func canonicalBatchIdentityForTest(
	t *testing.T,
	agentName, objectKind, objectID string,
	content *messagecontent.MessageContent,
) (contentDigest string, dedupeKey string) {
	t.Helper()
	if content == nil {
		t.Fatal("canonical content is missing")
	}
	identities := make([]usecase.DeliveryAttachmentIdentity, 0, len(content.Attachments))
	for _, attachment := range content.Attachments {
		identities = append(identities, usecase.DeliveryAttachmentIdentity{
			Name: attachment.Name, MIME: attachment.MIME, ContentDigest: attachment.Digest,
		})
	}
	contentDigest = deliveryContentDigestForTest(t, content.Markdown, identities)
	dedupeKey = partTestDigest(strings.Join([]string{
		agentName, objectKind, objectID, contentDigest,
	}, "\x00"))
	return contentDigest, dedupeKey
}

func deliveryPartDedupeForTest(receipt k12.DeliveryReceipt, payloadDigest string) string {
	return partTestDigest(strings.Join([]string{
		receipt.AgentName, receipt.ObjectKind, receipt.ObjectID, receipt.BindingID,
		string(receipt.PartKind), receipt.PartMIME, fmt.Sprintf("%d", receipt.PartOrdinal),
		receipt.PartDigest, payloadDigest,
	}, "\x00"))
}

func messagePartKey(receipt k12.DeliveryReceipt) string {
	return fmt.Sprintf("%s/%d", receipt.BindingID, receipt.PartOrdinal)
}

func countMessagePartSends(receipts []k12.DeliveryReceipt, key string) int {
	count := 0
	for _, receipt := range receipts {
		if messagePartKey(receipt) == key {
			count++
		}
	}
	return count
}

func messageEnvelopeKey(receipts []k12.DeliveryReceipt) string {
	if len(receipts) == 0 {
		return "empty"
	}
	return receipts[0].BindingID
}

func messageWithImage() usecase.DeliveryMessage {
	return usecase.DeliveryMessage{
		Content: "## 作品点评\n\n画面色彩明快。",
		Attachments: []usecase.DeliveryAttachment{
			{Name: "work.png", MIME: "image/png", Data: creativeImageFixture()},
		},
	}
}

func canonicalCreativeDeliveryParts(
	t *testing.T,
	imageCount int,
) ([]channel.DeliveryPart, string) {
	return canonicalCreativeDeliveryPartsWithPDF(t, imageCount, 0)
}

func canonicalCreativeDeliveryPartsWithPDF(
	t *testing.T,
	imageCount int,
	pdfCount int,
) ([]channel.DeliveryPart, string) {
	t.Helper()
	attachments := make([]channel.Attachment, 0, imageCount+pdfCount)
	for i := range imageCount {
		attachments = append(attachments, channel.Attachment{
			Name: fmt.Sprintf("work-%d.png", i+1),
			MIME: "image/png",
			Data: []byte(fmt.Sprintf("creative-image-%d", i+1)),
		})
	}
	for i := range pdfCount {
		attachments = append(attachments, channel.Attachment{
			Name: fmt.Sprintf("work-%d.pdf", i+1),
			MIME: "application/pdf",
			Data: []byte(fmt.Sprintf("%%PDF-1.7\ncreative-pdf-%d\n%%%%EOF", i+1)),
		})
	}
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 作品点评\n\n画面色彩明快。",
		"## 作品点评\n\n画面色彩明快。",
		"",
		attachments,
	)
	if err != nil {
		t.Fatalf("build canonical creative message: %v", err)
	}
	parts, err := message.DeliveryParts()
	if err != nil {
		t.Fatalf("build canonical creative parts: %v", err)
	}
	renderJSON, err := json.Marshal(message.RenderManifest)
	if err != nil {
		t.Fatalf("encode canonical creative render manifest: %v", err)
	}
	return parts, string(renderJSON)
}

func freezeCreativeCanonicalPartPrefix(
	t *testing.T,
	d usecase.Deps,
	fake *messagePartTransport,
	objectID string,
	imageCount int,
	storedPartCount int,
) k12.DeliveryBatch {
	return freezeCreativeCanonicalPartPrefixWithPDF(
		t, d, fake, objectID, imageCount, 0, storedPartCount,
	)
}

func freezeCreativeCanonicalPartPrefixWithPDF(
	t *testing.T,
	d usecase.Deps,
	fake *messagePartTransport,
	objectID string,
	imageCount int,
	pdfCount int,
	storedPartCount int,
) k12.DeliveryBatch {
	t.Helper()
	parts, renderJSON := canonicalCreativeDeliveryPartsWithPDF(t, imageCount, pdfCount)
	if storedPartCount < 1 || storedPartCount > len(parts) {
		t.Fatalf("invalid stored canonical part prefix: %d/%d", storedPartCount, len(parts))
	}
	target := fake.targets[0]
	contentDigest, batchDedupeKey := canonicalBatchIdentityForTest(
		t, "xiaoming", "creative_work", objectID, parts[0].MessageContent,
	)
	batch := k12.DeliveryBatch{
		BatchID:       "batch-" + objectID,
		AgentName:     "xiaoming",
		ObjectKind:    "creative_work",
		ObjectID:      objectID,
		DedupeKey:     batchDedupeKey,
		ContentDigest: contentDigest,
	}
	for i, part := range parts[:storedPartCount] {
		payload, err := json.Marshal(part)
		if err != nil {
			t.Fatalf("encode canonical creative part %d: %v", i+1, err)
		}
		receipt := k12.DeliveryReceipt{
			DeliveryID:    fmt.Sprintf("%s-part-%d", objectID, i+1),
			BindingID:     target.BindingID,
			Target:        target.Target,
			PartKind:      part.Kind,
			PartMIME:      part.MIME,
			PartOrdinal:   part.Ordinal,
			PartDigest:    part.Digest,
			PayloadDigest: partTestDigest(string(payload)),
			PayloadJSON:   string(payload),
			RenderJSON:    renderJSON,
		}
		receipt.AgentName = batch.AgentName
		receipt.ObjectKind = batch.ObjectKind
		receipt.ObjectID = batch.ObjectID
		receipt.DedupeKey = deliveryPartDedupeForTest(receipt, receipt.PayloadDigest)
		batch.Receipts = append(batch.Receipts, receipt)
	}
	stored, created, err := d.Records.PrepareDeliveryBatch(context.Background(), batch)
	if err != nil || !created {
		t.Fatalf("freeze incomplete canonical creative batch: created=%v batch=%+v err=%v", created, stored, err)
	}
	for _, receipt := range stored.Receipts {
		if receipt.PartKind != messagecontent.PartArtifact {
			continue
		}
		if _, err := d.Records.SaveDeliveryPreparedResource(
			context.Background(), stored.AgentName, receipt.DeliveryID, "resource:"+receipt.DeliveryID,
		); err != nil {
			t.Fatalf("freeze canonical creative resource: %v", err)
		}
	}
	return stored
}

func creativeImageFixture() []byte {
	data, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
	)
	if err != nil {
		panic(err)
	}
	return data
}

func messageWithImageAndPDF() usecase.DeliveryMessage {
	return usecase.DeliveryMessage{
		Content: "## 本周练习\n\n请完成后订正。",
		Attachments: []usecase.DeliveryAttachment{
			{Name: "page-1.png", MIME: "image/png", Data: creativeImageFixture()},
			{Name: "practice.pdf", MIME: "application/pdf", Data: []byte("%PDF-1.7\n%%EOF\n")},
		},
	}
}

func receiptsForBinding(receipts []k12.DeliveryReceipt, bindingID string) []k12.DeliveryReceipt {
	out := make([]k12.DeliveryReceipt, 0, len(receipts))
	for _, receipt := range receipts {
		if receipt.BindingID == bindingID {
			out = append(out, receipt)
		}
	}
	return out
}

func assertSharedEnvelopeState(t *testing.T, receipts []k12.DeliveryReceipt, wantStatus k12.DeliveryReceiptStatus, wantAttempt int) {
	t.Helper()
	if len(receipts) != 2 {
		t.Fatalf("creative envelope component rows=%d want=2: %+v", len(receipts), receipts)
	}
	markdown, image := receipts[0], receipts[1]
	if markdown.PartKind != messagecontent.PartMarkdown || markdown.PartOrdinal != 1 ||
		image.PartKind != messagecontent.PartArtifact || image.PartOrdinal != 2 || image.PartMIME != "image/png" ||
		markdown.DeliveryID == image.DeliveryID || markdown.DedupeKey == image.DedupeKey {
		t.Fatalf("creative envelope lost independent ordered component identity: %+v", receipts)
	}
	if image.PreparedResourceID == "" {
		t.Fatalf("creative image component has no prepared resource: %+v", image)
	}
	if markdown.Attempt != wantAttempt || image.Attempt != wantAttempt ||
		markdown.Status != wantStatus || image.Status != wantStatus ||
		markdown.ExternalMessageID == "" || markdown.ExternalMessageID != image.ExternalMessageID ||
		markdown.LastError != image.LastError {
		t.Fatalf("creative envelope component state diverged: %+v", receipts)
	}
}

func TestCreativeCompositeKeepsTwoComponentsAndSendsOncePerTarget(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "creative_work", "work-composite", messageWithImage(),
	)
	if err != nil || !created {
		t.Fatalf("prepare creative composite: created=%v batch=%+v err=%v", created, batch, err)
	}
	if len(batch.Receipts) != len(fake.targets)*2 {
		t.Fatalf("target×component rows=%d want=%d: %+v", len(batch.Receipts), len(fake.targets)*2, batch)
	}
	if len(fake.envelopeSends) != len(fake.targets) || len(fake.sends) != 0 {
		t.Fatalf("each target needs one physical envelope and zero legacy part sends: envelopes=%+v legacy=%+v",
			fake.envelopeSends, fake.sends)
	}
	seenExternal := make(map[string]struct{}, len(fake.targets))
	for _, target := range fake.targets {
		group := receiptsForBinding(batch.Receipts, target.BindingID)
		assertSharedEnvelopeState(t, group, k12.DeliveryDelivered, 1)
		if _, duplicate := seenExternal[group[0].ExternalMessageID]; duplicate {
			t.Fatalf("different targets shared one external message id: %+v", batch.Receipts)
		}
		seenExternal[group[0].ExternalMessageID] = struct{}{}
	}
	if len(fake.preflightCalls) != len(fake.targets) {
		t.Fatalf("each target image must be prepared exactly once: %+v", fake.preflightCalls)
	}
	firstEnvelope := len(fake.events)
	for i, event := range fake.events {
		if strings.HasPrefix(event, "envelope-send:") {
			firstEnvelope = i
			break
		}
	}
	if firstEnvelope != len(fake.targets) {
		t.Fatalf("all image preflights must finish before any envelope send: %v", fake.events)
	}
	for _, envelope := range fake.envelopeSends {
		if len(envelope) != 2 || envelope[0].BindingID != envelope[1].BindingID ||
			envelope[0].PartOrdinal != 1 || envelope[1].PartOrdinal != 2 {
			t.Fatalf("physical envelope did not receive one target's exact ordered components: %+v", envelope)
		}
	}
}

func TestCreativeCanonicalTwoImagesSendOneEnvelopeWithThreeSharedRows(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	d.Delivery = fake
	batch := freezeCreativeCanonicalPartPrefix(t, d, fake, "canonical-two-images", 2, 3)

	processed, err := d.RecoverDeliveryReceipts(context.Background(), batch.AgentName)
	if err != nil || processed != 3 {
		t.Fatalf("recover complete two-image creative batch: processed=%d err=%v", processed, err)
	}
	if len(fake.envelopeSends) != 1 || len(fake.sends) != 0 {
		t.Fatalf("two images must use one envelope and zero legacy sends: envelopes=%+v legacy=%+v",
			fake.envelopeSends, fake.sends)
	}
	envelope := fake.envelopeSends[0]
	if len(envelope) != 3 {
		t.Fatalf("two-image envelope components=%d want=3: %+v", len(envelope), envelope)
	}
	wantKinds := []messagecontent.PartKind{
		messagecontent.PartMarkdown, messagecontent.PartArtifact, messagecontent.PartArtifact,
	}
	wantMIMEs := []string{"", "image/png", "image/png"}
	for i, receipt := range envelope {
		if receipt.PartKind != wantKinds[i] || receipt.PartMIME != wantMIMEs[i] ||
			receipt.PartOrdinal != i+1 {
			t.Fatalf("two-image envelope order[%d] diverged: %+v", i, receipt)
		}
		if i == 0 && receipt.PreparedResourceID != "" {
			t.Fatalf("Markdown component unexpectedly has a prepared resource: %+v", receipt)
		}
		if i > 0 && receipt.PreparedResourceID == "" {
			t.Fatalf("image component %d has no prepared resource: %+v", i, receipt)
		}
	}

	persisted, err := d.GetDeliveryBatch(context.Background(), batch.AgentName, batch.BatchID)
	if err != nil || persisted.Status != k12.DeliveryBatchDelivered || len(persisted.Receipts) != 3 {
		t.Fatalf("reload two-image envelope: batch=%+v err=%v", persisted, err)
	}
	sharedExternalID := persisted.Receipts[0].ExternalMessageID
	if sharedExternalID == "" {
		t.Fatal("two-image envelope has no shared external message id")
	}
	for i, receipt := range persisted.Receipts {
		if receipt.Status != k12.DeliveryDelivered || receipt.Attempt != 1 ||
			receipt.ExternalMessageID != sharedExternalID || receipt.LastError != "" ||
			receipt.PartOrdinal != i+1 {
			t.Fatalf("two-image envelope persisted state[%d] diverged: %+v", i, receipt)
		}
	}
}

func TestCreativeCompositeOnlyGroupsDingTalkTargets(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets[1].Target.Platform = " FeiShu "
	fake.targets[1].Target.Label = "Feishu"
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "creative_work", "work-mixed-platforms", messageWithImage(),
	)
	if err != nil || !created || len(batch.Receipts) != 4 {
		t.Fatalf("prepare mixed-platform creative batch: created=%v batch=%+v err=%v", created, batch, err)
	}
	if len(fake.envelopeSends) != 1 || len(fake.sends) != 2 {
		t.Fatalf("only DingTalk may use a creative envelope: envelopes=%+v legacy=%+v",
			fake.envelopeSends, fake.sends)
	}
	dingtalkRows := receiptsForBinding(batch.Receipts, "agent-rule:1")
	assertSharedEnvelopeState(t, dingtalkRows, k12.DeliveryDelivered, 1)
	if len(fake.envelopeSends[0]) != 2 || fake.envelopeSends[0][0].Target.Platform != "dingtalk" {
		t.Fatalf("creative envelope was routed to a non-DingTalk target: %+v", fake.envelopeSends)
	}
	feishuRows := receiptsForBinding(batch.Receipts, "agent-rule:2")
	if len(feishuRows) != 2 {
		t.Fatalf("Feishu component rows=%d want=2: %+v", len(feishuRows), feishuRows)
	}
	seenExternal := make(map[string]struct{}, len(feishuRows))
	for _, receipt := range feishuRows {
		if receipt.Target.Platform != "feishu" || receipt.Status != k12.DeliveryDelivered ||
			receipt.Attempt != 1 || receipt.ExternalMessageID == "" {
			t.Fatalf("Feishu component did not keep independent delivery state: %+v", receipt)
		}
		if _, duplicate := seenExternal[receipt.ExternalMessageID]; duplicate {
			t.Fatalf("Feishu legacy parts unexpectedly shared an external id: %+v", feishuRows)
		}
		seenExternal[receipt.ExternalMessageID] = struct{}{}
	}
	for _, receipt := range fake.sends {
		if receipt.Target.Platform != "feishu" {
			t.Fatalf("legacy part send reached the DingTalk target: %+v", fake.sends)
		}
	}
}

func TestCreativeCompositeGroupCASFailureHasZeroProviderSideEffects(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	d.Delivery = fake
	if _, err := d.Records.DB().Exec(`CREATE TRIGGER fail_creative_group_attempt
		BEFORE UPDATE OF status, attempt ON k12_delivery_receipts
		WHEN NEW.batch_ordinal = 2 AND NEW.status = 'sending'
		BEGIN SELECT RAISE(ABORT, 'forced creative group CAS failure'); END`); err != nil {
		t.Fatal(err)
	}

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "creative_work", "work-cas-failure", messageWithImage(),
	)
	if err == nil || !created {
		t.Fatalf("group CAS failure must retain the frozen batch and return an error: created=%v batch=%+v err=%v", created, batch, err)
	}
	if len(fake.envelopeSends) != 0 || len(fake.sends) != 0 {
		t.Fatalf("group CAS failure crossed the provider boundary: envelopes=%+v legacy=%+v", fake.envelopeSends, fake.sends)
	}
	current, getErr := d.GetDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if getErr != nil || len(current.Receipts) != 2 {
		t.Fatalf("reload group CAS failure: batch=%+v err=%v", current, getErr)
	}
	first, second := current.Receipts[0], current.Receipts[1]
	if first.Status != second.Status || first.Attempt != 0 || second.Attempt != 0 ||
		first.ExternalMessageID != "" || second.ExternalMessageID != "" {
		t.Fatalf("failed group CAS left mixed component state: %+v", current.Receipts)
	}
}

func TestCreativeCanonicalImageManifestMissingComponentFailsClosed(t *testing.T) {
	tests := []struct {
		name            string
		imageCount      int
		storedPartCount int
	}{
		{name: "one image missing", imageCount: 1, storedPartCount: 1},
		{name: "second image missing", imageCount: 2, storedPartCount: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDataDeps(t)
			fake := newMessagePartTransport()
			fake.targets = fake.targets[:1]
			d.Delivery = fake
			batch := freezeCreativeCanonicalPartPrefix(
				t, d, fake, strings.ReplaceAll(tt.name, " ", "-"),
				tt.imageCount, tt.storedPartCount,
			)

			processed, err := d.RecoverDeliveryReceipts(context.Background(), batch.AgentName)
			if !errors.Is(err, records.ErrIllegalTransition) || processed != 0 {
				t.Fatalf("missing canonical image component must fail closed: processed=%d err=%v", processed, err)
			}
			if len(fake.sends) != 0 || len(fake.envelopeSends) != 0 {
				t.Fatalf("missing canonical image component crossed provider boundary: legacy=%+v envelopes=%+v",
					fake.sends, fake.envelopeSends)
			}
			current, getErr := d.GetDeliveryBatch(context.Background(), batch.AgentName, batch.BatchID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			for _, receipt := range current.Receipts {
				if receipt.Status != k12.DeliveryPending || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
					t.Fatalf("missing canonical component changed delivery state: %+v", current.Receipts)
				}
			}
		})
	}
}

func TestCreativeCanonicalMarkdownWithoutAttachmentsKeepsSinglePartDelivery(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	d.Delivery = fake
	batch := freezeCreativeCanonicalPartPrefix(t, d, fake, "canonical-markdown-only", 0, 1)

	processed, err := d.RecoverDeliveryReceipts(context.Background(), batch.AgentName)
	if err != nil || processed != 1 {
		t.Fatalf("recover canonical Markdown-only creative batch: processed=%d err=%v", processed, err)
	}
	if len(fake.sends) != 1 || len(fake.envelopeSends) != 0 {
		t.Fatalf("Markdown-only creative delivery changed transport shape: legacy=%+v envelopes=%+v",
			fake.sends, fake.envelopeSends)
	}
}

func TestCreativeCanonicalPDFManifestMissingComponentFailsClosed(t *testing.T) {
	tests := []struct {
		name            string
		imageCount      int
		storedPartCount int
	}{
		{name: "image and PDF missing PDF", imageCount: 1, storedPartCount: 2},
		{name: "PDF only missing PDF", imageCount: 0, storedPartCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDataDeps(t)
			fake := newMessagePartTransport()
			fake.targets = fake.targets[:1]
			d.Delivery = fake
			batch := freezeCreativeCanonicalPartPrefixWithPDF(
				t, d, fake, strings.ReplaceAll(tt.name, " ", "-"),
				tt.imageCount, 1, tt.storedPartCount,
			)

			processed, err := d.RecoverDeliveryReceipts(context.Background(), batch.AgentName)
			if !errors.Is(err, records.ErrIllegalTransition) || processed != 0 {
				t.Fatalf("missing canonical PDF component must fail closed: processed=%d err=%v", processed, err)
			}
			if len(fake.sends) != 0 || len(fake.envelopeSends) != 0 {
				t.Fatalf("missing canonical PDF component crossed provider boundary: legacy=%+v envelopes=%+v",
					fake.sends, fake.envelopeSends)
			}
			current, getErr := d.GetDeliveryBatch(context.Background(), batch.AgentName, batch.BatchID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			for _, receipt := range current.Receipts {
				if receipt.Status != k12.DeliveryPending || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
					t.Fatalf("missing canonical PDF changed delivery state: %+v", current.Receipts)
				}
			}
		})
	}
}

func TestCreativeCanonicalCompleteImageAndPDFKeepsTwoPhysicalMessages(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	d.Delivery = fake
	batch := freezeCreativeCanonicalPartPrefixWithPDF(
		t, d, fake, "canonical-image-pdf-complete", 1, 1, 3,
	)

	processed, err := d.RecoverDeliveryReceipts(context.Background(), batch.AgentName)
	if err != nil || processed != 3 {
		t.Fatalf("recover complete image+PDF creative batch: processed=%d err=%v", processed, err)
	}
	if len(fake.envelopeSends) != 1 || len(fake.sends) != 1 ||
		fake.sends[0].PartMIME != "application/pdf" {
		t.Fatalf("complete creative image+PDF transport shape changed: envelopes=%+v legacy=%+v",
			fake.envelopeSends, fake.sends)
	}
}

func TestCreativeCanonicalPDFComponentIdentityDriftFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch)
	}{
		{
			name: "wrong PDF digest",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET part_digest=? WHERE agent_name=? AND delivery_id=?`,
					partTestDigest("wrong-pdf"), batch.AgentName, batch.Receipts[2].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "extra PDF row",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(`INSERT INTO k12_delivery_receipts (
					delivery_id,batch_id,batch_ordinal,part_kind,part_mime,part_ordinal,
					part_digest,prepared_resource_id,agent_name,object_kind,object_id,binding_id,
					platform,instance_id,chat_id,target_label,status,dedupe_key,payload_digest,payload_json,
					render_manifest_json,external_message_id,attempt,last_error,created_at,updated_at)
					SELECT ?,batch_id,4,part_kind,part_mime,4,
					part_digest,prepared_resource_id,agent_name,object_kind,object_id,binding_id,
					platform,instance_id,chat_id,target_label,status,?,payload_digest,payload_json,
					render_manifest_json,external_message_id,attempt,last_error,created_at,updated_at
					FROM k12_delivery_receipts WHERE agent_name=? AND delivery_id=?`,
					"extra-pdf-row", "extra-pdf-row-dedupe", batch.AgentName, batch.Receipts[2].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDataDeps(t)
			fake := newMessagePartTransport()
			fake.targets = fake.targets[:1]
			d.Delivery = fake
			batch := freezeCreativeCanonicalPartPrefixWithPDF(
				t, d, fake, "canonical-pdf-"+strings.ReplaceAll(tt.name, " ", "-"), 1, 1, 3,
			)
			tt.mutate(t, d, batch)

			processed, err := d.RecoverDeliveryReceipts(context.Background(), batch.AgentName)
			if !errors.Is(err, records.ErrIllegalTransition) || processed != 0 {
				t.Fatalf("PDF identity drift must fail closed: processed=%d err=%v", processed, err)
			}
			if len(fake.sends) != 0 || len(fake.envelopeSends) != 0 {
				t.Fatalf("PDF identity drift crossed provider boundary: legacy=%+v envelopes=%+v",
					fake.sends, fake.envelopeSends)
			}
		})
	}
}

func TestCreativeCompositeNeutralIntegrityFailsBeforeGroupCAS(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch)
	}{
		{
			name: "payload digest drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET payload_digest=? WHERE agent_name=? AND delivery_id=?`,
					partTestDigest("different-payload"), batch.AgentName, batch.Receipts[1].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "render evidence drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET render_manifest_json=? WHERE agent_name=? AND delivery_id=?`,
					`{"render_id":"drifted-render"}`, batch.AgentName, batch.Receipts[1].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "batch ordinal drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET batch_ordinal=? WHERE agent_name=? AND delivery_id=?`,
					9, batch.AgentName, batch.Receipts[1].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "payload part ordinal drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				var payload map[string]any
				if err := json.Unmarshal([]byte(batch.Receipts[1].PayloadJSON), &payload); err != nil {
					t.Fatal(err)
				}
				payload["ordinal"] = 9
				changed, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET payload_json=?,payload_digest=? WHERE agent_name=? AND delivery_id=?`,
					string(changed), partTestDigest(string(changed)), batch.AgentName, batch.Receipts[1].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "payload part MIME drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				var payload map[string]any
				if err := json.Unmarshal([]byte(batch.Receipts[1].PayloadJSON), &payload); err != nil {
					t.Fatal(err)
				}
				payload["mime"] = "image/jpeg"
				changed, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET payload_json=?,payload_digest=? WHERE agent_name=? AND delivery_id=?`,
					string(changed), partTestDigest(string(changed)), batch.AgentName, batch.Receipts[1].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "payload Markdown text drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				rewriteCreativePartPayload(t, d, batch, 0, func(payload map[string]any) {
					payload["text"] = "file:///private/tmp/leak"
				})
			},
		},
		{
			name: "payload image bytes drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				rewriteCreativePartPayload(t, d, batch, 1, func(payload map[string]any) {
					creativePayloadAttachment(t, payload)["Data"] = base64.StdEncoding.EncodeToString([]byte("changed-image"))
				})
			},
		},
		{
			name: "payload image attachment MIME drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				rewriteCreativePartPayload(t, d, batch, 1, func(payload map[string]any) {
					creativePayloadAttachment(t, payload)["MIME"] = "image/jpeg"
				})
			},
		},
		{
			name: "payload image attachment name drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				rewriteCreativePartPayload(t, d, batch, 1, func(payload map[string]any) {
					creativePayloadAttachment(t, payload)["Name"] = "renamed.png"
				})
			},
		},
		{
			name: "outer object identity drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET object_id=? WHERE agent_name=? AND delivery_id=?`,
					"different-work", batch.AgentName, batch.Receipts[1].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "target instance identity incomplete",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET instance_id='' WHERE agent_name=? AND batch_id=?`,
					batch.AgentName, batch.BatchID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "target platform identity incomplete",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				ctx := context.Background()
				conn, err := d.Records.DB().Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if _, err := conn.ExecContext(ctx, `PRAGMA ignore_check_constraints=ON`); err != nil {
					t.Fatal(err)
				}
				if _, err := conn.ExecContext(ctx,
					`UPDATE k12_delivery_receipts SET platform='' WHERE agent_name=? AND batch_id=?`,
					batch.AgentName, batch.BatchID,
				); err != nil {
					t.Fatal(err)
				}
				if _, err := conn.ExecContext(ctx, `PRAGMA ignore_check_constraints=OFF`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "Markdown prepared resource drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET prepared_resource_id=? WHERE agent_name=? AND delivery_id=?`,
					"unexpected-markdown-resource", batch.AgentName, batch.Receipts[0].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "batch content digest drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_batches SET content_digest=? WHERE agent_name=? AND batch_id=?`,
					partTestDigest("different-root-content"), batch.AgentName, batch.BatchID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "batch dedupe drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_batches SET dedupe_key=? WHERE agent_name=? AND batch_id=?`,
					partTestDigest("different-root-dedupe"), batch.AgentName, batch.BatchID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "child dedupe drift",
			mutate: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET dedupe_key=? WHERE agent_name=? AND delivery_id=?`,
					partTestDigest("different-child-dedupe"), batch.AgentName, batch.Receipts[1].DeliveryID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDataDeps(t)
			fake := newMessagePartTransport()
			fake.targets = fake.targets[:1]
			d.Delivery = fake
			batch := freezeCreativeCanonicalPartPrefix(
				t, d, fake, "neutral-"+strings.ReplaceAll(tt.name, " ", "-"), 1, 2,
			)
			tt.mutate(t, d, batch)

			processed, err := d.RecoverDeliveryReceipts(context.Background(), batch.AgentName)
			if !errors.Is(err, records.ErrIllegalTransition) || processed != 0 {
				t.Fatalf("neutral integrity drift must fail before group CAS: processed=%d err=%v", processed, err)
			}
			if len(fake.preflightCalls) != 0 || len(fake.sends) != 0 || len(fake.envelopeSends) != 0 {
				t.Fatalf("neutral integrity drift crossed a provider boundary: preflight=%+v legacy=%+v envelopes=%+v",
					fake.preflightCalls, fake.sends, fake.envelopeSends)
			}
			current, getErr := d.GetDeliveryBatch(context.Background(), batch.AgentName, batch.BatchID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			for _, receipt := range current.Receipts {
				if receipt.Status != k12.DeliveryPending || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
					t.Fatalf("neutral integrity drift consumed a group attempt: %+v", current.Receipts)
				}
			}
		})
	}
}

func TestCreativeCompositeCanonicalChildrenCannotDetachFromBatchRoot(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	d.Delivery = fake
	batch := freezeCreativeCanonicalPartPrefix(t, d, fake, "root-a", 1, 2)
	messageB, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 另一份作品点评\n\n这是完整且自洽的正文 B。",
		"## 另一份作品点评\n\n这是完整且自洽的正文 B。",
		"",
		[]channel.Attachment{{Name: "work-1.png", MIME: "image/png", Data: []byte("creative-image-1")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	partsB, err := messageB.DeliveryParts()
	if err != nil {
		t.Fatal(err)
	}
	renderB, err := json.Marshal(messageB.RenderManifest)
	if err != nil {
		t.Fatal(err)
	}
	for i, part := range partsB {
		payload, err := json.Marshal(part)
		if err != nil {
			t.Fatal(err)
		}
		payloadDigest := partTestDigest(string(payload))
		changed := batch.Receipts[i]
		changed.PartKind = part.Kind
		changed.PartMIME = part.MIME
		changed.PartOrdinal = part.Ordinal
		changed.PartDigest = part.Digest
		changed.PayloadDigest = payloadDigest
		changed.DedupeKey = deliveryPartDedupeForTest(changed, payloadDigest)
		if _, err := d.Records.DB().Exec(
			`UPDATE k12_delivery_receipts
			 SET part_kind=?,part_mime=?,part_ordinal=?,part_digest=?,dedupe_key=?,payload_digest=?,payload_json=?,render_manifest_json=?
			 WHERE agent_name=? AND delivery_id=?`,
			changed.PartKind, changed.PartMIME, changed.PartOrdinal, changed.PartDigest,
			changed.DedupeKey, payloadDigest, string(payload), string(renderB),
			batch.AgentName, changed.DeliveryID,
		); err != nil {
			t.Fatal(err)
		}
	}

	processed, err := d.RecoverDeliveryReceipts(context.Background(), batch.AgentName)
	if !errors.Is(err, records.ErrIllegalTransition) || processed != 0 {
		t.Fatalf("canonical children detached from batch root: processed=%d err=%v", processed, err)
	}
	if len(fake.preflightCalls) != 0 || len(fake.envelopePreflights) != 0 ||
		len(fake.sends) != 0 || len(fake.envelopeSends) != 0 {
		t.Fatalf("detached canonical children crossed provider boundary: media=%+v envelopePreflight=%+v legacy=%+v envelope=%+v",
			fake.preflightCalls, fake.envelopePreflights, fake.sends, fake.envelopeSends)
	}
	current, getErr := d.GetDeliveryBatch(context.Background(), batch.AgentName, batch.BatchID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	for _, receipt := range current.Receipts {
		if receipt.Status != k12.DeliveryPending || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
			t.Fatalf("detached canonical children consumed an attempt: %+v", current.Receipts)
		}
	}
}

func TestCreativeCompositeRejectsLegacyPayloadBeforeCAS(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	d.Delivery = fake
	target := fake.targets[0]
	content := "## 作品点评\n\n画面色彩明快。"
	image := creativeImageFixture()
	markdownPayload := mustPartPayload(map[string]any{
		"kind": "markdown", "content": content,
	})
	imagePayload := mustPartPayload(map[string]any{
		"kind": "artifact", "mime": "image/png", "data": image,
	})
	batch := k12.DeliveryBatch{
		BatchID: "legacy-composite", AgentName: "xiaoming",
		ObjectKind: "creative_work", ObjectID: "legacy-work",
		DedupeKey: "legacy-composite-dedupe", ContentDigest: partTestDigest(content),
		Receipts: []k12.DeliveryReceipt{
			{
				DeliveryID: "legacy-markdown", BindingID: target.BindingID, Target: target.Target,
				PartKind: messagecontent.PartMarkdown, PartOrdinal: 1, PartDigest: partTestDigest(content),
				DedupeKey: "legacy-markdown-dedupe", PayloadDigest: partTestDigest(markdownPayload),
				PayloadJSON: markdownPayload, RenderJSON: `{"render_id":"legacy-render"}`,
			},
			{
				DeliveryID: "legacy-image", BindingID: target.BindingID, Target: target.Target,
				PartKind: messagecontent.PartArtifact, PartMIME: "image/png", PartOrdinal: 2,
				PartDigest: partTestDigestBytes(image), DedupeKey: "legacy-image-dedupe",
				PayloadDigest: partTestDigest(imagePayload), PayloadJSON: imagePayload,
				RenderJSON: `{"render_id":"legacy-render"}`,
			},
		},
	}
	stored, created, err := d.Records.PrepareDeliveryBatch(context.Background(), batch)
	if err != nil || !created {
		t.Fatalf("freeze legacy composite: created=%v batch=%+v err=%v", created, stored, err)
	}
	if _, err := d.Records.SaveDeliveryPreparedResource(
		context.Background(), stored.AgentName, stored.Receipts[1].DeliveryID, "resource:legacy-image",
	); err != nil {
		t.Fatal(err)
	}

	processed, err := d.RecoverDeliveryReceipts(context.Background(), stored.AgentName)
	if !errors.Is(err, records.ErrIllegalTransition) || processed != 0 {
		t.Fatalf("legacy creative composite must fail before group CAS: processed=%d err=%v", processed, err)
	}
	if len(fake.preflightCalls) != 0 || len(fake.envelopePreflights) != 0 ||
		len(fake.sends) != 0 || len(fake.envelopeSends) != 0 {
		t.Fatalf("legacy creative composite crossed provider boundary: media=%+v envelopePreflight=%+v legacy=%+v envelope=%+v",
			fake.preflightCalls, fake.envelopePreflights, fake.sends, fake.envelopeSends)
	}
	current, getErr := d.GetDeliveryBatch(context.Background(), stored.AgentName, stored.BatchID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	for _, receipt := range current.Receipts {
		if receipt.Status != k12.DeliveryPending || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
			t.Fatalf("legacy creative composite consumed an attempt: %+v", current.Receipts)
		}
	}
}

func TestCreativeCompositeEnvelopePreflightFailureDoesNotConsumeAttempt(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	preflightErr := errors.New("prepared envelope provider resource is invalid")
	fake.envelopePreflightErr = preflightErr
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "creative_work", "work-envelope-preflight", messageWithImage(),
	)
	if !created || !errors.Is(err, preflightErr) {
		t.Fatalf("envelope preflight failure: created=%v batch=%+v err=%v", created, batch, err)
	}
	if len(fake.envelopePreflights) != 1 || len(fake.envelopeSends) != 0 || len(fake.sends) != 0 {
		t.Fatalf("envelope preflight failure crossed send boundary: preflights=%+v envelopes=%+v legacy=%+v",
			fake.envelopePreflights, fake.envelopeSends, fake.sends)
	}
	for _, receipt := range batch.Receipts {
		if receipt.Status != k12.DeliveryPending || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
			t.Fatalf("envelope preflight failure consumed an attempt: %+v", batch.Receipts)
		}
	}
}

func TestCreativeCompositeRootMutationAfterEnvelopePreflightLosesCAS(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	d.Delivery = fake
	batch := freezeCreativeCanonicalPartPrefix(t, d, fake, "root-mutated-after-preflight", 1, 2)

	fake.envelopePreflightHook = func([]k12.DeliveryReceipt) {
		changedDigest := partTestDigest("different-root-after-envelope-preflight")
		changedDedupe := partTestDigest(strings.Join([]string{
			batch.AgentName, batch.ObjectKind, batch.ObjectID, changedDigest,
		}, "\x00"))
		if _, err := d.Records.DB().Exec(
			`UPDATE k12_delivery_batches SET content_digest=?,dedupe_key=? WHERE agent_name=? AND batch_id=?`,
			changedDigest, changedDedupe, batch.AgentName, batch.BatchID,
		); err != nil {
			t.Fatal(err)
		}
	}

	processed, err := d.RecoverDeliveryReceipts(context.Background(), batch.AgentName)
	if !errors.Is(err, k12storage.ErrDeliveryBatchConflict) || processed != 0 {
		t.Fatalf("root mutation after envelope preflight must lose group CAS: processed=%d err=%v", processed, err)
	}
	if len(fake.envelopePreflights) != 1 || len(fake.envelopeSends) != 0 || len(fake.sends) != 0 {
		t.Fatalf("root mutation crossed provider boundary: preflights=%d envelopes=%d legacy=%d",
			len(fake.envelopePreflights), len(fake.envelopeSends), len(fake.sends))
	}
	current, getErr := d.GetDeliveryBatch(context.Background(), batch.AgentName, batch.BatchID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	for _, receipt := range current.Receipts {
		if receipt.Status != k12.DeliveryPending || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
			t.Fatalf("root mutation consumed component state: %+v", current.Receipts)
		}
	}
}

func TestCreativeCanonicalIntegrityPrecedesMediaPreparationForSendAndRetry(t *testing.T) {
	tests := []struct {
		name        string
		wantStatus  k12.DeliveryReceiptStatus
		wantAttempt int
		prepare     func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch, message usecase.DeliveryMessage)
		invoke      func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch, message usecase.DeliveryMessage) error
	}{
		{
			name:       "initial send",
			wantStatus: k12.DeliveryPending,
			prepare: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch, message usecase.DeliveryMessage) {
				t.Helper()
				dedupeKey := messageBatchDedupeForTest(
					t, batch.AgentName, batch.ObjectKind, batch.ObjectID, message,
				)
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_batches SET dedupe_key=? WHERE agent_name=? AND batch_id=?`,
					dedupeKey, batch.AgentName, batch.BatchID,
				); err != nil {
					t.Fatal(err)
				}
			},
			invoke: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch, message usecase.DeliveryMessage) error {
				t.Helper()
				_, created, err := d.PrepareAndSendMessageBatch(
					context.Background(), batch.AgentName, batch.ObjectKind, batch.ObjectID, message,
				)
				if created {
					t.Fatal("corrupt frozen batch replay unexpectedly created a new batch")
				}
				return err
			},
		},
		{
			name:        "retry",
			wantStatus:  k12.DeliveryFailed,
			wantAttempt: 1,
			prepare: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch, _ usecase.DeliveryMessage) {
				t.Helper()
				if _, err := d.Records.DB().Exec(
					`UPDATE k12_delivery_receipts SET status=?,attempt=1,last_error=? WHERE agent_name=? AND batch_id=?`,
					k12.DeliveryFailed, "provider rejected", batch.AgentName, batch.BatchID,
				); err != nil {
					t.Fatal(err)
				}
			},
			invoke: func(t *testing.T, d usecase.Deps, batch k12.DeliveryBatch, _ usecase.DeliveryMessage) error {
				t.Helper()
				_, err := d.RetryDeliveryBatch(context.Background(), batch.AgentName, batch.BatchID)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDataDeps(t)
			fake := newMessagePartTransport()
			fake.targets = fake.targets[:1]
			d.Delivery = fake
			message := messageWithImage()
			batch := freezeCreativeCanonicalPartPrefix(t, d, fake, "preflight-order-"+tt.name, 1, 2)
			if _, err := d.Records.DB().Exec(
				`UPDATE k12_delivery_receipts SET prepared_resource_id='' WHERE agent_name=? AND batch_id=?`,
				batch.AgentName, batch.BatchID,
			); err != nil {
				t.Fatal(err)
			}
			rewriteCreativePartPayload(t, d, batch, 0, func(payload map[string]any) {
				payload["text"] = "file:///private/tmp/leak"
			})
			tt.prepare(t, d, batch, message)

			err := tt.invoke(t, d, batch, message)
			if !errors.Is(err, records.ErrIllegalTransition) {
				t.Fatalf("corrupt canonical batch must fail before media preparation: %v", err)
			}
			if len(fake.preflightCalls) != 0 || len(fake.sends) != 0 || len(fake.envelopeSends) != 0 {
				t.Fatalf("corrupt canonical batch crossed provider boundary: preflight=%+v legacy=%+v envelopes=%+v",
					fake.preflightCalls, fake.sends, fake.envelopeSends)
			}
			current, getErr := d.GetDeliveryBatch(context.Background(), batch.AgentName, batch.BatchID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			for _, receipt := range current.Receipts {
				if receipt.Status != tt.wantStatus || receipt.Attempt != tt.wantAttempt ||
					receipt.ExternalMessageID != "" || receipt.PreparedResourceID != "" {
					t.Fatalf("corrupt canonical batch changed durable state before rejection: %+v", current.Receipts)
				}
			}
		})
	}
}

func TestCreativeCompositePreflightFailureAndRetryStayAtomic(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	failingKey := fake.targets[0].BindingID + "/2"
	fake.preflightFailures[failingKey] = errors.New("image preparation rejected")
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "creative_work", "work-preflight-retry", messageWithImage(),
	)
	if err == nil || !created || len(batch.Receipts) != 2 {
		t.Fatalf("preflight failure must retain both components: created=%v batch=%+v err=%v", created, batch, err)
	}
	if len(fake.envelopeSends) != 0 || len(fake.sends) != 0 {
		t.Fatalf("preflight failure produced a visible send: envelopes=%+v legacy=%+v", fake.envelopeSends, fake.sends)
	}
	for _, receipt := range batch.Receipts {
		if receipt.Status != k12.DeliveryFailed || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
			t.Fatalf("unsent creative group is not safely retryable: %+v", batch.Receipts)
		}
	}

	delete(fake.preflightFailures, failingKey)
	retried, err := d.RetryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil || retried.Status != k12.DeliveryBatchDelivered {
		t.Fatalf("retry creative envelope: batch=%+v err=%v", retried, err)
	}
	if fake.preflightCalls[failingKey] != 2 || len(fake.envelopeSends) != 1 || len(fake.sends) != 0 {
		t.Fatalf("safe group retry counts: preflight=%+v envelopes=%+v legacy=%+v",
			fake.preflightCalls, fake.envelopeSends, fake.sends)
	}
	assertSharedEnvelopeState(t, retried.Receipts, k12.DeliveryDelivered, 1)
	if _, err := d.RetryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID); err != nil {
		t.Fatal(err)
	}
	if len(fake.envelopeSends) != 1 || len(fake.sends) != 0 {
		t.Fatalf("terminal creative envelope retried physically: envelopes=%+v legacy=%+v", fake.envelopeSends, fake.sends)
	}
}

func TestCreativeCompositeCanceledSendPersistsWholeGroupOutcomeUnknown(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.envelopeSendHook = cancel
	key := fake.targets[0].BindingID
	fake.envelopeSendPlans[key] = []usecase.DeliveryTransportAck{{
		ExternalMessageID: "external:creative-canceled",
		Err:               context.Canceled,
	}}
	d.Delivery = fake

	_, created, err := d.PrepareAndSendMessageBatch(
		requestCtx, "xiaoming", "creative_work", "work-canceled", messageWithImage(),
	)
	if !created || (err != nil && !errors.Is(err, context.Canceled)) {
		t.Fatalf("canceled creative envelope must keep its frozen batch: created=%v err=%v", created, err)
	}
	if !fake.envelopeSendSawLiveCtx {
		t.Fatal("creative envelope provider send must begin with a live request context")
	}
	if len(fake.envelopeSends) != 1 || len(fake.sends) != 0 || len(fake.envelopeSends[0]) != 2 {
		t.Fatalf("canceled creative envelope crossed an unexpected provider boundary: envelopes=%+v legacy=%+v",
			fake.envelopeSends, fake.sends)
	}
	batchID := fake.envelopeSends[0][0].BatchID
	persisted, getErr := d.GetDeliveryBatch(context.Background(), "xiaoming", batchID)
	if getErr != nil || persisted.Status != k12.DeliveryBatchOutcomeUnknown || len(persisted.Receipts) != 2 {
		t.Fatalf("reload canceled creative envelope: batch=%+v err=%v", persisted, getErr)
	}
	first := persisted.Receipts[0]
	if first.Status != k12.DeliveryOutcomeUnknown || first.Attempt != 1 ||
		first.ExternalMessageID != "external:creative-canceled" || first.LastError == "" {
		t.Fatalf("canceled creative envelope did not persist queryable unknown evidence: %+v", first)
	}
	for _, receipt := range persisted.Receipts[1:] {
		if receipt.Status != first.Status || receipt.Attempt != first.Attempt ||
			receipt.ExternalMessageID != first.ExternalMessageID || receipt.LastError != first.LastError {
			t.Fatalf("canceled creative envelope persisted partial group state: %+v", persisted.Receipts)
		}
	}
}

func TestCreativeCompositeUnknownRestartQueriesOneEnvelopeWithoutResend(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	key := fake.targets[0].BindingID
	fake.envelopeSendPlans[key] = []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "external:creative-unknown",
		Detail: "provider response lost",
	}}
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "creative_work", "work-unknown", messageWithImage(),
	)
	if err != nil || !created || batch.Status != k12.DeliveryBatchOutcomeUnknown {
		t.Fatalf("seed creative envelope unknown: created=%v batch=%+v err=%v", created, batch, err)
	}
	assertSharedEnvelopeState(t, batch.Receipts, k12.DeliveryOutcomeUnknown, 1)
	if _, err := d.RetryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID); err != nil {
		t.Fatal(err)
	}
	if len(fake.envelopeSends) != 1 || len(fake.sends) != 0 {
		t.Fatalf("ordinary retry resent an unknown envelope: envelopes=%+v legacy=%+v", fake.envelopeSends, fake.sends)
	}

	restarted := d
	restartTransport := newMessagePartTransport()
	restartTransport.targets = restartTransport.targets[:1]
	restartTransport.envelopeQueryPlans[key] = []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryDelivered, ExternalMessageID: "external:creative-unknown",
	}}
	restarted.Delivery = restartTransport
	processed, err := restarted.RecoverDeliveryReceipts(context.Background(), "xiaoming")
	if err != nil || processed != 2 {
		t.Fatalf("restart recovery: processed=%d err=%v", processed, err)
	}
	if len(restartTransport.envelopeQueries) != 1 || len(restartTransport.queries) != 0 ||
		len(restartTransport.envelopeSends) != 0 || len(restartTransport.sends) != 0 {
		t.Fatalf("restart must query one envelope without resend: envelopeQueries=%+v queries=%+v envelopeSends=%+v sends=%+v",
			restartTransport.envelopeQueries, restartTransport.queries,
			restartTransport.envelopeSends, restartTransport.sends)
	}
	terminal, err := restarted.GetDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	assertSharedEnvelopeState(t, terminal.Receipts, k12.DeliveryDelivered, 1)
}

func TestCreativeCompositeQueryImageChildQueriesOneEnvelopeAndReturnsThatChild(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	key := fake.targets[0].BindingID
	fake.envelopeSendPlans[key] = []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "external:creative-child-query",
		Detail: "provider response lost",
	}}
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "creative_work", "work-child-query", messageWithImage(),
	)
	if err != nil || !created || len(batch.Receipts) != 2 {
		t.Fatalf("seed queryable creative envelope: created=%v batch=%+v err=%v", created, batch, err)
	}
	imageChild := batch.Receipts[1]
	fake.envelopeQueryPlans[key] = []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryDelivered, ExternalMessageID: "external:creative-child-query",
	}}

	queried, err := d.QueryDeliveryReceipt(context.Background(), batch.AgentName, imageChild.DeliveryID)
	if err != nil {
		t.Fatalf("query creative image child: %v", err)
	}
	if queried.DeliveryID != imageChild.DeliveryID || queried.PartKind != messagecontent.PartArtifact ||
		queried.Status != k12.DeliveryDelivered {
		t.Fatalf("query returned the wrong creative component: got=%+v want_id=%s", queried, imageChild.DeliveryID)
	}
	if len(fake.envelopeQueries) != 1 || len(fake.queries) != 0 ||
		len(fake.envelopeSends) != 1 || len(fake.sends) != 0 {
		t.Fatalf("image-child query must use one envelope query without resend: envelopeQueries=%+v queries=%+v envelopeSends=%+v sends=%+v",
			fake.envelopeQueries, fake.queries, fake.envelopeSends, fake.sends)
	}
	if group := fake.envelopeQueries[0]; len(group) != 2 ||
		group[0].PartKind != messagecontent.PartMarkdown || group[1].DeliveryID != imageChild.DeliveryID {
		t.Fatalf("image-child query lost the exact ordered envelope: %+v", group)
	}
	persisted, err := d.GetDeliveryBatch(context.Background(), batch.AgentName, batch.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	assertSharedEnvelopeState(t, persisted.Receipts, k12.DeliveryDelivered, 1)
}

func TestCreativeCompositeLeavesPDFAsIndependentPhysicalMessage(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "creative_work", "work-image-pdf", messageWithImageAndPDF(),
	)
	if err != nil || !created || batch.Status != k12.DeliveryBatchDelivered || len(batch.Receipts) != 3 {
		t.Fatalf("creative image+PDF delivery: created=%v batch=%+v err=%v", created, batch, err)
	}
	if len(fake.envelopeSends) != 1 || len(fake.sends) != 1 {
		t.Fatalf("creative Markdown+image and PDF need exactly two physical messages: envelopes=%+v legacy=%+v",
			fake.envelopeSends, fake.sends)
	}
	assertSharedEnvelopeState(t, batch.Receipts[:2], k12.DeliveryDelivered, 1)
	pdf := batch.Receipts[2]
	if pdf.PartKind != messagecontent.PartArtifact || pdf.PartOrdinal != 3 || pdf.PartMIME != "application/pdf" ||
		pdf.PreparedResourceID == "" || pdf.Status != k12.DeliveryDelivered || pdf.Attempt != 1 ||
		pdf.ExternalMessageID == "" || pdf.ExternalMessageID == batch.Receipts[0].ExternalMessageID {
		t.Fatalf("PDF did not remain an independent physical sampleFile: %+v", batch.Receipts)
	}
}

func TestDeliveryMessagePartsFreezeTwoTargetsByThreePartsBeforeVisibleSend(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "practice_set", "practice-1", messageWithImageAndPDF(),
	)
	if err != nil || !created {
		t.Fatalf("prepare target×part batch: created=%v batch=%+v err=%v", created, batch, err)
	}
	if batch.Status != k12.DeliveryBatchDelivered || len(batch.Receipts) != 6 {
		t.Fatalf("2 targets × 3 parts must converge as six receipts: %+v", batch)
	}
	wantKinds := []messagecontent.PartKind{
		messagecontent.PartMarkdown, messagecontent.PartArtifact, messagecontent.PartArtifact,
		messagecontent.PartMarkdown, messagecontent.PartArtifact, messagecontent.PartArtifact,
	}
	wantMIMEs := []string{"", "image/png", "application/pdf", "", "image/png", "application/pdf"}
	seenDedupe := make(map[string]struct{}, len(batch.Receipts))
	seenExternal := make(map[string]struct{}, len(batch.Receipts))
	for i, receipt := range batch.Receipts {
		if receipt.PartKind != wantKinds[i] || receipt.PartMIME != wantMIMEs[i] ||
			receipt.PartOrdinal != i%3+1 || receipt.PartDigest == "" {
			t.Fatalf("receipt %d lost canonical part identity: %+v", i, receipt)
		}
		if receipt.PartKind == messagecontent.PartArtifact && receipt.PreparedResourceID == "" {
			t.Fatalf("artifact receipt %d sent without durable prepared resource: %+v", i, receipt)
		}
		if receipt.PartKind == messagecontent.PartMarkdown && receipt.PreparedResourceID != "" {
			t.Fatalf("markdown receipt %d unexpectedly has media resource: %+v", i, receipt)
		}
		if _, duplicate := seenDedupe[receipt.DedupeKey]; duplicate {
			t.Fatalf("target×part dedupe collision at receipt %d: %s", i, receipt.DedupeKey)
		}
		seenDedupe[receipt.DedupeKey] = struct{}{}
		if _, duplicate := seenExternal[receipt.ExternalMessageID]; duplicate || receipt.ExternalMessageID == "" {
			t.Fatalf("each part needs an independent external message id: %+v", batch.Receipts)
		}
		seenExternal[receipt.ExternalMessageID] = struct{}{}
	}
	if len(fake.events) != 10 {
		t.Fatalf("want four media preflights followed by six sends, events=%v", fake.events)
	}
	for i := 0; i < 4; i++ {
		if !strings.HasPrefix(fake.events[i], "preflight:") {
			t.Fatalf("visible send crossed boundary before every media preflight: %v", fake.events)
		}
	}
	for i := 4; i < len(fake.events); i++ {
		if !strings.HasPrefix(fake.events[i], "send:") {
			t.Fatalf("media preflight happened after visible send began: %v", fake.events)
		}
	}
}

func TestDeliveryMessagePartsPreflightFailureSendsNothingAndRetryUploadsOnlyMissingResource(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	failingKey := fake.targets[0].BindingID + "/3"
	fake.preflightFailures[failingKey] = errors.New("Drive upload rejected")
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "practice_set", "practice-preflight", messageWithImageAndPDF(),
	)
	if err == nil || !created || len(batch.Receipts) != 6 {
		t.Fatalf("preflight failure must retain the complete frozen batch: created=%v batch=%+v err=%v", created, batch, err)
	}
	if len(fake.sends) != 0 {
		t.Fatalf("media preflight failure must keep every visible provider send at zero: %+v", fake.sends)
	}
	for _, receipt := range batch.Receipts {
		if receipt.Status != k12.DeliveryFailed || receipt.ExternalMessageID != "" {
			t.Fatalf("preflight failure must make every unsent child explicitly retryable: %+v", batch.Receipts)
		}
	}
	if len(fake.preflightCalls) != 4 {
		t.Fatalf("all media parts must be preflighted before deciding the barrier: %+v", fake.preflightCalls)
	}

	delete(fake.preflightFailures, failingKey)
	retried, err := d.RetryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil || retried.Status != k12.DeliveryBatchDelivered {
		t.Fatalf("batch retry did not converge: batch=%+v err=%v", retried, err)
	}
	for key, calls := range fake.preflightCalls {
		want := 1
		if key == failingKey {
			want = 2
		}
		if calls != want {
			t.Fatalf("retry reuploaded an already prepared media part: key=%s calls=%d want=%d", key, calls, want)
		}
	}
	if len(fake.sends) != 6 {
		t.Fatalf("zero-send failed batch must send each frozen part once after recovery: %+v", fake.sends)
	}
}

func TestDeliveryMessagePartsPartialFailureRetriesOnlyFailedPart(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	failingKey := fake.targets[1].BindingID + "/3"
	fake.sendPlans[failingKey] = []usecase.DeliveryTransportAck{
		{Status: k12.DeliveryFailed, Detail: "Drive file send rejected"},
		{Status: k12.DeliveryDelivered, ExternalMessageID: "external:retried-pdf"},
	}
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "practice_set", "practice-partial", messageWithImageAndPDF(),
	)
	if err != nil || !created || batch.Status != k12.DeliveryBatchPartialFailed || len(fake.sends) != 6 {
		t.Fatalf("seed one failed part: created=%v batch=%+v sends=%d err=%v", created, batch, len(fake.sends), err)
	}
	var failed k12.DeliveryReceipt
	for _, receipt := range batch.Receipts {
		if receipt.Status == k12.DeliveryFailed {
			failed = receipt
		}
	}
	if failed.DeliveryID == "" || messagePartKey(failed) != failingKey {
		t.Fatalf("wrong failed child: %+v", batch.Receipts)
	}
	retriedChild, err := d.RetryDeliveryReceipt(context.Background(), "xiaoming", failed.DeliveryID)
	if err != nil || retriedChild.Status != k12.DeliveryDelivered {
		t.Fatalf("legacy child retry must delegate to the safe batch barrier: child=%+v err=%v", retriedChild, err)
	}
	retried, err := d.GetDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil || retried.Status != k12.DeliveryBatchDelivered {
		t.Fatalf("delegated failed-only batch retry: batch=%+v err=%v", retried, err)
	}
	for _, receipt := range retried.Receipts {
		want := 1
		if messagePartKey(receipt) == failingKey {
			want = 2
		}
		if got := countMessagePartSends(fake.sends, messagePartKey(receipt)); got != want {
			t.Fatalf("retry send count for %s=%d want=%d", messagePartKey(receipt), got, want)
		}
	}
	for key, calls := range fake.preflightCalls {
		if calls != 1 {
			t.Fatalf("provider-send retry must not reupload prepared media %s: calls=%d", key, calls)
		}
	}
}

func TestDeliveryMessagePartsDefiniteTransportFailureDoesNotStrandLaterParts(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	failingKey := fake.targets[0].BindingID + "/2"
	fake.sendPlans[failingKey] = []usecase.DeliveryTransportAck{
		{
			Status: k12.DeliveryFailed, Detail: "image send rejected",
			Err: errors.New("provider rejected image send"),
		},
		{Status: k12.DeliveryDelivered, ExternalMessageID: "external:retried-image"},
	}
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "practice_set", "work-definite-failure", messageWithImageAndPDF(),
	)
	if err != nil || !created || batch.Status != k12.DeliveryBatchPartialFailed {
		t.Fatalf("definite provider failure must retain a retryable batch: created=%v batch=%+v err=%v", created, batch, err)
	}
	if len(fake.sends) != 6 {
		t.Fatalf("definite part failure stranded later frozen parts: sends=%+v", fake.sends)
	}
	for _, receipt := range batch.Receipts {
		if receipt.Status == k12.DeliveryPending {
			t.Fatalf("first pass left a later part pending after definite failure: %+v", batch.Receipts)
		}
	}

	retried, err := d.RetryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil || retried.Status != k12.DeliveryBatchDelivered {
		t.Fatalf("failed-only retry did not converge: batch=%+v err=%v", retried, err)
	}
	for _, receipt := range retried.Receipts {
		want := 1
		if messagePartKey(receipt) == failingKey {
			want = 2
		}
		if got := countMessagePartSends(fake.sends, messagePartKey(receipt)); got != want {
			t.Fatalf("retry send count for %s=%d want=%d", messagePartKey(receipt), got, want)
		}
	}
}

func TestDeliveryMessagePartsUnknownIsQueryOnly(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	unknownKey := fake.targets[0].BindingID + "/2"
	fake.sendPlans[unknownKey] = []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "external:unknown-image",
		Detail: "provider response lost",
	}}
	d.Delivery = fake

	batch, _, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "practice_set", "practice-unknown", messageWithImageAndPDF(),
	)
	if err != nil || batch.Status != k12.DeliveryBatchOutcomeUnknown {
		t.Fatalf("seed unknown part: batch=%+v err=%v", batch, err)
	}
	if _, err := d.RetryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID); err != nil {
		t.Fatal(err)
	}
	if len(fake.sends) != 6 {
		t.Fatalf("batch retry blindly resent unknown part: %+v", fake.sends)
	}
	resolved, err := d.QueryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil || resolved.Status != k12.DeliveryBatchDelivered || len(fake.queries) != 1 ||
		messagePartKey(fake.queries[0]) != unknownKey {
		t.Fatalf("query-only convergence failed: batch=%+v queries=%+v err=%v", resolved, fake.queries, err)
	}
}

func TestDeliveryMessagePartsIdentityReplayDoesNotPrepareOrResolveMutableInputs(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	d.Delivery = fake
	message := messageWithImageAndPDF()

	first, _, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "practice_set", "practice-replay", message,
	)
	if err != nil {
		t.Fatal(err)
	}
	beforeResolve, beforePrepare := fake.resolveCalls, fake.prepareCalls
	beforeSends, beforePreflights := len(fake.sends), len(fake.events)-len(fake.sends)
	identities := make([]usecase.DeliveryAttachmentIdentity, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		identities = append(identities, usecase.DeliveryAttachmentIdentity{
			Name: attachment.Name, MIME: attachment.MIME,
			ContentDigest: partTestDigestBytes(attachment.Data),
		})
	}
	replayed, err := d.ReplayDeliveryBatchForMessageIdentity(
		context.Background(), "xiaoming", "practice_set", "practice-replay", message.Content, identities,
	)
	if err != nil || replayed.BatchID != first.BatchID {
		t.Fatalf("frozen identity replay: batch=%+v err=%v", replayed, err)
	}
	if fake.resolveCalls != beforeResolve || fake.prepareCalls != beforePrepare ||
		len(fake.sends) != beforeSends || len(fake.events)-len(fake.sends) != beforePreflights {
		t.Fatalf("replay crossed mutable preparation/provider boundaries: resolve=%d/%d prepare=%d/%d sends=%d/%d preflights=%d/%d",
			fake.resolveCalls, beforeResolve, fake.prepareCalls, beforePrepare,
			len(fake.sends), beforeSends, len(fake.events)-len(fake.sends), beforePreflights)
	}
	for i := range first.Receipts {
		if replayed.Receipts[i].DeliveryID != first.Receipts[i].DeliveryID ||
			replayed.Receipts[i].PayloadJSON != first.Receipts[i].PayloadJSON ||
			replayed.Receipts[i].PartDigest != first.Receipts[i].PartDigest {
			t.Fatalf("replay changed frozen part %d: first=%+v replay=%+v", i, first.Receipts[i], replayed.Receipts[i])
		}
	}
}

func TestDeliveryMessagePartsChildInsertFailurePreventsMediaAndVisibleProviderCalls(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	d.Delivery = fake
	if _, err := d.Records.DB().Exec(`CREATE TRIGGER fail_delivery_message_part_child
		BEFORE INSERT ON k12_delivery_receipts
		WHEN NEW.batch_id != '' AND NEW.batch_ordinal = 2
		BEGIN SELECT RAISE(ABORT, 'forced message part child failure'); END`); err != nil {
		t.Fatal(err)
	}

	_, created, err := d.PrepareAndSendMessageBatch(
		context.Background(), "xiaoming", "practice_set", "practice-atomic", messageWithImageAndPDF(),
	)
	if err == nil || created {
		t.Fatalf("child insert failure must abort the batch: created=%v err=%v", created, err)
	}
	if len(fake.preflightCalls) != 0 || len(fake.sends) != 0 {
		t.Fatalf("provider boundary crossed before atomic child commit: preflight=%v sends=%v", fake.preflightCalls, fake.sends)
	}
	var batches, receipts int
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_delivery_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_delivery_receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || receipts != 0 {
		t.Fatalf("failed child insert left partial delivery state: batches=%d receipts=%d", batches, receipts)
	}
}

func TestDeliveryMessagePartsRestartRecoveryUsesBatchPreflightBeforePendingMarkdown(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	fake.targets = fake.targets[:1]
	d.Delivery = fake
	message := usecase.DeliveryMessage{
		Content: "## 作品点评",
		Attachments: []usecase.DeliveryAttachment{
			{Name: "work.png", MIME: "image/png", Data: []byte("work-image")},
		},
	}
	prepared, err := fake.PrepareMessageForTargets(context.Background(), message, fake.targets)
	if err != nil {
		t.Fatal(err)
	}
	var firstPart channel.DeliveryPart
	if err := json.Unmarshal([]byte(prepared[0].PayloadJSON), &firstPart); err != nil {
		t.Fatal(err)
	}
	contentDigest, batchDedupeKey := canonicalBatchIdentityForTest(
		t, "xiaoming", "creative_work", "work-1", firstPart.MessageContent,
	)
	batch := k12.DeliveryBatch{
		BatchID: "restart-parts", AgentName: "xiaoming",
		ObjectKind: "creative_work", ObjectID: "work-1",
		DedupeKey: batchDedupeKey, ContentDigest: contentDigest,
	}
	for i, part := range prepared {
		receipt := k12.DeliveryReceipt{
			DeliveryID: fmt.Sprintf("restart-part-%d", i+1),
			PartKind:   part.PartKind, PartMIME: part.PartMIME,
			PartOrdinal: part.PartOrdinal, PartDigest: part.PartDigest,
			BindingID: part.BindingID, Target: part.Target,
			PayloadDigest: partTestDigest(part.PayloadJSON),
			PayloadJSON:   part.PayloadJSON, RenderJSON: part.RenderJSON,
			AgentName: batch.AgentName, ObjectKind: batch.ObjectKind, ObjectID: batch.ObjectID,
		}
		receipt.DedupeKey = deliveryPartDedupeForTest(receipt, receipt.PayloadDigest)
		batch.Receipts = append(batch.Receipts, receipt)
	}
	if _, created, err := d.Records.PrepareDeliveryBatch(context.Background(), batch); err != nil || !created {
		t.Fatalf("freeze restart batch: created=%v err=%v", created, err)
	}
	fake.events = nil
	fake.sends = nil
	fake.preflightCalls = make(map[string]int)

	processed, err := d.RecoverDeliveryReceipts(context.Background(), "xiaoming")
	if err != nil || processed != 2 {
		t.Fatalf("recover target×part batch: processed=%d err=%v events=%v", processed, err, fake.events)
	}
	recovered, err := d.GetDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil || recovered.Status != k12.DeliveryBatchDelivered {
		t.Fatalf("recovered batch did not converge: batch=%+v err=%v", recovered, err)
	}
	if len(fake.events) != 2 || !strings.HasPrefix(fake.events[0], "preflight:") ||
		!strings.HasPrefix(fake.events[1], "envelope-send:") ||
		len(fake.sends) != 0 || len(fake.envelopeSends) != 1 {
		t.Fatalf("restart sent pending Markdown before batch media preflight: %v", fake.events)
	}
}
