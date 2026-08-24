package usecase_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type messagePartTransport struct {
	targets           []usecase.ResolvedDeliveryTarget
	resolveCalls      int
	prepareCalls      int
	preflightCalls    map[string]int
	preflightFailures map[string]error
	sendPlans         map[string][]usecase.DeliveryTransportAck
	sends             []k12.DeliveryReceipt
	queries           []k12.DeliveryReceipt
	events            []string
}

func newMessagePartTransport() *messagePartTransport {
	return &messagePartTransport{
		targets:           batchTargets(),
		preflightCalls:    make(map[string]int),
		preflightFailures: make(map[string]error),
		sendPlans:         make(map[string][]usecase.DeliveryTransportAck),
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
	out := make([]usecase.PreparedTextDelivery, 0, len(targets)*(len(message.Attachments)+1))
	for _, target := range targets {
		markdownPayload := mustPartPayload(map[string]any{
			"kind": "markdown", "binding": target.BindingID, "content": message.Content,
		})
		out = append(out, preparedPart(
			target, messagecontent.PartMarkdown, "", 1,
			partTestDigest(strings.TrimSpace(message.Content)), markdownPayload,
		))
		for i, attachment := range message.Attachments {
			payload := mustPartPayload(map[string]any{
				"kind": "artifact", "binding": target.BindingID,
				"name": attachment.Name, "mime": attachment.MIME, "data": attachment.Data,
			})
			out = append(out, preparedPart(
				target, messagecontent.PartArtifact, attachment.MIME, i+2,
				partTestDigestBytes(attachment.Data), payload,
			))
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

func messageWithImageAndPDF() usecase.DeliveryMessage {
	return usecase.DeliveryMessage{
		Content: "## 本周练习\n\n请完成后订正。",
		Attachments: []usecase.DeliveryAttachment{
			{Name: "page-1.png", MIME: "image/png", Data: []byte("png-bytes")},
			{Name: "practice.pdf", MIME: "application/pdf", Data: []byte("pdf-bytes")},
		},
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
		context.Background(), "xiaoming", "creative_work", "work-definite-failure", messageWithImageAndPDF(),
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
	batch := k12.DeliveryBatch{
		BatchID: "restart-parts", AgentName: "xiaoming",
		ObjectKind: "creative_work", ObjectID: "work-1",
		DedupeKey: "restart-parts-dedupe", ContentDigest: partTestDigest("restart-content"),
	}
	for i, part := range prepared {
		batch.Receipts = append(batch.Receipts, k12.DeliveryReceipt{
			DeliveryID: fmt.Sprintf("restart-part-%d", i+1),
			PartKind:   part.PartKind, PartMIME: part.PartMIME,
			PartOrdinal: part.PartOrdinal, PartDigest: part.PartDigest,
			BindingID: part.BindingID, Target: part.Target,
			DedupeKey:     fmt.Sprintf("restart-part-dedupe-%d", i+1),
			PayloadDigest: partTestDigest(part.PayloadJSON),
			PayloadJSON:   part.PayloadJSON, RenderJSON: part.RenderJSON,
		})
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
	if len(fake.events) != 3 || !strings.HasPrefix(fake.events[0], "preflight:") ||
		!strings.HasPrefix(fake.events[1], "send:") || !strings.HasPrefix(fake.events[2], "send:") {
		t.Fatalf("restart sent pending Markdown before batch media preflight: %v", fake.events)
	}
}
