package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type batchTransport struct {
	targets      []usecase.ResolvedDeliveryTarget
	resolveCalls int
	prepareCalls int
	contents     []string
	send         []usecase.DeliveryTransportAck
	query        []usecase.DeliveryTransportAck
	sends        []k12.DeliveryReceipt
	queries      []k12.DeliveryReceipt
}

func (f *batchTransport) ResolveTextTargets(_ context.Context, _ string) ([]usecase.ResolvedDeliveryTarget, error) {
	f.resolveCalls++
	return append([]usecase.ResolvedDeliveryTarget(nil), f.targets...), nil
}

func (f *batchTransport) PrepareTextForTargets(
	_ context.Context,
	content string,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	f.prepareCalls++
	f.contents = append(f.contents, content)
	out := make([]usecase.PreparedTextDelivery, 0, len(targets))
	for _, target := range targets {
		out = append(out, usecase.PreparedTextDelivery{
			BindingID:   target.BindingID,
			Target:      target.Target,
			PayloadJSON: fmt.Sprintf(`{"binding":%q,"content":%q}`, target.BindingID, content),
			RenderJSON:  `{"surface":"channel"}`,
		})
	}
	return out, nil
}

func (*batchTransport) PrepareText(context.Context, string, string) (usecase.PreparedTextDelivery, error) {
	return usecase.PreparedTextDelivery{}, errors.New("legacy singleton preparation must not be used")
}

func (f *batchTransport) SendPrepared(_ context.Context, receipt k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	f.sends = append(f.sends, receipt)
	if len(f.send) == 0 {
		return usecase.DeliveryTransportAck{}, errors.New("unexpected send")
	}
	ack := f.send[0]
	f.send = f.send[1:]
	return ack, ack.Err
}

func (f *batchTransport) QueryPrepared(_ context.Context, receipt k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	f.queries = append(f.queries, receipt)
	if len(f.query) == 0 {
		return usecase.DeliveryTransportAck{}, errors.New("unexpected query")
	}
	ack := f.query[0]
	f.query = f.query[1:]
	return ack, ack.Err
}

func batchTargets() []usecase.ResolvedDeliveryTarget {
	return []usecase.ResolvedDeliveryTarget{
		{
			BindingID: "agent-rule:1",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-a", ChatID: "parent", Label: "dingtalk",
			},
		},
		{
			BindingID: "agent-rule:2",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-b", ChatID: "parent", Label: "dingtalk",
			},
		},
	}
}

func attachDeliveredPracticeTransport(d *usecase.Deps, sends int) *batchTransport {
	fake := &batchTransport{
		targets: batchTargets()[:1],
		send:    make([]usecase.DeliveryTransportAck, sends),
	}
	for i := range fake.send {
		fake.send[i] = usecase.DeliveryTransportAck{
			Status: k12.DeliveryDelivered, ExternalMessageID: fmt.Sprintf("practice-%d", i+1),
		}
	}
	d.Delivery = fake
	return fake
}

func TestDeliveryBatchReplayAndPartialRetryOnlySendFailedChild(t *testing.T) {
	d := newDataDeps(t)
	fake := &batchTransport{
		targets: batchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "provider-a"},
			{Status: k12.DeliveryFailed, Detail: "provider rejected bot-b"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "provider-b"},
		},
	}
	d.Delivery = fake

	batch, created, err := d.PrepareAndSendTextBatch(
		context.Background(), "xiaoming", "tutoring_tips", "tips-1", "辅导要点",
	)
	if err != nil || !created {
		t.Fatalf("prepare batch: created=%v batch=%+v err=%v", created, batch, err)
	}
	if batch.Status != k12.DeliveryBatchPartialFailed || len(batch.Receipts) != 2 || len(fake.sends) != 2 {
		t.Fatalf("first batch must preserve one delivered + one failed: %+v sends=%d", batch, len(fake.sends))
	}
	if fake.sends[0].Target.InstanceID == fake.sends[1].Target.InstanceID {
		t.Fatalf("different platform instances must each receive once: %+v", fake.sends)
	}

	replay, created, err := d.PrepareAndSendTextBatch(
		context.Background(), "xiaoming", "tutoring_tips", "tips-1", "辅导要点",
	)
	if err != nil || created || replay.BatchID != batch.BatchID {
		t.Fatalf("HTTP replay must return frozen batch: created=%v batch=%+v err=%v", created, replay, err)
	}
	if fake.resolveCalls != 1 || fake.prepareCalls != 1 || len(fake.sends) != 2 {
		t.Fatalf("replay must not resolve mutable bindings or resend: resolve=%d prepare=%d sends=%d",
			fake.resolveCalls, fake.prepareCalls, len(fake.sends))
	}

	retried, err := d.RetryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != k12.DeliveryBatchDelivered || len(fake.sends) != 3 {
		t.Fatalf("failed-only retry must converge without resending delivered child: %+v sends=%d",
			retried, len(fake.sends))
	}
	if fake.sends[2].Target.InstanceID != "bot-b" || fake.sends[2].Attempt != 2 {
		t.Fatalf("retry reached wrong child or did not reuse receipt: %+v", fake.sends[2])
	}
}

func TestDeliveryBatchUnknownIsQueryOnlyAndZeroBindingHasNoDomainSideEffect(t *testing.T) {
	d := newDataDeps(t)
	fake := &batchTransport{
		targets: batchTargets(),
		send: []usecase.DeliveryTransportAck{
			{
				Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "provider-a",
				Detail: "timeout after provider request",
			},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "provider-b"},
		},
		query: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "provider-a"},
		},
	}
	d.Delivery = fake
	batch, _, err := d.PrepareAndSendTextBatch(
		context.Background(), "xiaoming", "accumulation", "accumulation-1", "积累内容",
	)
	if err != nil || batch.Status != k12.DeliveryBatchOutcomeUnknown {
		t.Fatalf("seed unknown batch: %+v err=%v", batch, err)
	}
	if _, err := d.RetryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID); err != nil {
		t.Fatal(err)
	}
	if len(fake.sends) != 2 || len(fake.queries) != 0 {
		t.Fatalf("batch retry must neither resend nor query unknown: sends=%d queries=%d",
			len(fake.sends), len(fake.queries))
	}
	resolved, err := d.QueryDeliveryBatch(context.Background(), "xiaoming", batch.BatchID)
	if err != nil || resolved.Status != k12.DeliveryBatchDelivered {
		t.Fatalf("query-only convergence: %+v err=%v", resolved, err)
	}
	if len(fake.sends) != 2 || len(fake.queries) != 1 ||
		fake.queries[0].Target.InstanceID != "bot-a" {
		t.Fatalf("query must touch only unknown child: sends=%d queries=%+v", len(fake.sends), fake.queries)
	}

	empty := &batchTransport{}
	d.Delivery = empty
	before := countDeliveryDomainRows(t, d)
	_, created, err := d.PrepareAndSendTextBatch(
		context.Background(), "xiaoming", "tutoring_tips", "tips-empty", "无人接收",
	)
	if !errors.Is(err, usecase.ErrNoActiveDirectBindings) || created {
		t.Fatalf("zero binding must fail before persistence: created=%v err=%v", created, err)
	}
	after := countDeliveryDomainRows(t, d)
	if before != after {
		t.Fatalf("zero binding changed delivery domain rows: before=%d after=%d", before, after)
	}
}

func countDeliveryDomainRows(t *testing.T, d usecase.Deps) int {
	t.Helper()
	var batches, receipts int
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_delivery_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_delivery_receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	return batches + receipts
}

func TestPracticeFinalizeDeliveryTransactionFailureRollsBackEverythingBeforeProviderCall(t *testing.T) {
	tests := []struct {
		triggerName string
		name        string
		failSQL     string
		wantMarker  string
	}{
		{
			name:        "batch root insert",
			triggerName: "fail_practice_delivery_batch_root",
			failSQL: `CREATE TRIGGER fail_practice_delivery_batch_root
				BEFORE INSERT ON k12_delivery_batches
				WHEN NEW.object_kind = 'practice_set_question'
				BEGIN SELECT RAISE(ABORT, 'forced batch root failure'); END`,
			wantMarker: "forced batch root failure",
		},
		{
			name:        "second batch child insert",
			triggerName: "fail_practice_delivery_batch_child",
			failSQL: `CREATE TRIGGER fail_practice_delivery_batch_child
				BEFORE INSERT ON k12_delivery_receipts
				WHEN NEW.batch_id != '' AND NEW.batch_ordinal = 2
				BEGIN SELECT RAISE(ABORT, 'forced batch child failure'); END`,
			wantMarker: "forced batch child failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDataDeps(t)
			fake := &batchTransport{
				targets: batchTargets(),
				send: []usecase.DeliveryTransportAck{
					{Status: k12.DeliveryDelivered, ExternalMessageID: "must-not-send-a"},
					{Status: k12.DeliveryDelivered, ExternalMessageID: "must-not-send-b"},
				},
			}
			d.Delivery = fake
			id, created, err := d.CreatePracticeSet(
				context.Background(),
				"xiaoming",
				"atomic-finalize",
				k12.PracticeSetFields{
					SourceKind: k12.PracticeSourceWeekly,
					Title:      "原子发送卷",
					Items:      []k12.PracticeItem{verifiedItem("q1", "1+1=?", "2")},
				},
			)
			if err != nil || !created {
				t.Fatalf("seed practice set: created=%v err=%v", created, err)
			}
			before, err := d.GetPracticeSet(context.Background(), "xiaoming", id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Records.DB().Exec(tt.failSQL); err != nil {
				t.Fatal(err)
			}

			_, _, err = d.FinalizeBasket(context.Background(), "xiaoming", id, "send")
			if err == nil || !strings.Contains(err.Error(), tt.wantMarker) {
				t.Fatalf("finalize error=%v want marker %q", err, tt.wantMarker)
			}
			if len(fake.sends) != 0 {
				t.Fatalf("provider called before atomic commit: sends=%+v", fake.sends)
			}
			got, getErr := d.GetPracticeSet(context.Background(), "xiaoming", id)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if got.Record.Status != k12.PracticeStatusDraft ||
				got.Record.Version != before.Record.Version ||
				got.Fields.PaperNo != "" ||
				got.Fields.DeliveryBatchID != "" ||
				got.Fields.FinalizedAt != 0 ||
				got.Fields.FinalizedVia != "" ||
				got.Fields.QuestionArtifact != "" ||
				got.Fields.AnswerArtifact != "" {
				t.Fatalf("failed atomic finalize mutated PracticeSet: record=%+v fields=%+v",
					got.Record, got.Fields)
			}
			if got.Fields.Items[0].PaperSeq != 0 ||
				got.Fields.Items[0].PracticeProblemID != "" {
				t.Fatalf("failed atomic finalize mutated PracticeSet child: %+v", got.Fields.Items[0])
			}
			if rows := countDeliveryDomainRows(t, d); rows != 0 {
				t.Fatalf("failed atomic finalize left batch rows=%d", rows)
			}
			var paperCounterRows int
			if err := d.Records.DB().QueryRow(
				`SELECT count(*) FROM k12_paper_no_counters WHERE agent_name='xiaoming'`,
			).Scan(&paperCounterRows); err != nil {
				t.Fatal(err)
			}
			if paperCounterRows != 0 {
				t.Fatalf("failed atomic finalize consumed paper number counter rows=%d", paperCounterRows)
			}

			if _, err := d.Records.DB().Exec(`DROP TRIGGER ` + tt.triggerName); err != nil {
				t.Fatal(err)
			}
			retried, skipped, err := d.FinalizeBasket(
				context.Background(), "xiaoming", id, "send",
			)
			if err != nil || skipped != 0 {
				t.Fatalf("retry after rollback: skipped=%d set=%+v err=%v", skipped, retried, err)
			}
			wantPaperNo := k12.FormatPaperNo(time.Unix(1000, 0), 1)
			if retried.Record.Status != k12.PracticeStatusAssigned ||
				retried.Fields.PaperNo != wantPaperNo ||
				len(fake.sends) != len(batchTargets()) {
				t.Fatalf("retry must consume first paper number and send frozen children: set=%+v sends=%d",
					retried, len(fake.sends))
			}
		})
	}
}

func TestPracticeFinalizeReplayUsesFrozenBatchWithoutRebindingOrResending(t *testing.T) {
	d := newDataDeps(t)
	fake := &batchTransport{
		targets: batchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "paper-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "paper-b"},
		},
	}
	d.Delivery = fake
	id, created, err := d.CreatePracticeSet(
		context.Background(),
		"xiaoming",
		"atomic-finalize-replay",
		k12.PracticeSetFields{
			SourceKind: k12.PracticeSourceWeekly,
			Title:      "原子发送重放卷",
			Items:      []k12.PracticeItem{verifiedItem("q1", "1+1=?", "2")},
		},
	)
	if err != nil || !created {
		t.Fatalf("seed practice set: created=%v err=%v", created, err)
	}

	first, _, err := d.FinalizeBasket(context.Background(), "xiaoming", id, "send")
	if err != nil {
		t.Fatal(err)
	}
	if first.Fields.DeliveryBatchID == "" || len(fake.sends) != 2 {
		t.Fatalf("first finalize did not freeze and send two children: set=%+v sends=%d",
			first, len(fake.sends))
	}

	// Mutable bindings can disappear after the command commits. Replaying the
	// same finalize must read the linked immutable batch before consulting them.
	fake.targets = nil
	replay, _, err := d.FinalizeBasket(context.Background(), "xiaoming", id, "send")
	if err != nil {
		t.Fatal(err)
	}
	if replay.Fields.DeliveryBatchID != first.Fields.DeliveryBatchID ||
		replay.Fields.PaperNo != first.Fields.PaperNo ||
		fake.resolveCalls != 1 ||
		fake.prepareCalls != 1 ||
		len(fake.sends) != 2 {
		t.Fatalf("replay rebound, rebuilt or resent: first=%+v replay=%+v resolve=%d prepare=%d sends=%d",
			first, replay, fake.resolveCalls, fake.prepareCalls, len(fake.sends))
	}
	if rows := countDeliveryDomainRows(t, d); rows != 3 {
		t.Fatalf("replay changed frozen root/children row count: %d", rows)
	}
}
