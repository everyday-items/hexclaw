package k12storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

func deliveryFixture(agent, deliveryID, dedupe string) k12.DeliveryReceipt {
	return k12.DeliveryReceipt{
		DeliveryID: deliveryID,
		AgentName:  agent,
		ObjectKind: "creative_work_feedback",
		ObjectID:   "work-1",
		BindingID:  "binding-1",
		Target: k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: "pi-1", ChatID: "user-1", Label: "妈妈的钉钉",
		},
		DedupeKey:     dedupe,
		PayloadDigest: "sha256:payload-1",
		PayloadJSON:   `{"markdown":"辅导要点"}`,
		RenderJSON:    `{"render_id":"render-1"}`,
		CreatedAt:     100,
		UpdatedAt:     100,
	}
}

func setupDeliveryStore(t *testing.T) *k12storage.Store {
	t.Helper()
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12DeliveryReceiptsV21DDL); err != nil {
		t.Fatalf("delivery receipt ddl: %v", err)
	}
	return store
}

func TestDeliveryReceiptPrepareIsOwnerScopedIdempotentAndImmutable(t *testing.T) {
	store := setupDeliveryStore(t)
	ctx := context.Background()
	want := deliveryFixture("mingming", "delivery-1", "work-feedback:work-1:v1")
	first, created, err := store.PrepareDeliveryReceipt(ctx, want)
	if err != nil || !created || first.Status != k12.DeliveryPending || first.Attempt != 0 {
		t.Fatalf("first=%+v created=%v err=%v", first, created, err)
	}
	replay := want
	replay.DeliveryID = "attacker-id"
	got, created, err := store.PrepareDeliveryReceipt(ctx, replay)
	if err != nil || created || got.DeliveryID != first.DeliveryID {
		t.Fatalf("replay=%+v created=%v err=%v", got, created, err)
	}

	changed := want
	changed.DeliveryID = "changed-id"
	changed.PayloadDigest = "sha256:different"
	if _, _, err := store.PrepareDeliveryReceipt(ctx, changed); !errors.Is(err, k12storage.ErrDeliveryReceiptConflict) {
		t.Fatalf("changed payload under same dedupe key must fail closed: %v", err)
	}

	other := deliveryFixture("lele", "delivery-other", want.DedupeKey)
	other.ObjectID = "work-other"
	if _, created, err := store.PrepareDeliveryReceipt(ctx, other); err != nil || !created {
		t.Fatalf("same dedupe key in another owner must remain isolated: created=%v err=%v", created, err)
	}
	if _, err := store.GetDeliveryReceipt(ctx, "lele", first.DeliveryID); err == nil {
		t.Fatal("cross-owner delivery lookup must not succeed")
	}
}

func TestDeliveryReceiptAcceptedIsNotDeliveredAndFailedRetryReusesRow(t *testing.T) {
	store := setupDeliveryStore(t)
	ctx := context.Background()
	receipt, _, err := store.PrepareDeliveryReceipt(ctx, deliveryFixture("mingming", "delivery-1", "dedupe-1"))
	if err != nil {
		t.Fatal(err)
	}
	sending, started, err := store.BeginDeliveryAttempt(ctx, "mingming", receipt.DeliveryID)
	if err != nil || !started || sending.Status != k12.DeliverySending || sending.Attempt != 1 {
		t.Fatalf("sending=%+v started=%v err=%v", sending, started, err)
	}
	accepted, err := store.MarkDeliveryAccepted(ctx, "mingming", receipt.DeliveryID, "pqk-1")
	if err != nil || accepted.Status != k12.DeliverySending || accepted.ExternalMessageID != "pqk-1" {
		t.Fatalf("accepted must remain sending: %+v err=%v", accepted, err)
	}
	failed, err := store.MarkDeliveryFailed(ctx, "mingming", receipt.DeliveryID, "provider rejected")
	if err != nil || failed.Status != k12.DeliveryFailed {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	retry, started, err := store.BeginDeliveryAttempt(ctx, "mingming", receipt.DeliveryID)
	if err != nil || !started || retry.DeliveryID != receipt.DeliveryID || retry.Attempt != 2 || retry.ExternalMessageID != "" {
		t.Fatalf("retry=%+v started=%v err=%v", retry, started, err)
	}
	accepted, err = store.MarkDeliveryAccepted(ctx, "mingming", receipt.DeliveryID, "pqk-2")
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := store.MarkDeliveryDelivered(ctx, "mingming", receipt.DeliveryID)
	if err != nil || delivered.Status != k12.DeliveryDelivered || delivered.ExternalMessageID != "pqk-2" {
		t.Fatalf("delivered=%+v err=%v", delivered, err)
	}
	if _, started, err := store.BeginDeliveryAttempt(ctx, "mingming", receipt.DeliveryID); err == nil || started {
		t.Fatalf("terminal delivered receipt must not resend: started=%v err=%v", started, err)
	}
}

func TestDeliveryReceiptOutcomeUnknownBlocksBlindRetryAndSurvivesRestart(t *testing.T) {
	store := setupDeliveryStore(t)
	ctx := context.Background()
	receipt, _, err := store.PrepareDeliveryReceipt(ctx, deliveryFixture("mingming", "delivery-unknown", "dedupe-unknown"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginDeliveryAttempt(ctx, "mingming", receipt.DeliveryID); err != nil {
		t.Fatal(err)
	}
	unknown, err := store.MarkDeliveryOutcomeUnknown(ctx, "mingming", receipt.DeliveryID, "request timeout")
	if err != nil || unknown.Status != k12.DeliveryOutcomeUnknown {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	if _, started, err := store.BeginDeliveryAttempt(ctx, "mingming", receipt.DeliveryID); err == nil || started {
		t.Fatalf("outcome_unknown must prohibit blind retry: started=%v err=%v", started, err)
	}

	restarted := k12storage.NewStore(store.DB(), nil)
	recoverable, err := restarted.ListRecoverableDeliveryReceipts(ctx, "mingming")
	if err != nil || len(recoverable) != 1 || recoverable[0].Status != k12.DeliveryOutcomeUnknown {
		t.Fatalf("recoverable=%+v err=%v", recoverable, err)
	}
	terminal, err := restarted.ReconcileDeliveryReceipt(ctx, "mingming", receipt.DeliveryID, k12.DeliveryDelivered, "pqk-reconciled", "")
	if err != nil || terminal.Status != k12.DeliveryDelivered || terminal.ExternalMessageID != "pqk-reconciled" {
		t.Fatalf("reconciled=%+v err=%v", terminal, err)
	}
	replayed, err := restarted.ReconcileDeliveryReceipt(ctx, "mingming", receipt.DeliveryID, k12.DeliveryDelivered, "pqk-reconciled", "")
	if err != nil || replayed != terminal {
		t.Fatalf("terminal reconciliation replay must be idempotent: replay=%+v err=%v", replayed, err)
	}
}
