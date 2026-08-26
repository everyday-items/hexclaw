package k12storage_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type creativeDeliveryEnvelopeFixture struct {
	batch         k12.DeliveryBatch
	targetA       []string
	targetB       []string
	imageDelivery []string
}

type deliveryMutableSnapshot struct {
	status            k12.DeliveryReceiptStatus
	attempt           int
	externalMessageID string
	lastError         string
}

func newCreativeDeliveryEnvelopeFixture(includeSecondTarget bool) creativeDeliveryEnvelopeFixture {
	batch := k12.DeliveryBatch{
		BatchID:       "creative-envelope-batch",
		AgentName:     "mingming",
		ObjectKind:    "creative_work",
		ObjectID:      "creative-work-1",
		DedupeKey:     "creative-envelope-batch-dedupe",
		ContentDigest: "sha256:creative-envelope-content",
	}
	fixture := creativeDeliveryEnvelopeFixture{
		batch:   batch,
		targetA: []string{"creative-a-markdown", "creative-a-image"},
	}
	appendTarget := func(bindingID, instanceID, chatID string, deliveryIDs []string) {
		target := k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: instanceID, ChatID: chatID, Label: "DingTalk",
		}
		batch.Receipts = append(batch.Receipts,
			k12.DeliveryReceipt{
				DeliveryID: deliveryIDs[0], BindingID: bindingID, Target: target,
				PartKind: messagecontent.PartMarkdown, PartOrdinal: 1,
				PartDigest: "sha256:" + deliveryIDs[0], DedupeKey: deliveryIDs[0] + "-dedupe",
				PayloadDigest: "sha256:" + deliveryIDs[0] + "-payload",
				PayloadJSON:   `{"kind":"markdown","text":"作品点评"}`,
				RenderJSON:    `{"kind":"markdown"}`,
			},
			k12.DeliveryReceipt{
				DeliveryID: deliveryIDs[1], BindingID: bindingID, Target: target,
				PartKind: messagecontent.PartArtifact, PartMIME: "image/png", PartOrdinal: 2,
				PartDigest: "sha256:" + deliveryIDs[1], DedupeKey: deliveryIDs[1] + "-dedupe",
				PayloadDigest: "sha256:" + deliveryIDs[1] + "-payload",
				PayloadJSON:   `{"kind":"artifact","mime":"image/png"}`,
				RenderJSON:    `{"kind":"artifact"}`,
			},
		)
		fixture.imageDelivery = append(fixture.imageDelivery, deliveryIDs[1])
	}
	appendTarget("binding-a", "bot-a", "parent-a", fixture.targetA)
	if includeSecondTarget {
		fixture.targetB = []string{"creative-b-markdown", "creative-b-image"}
		appendTarget("binding-b", "bot-b", "parent-b", fixture.targetB)
	}
	fixture.batch = batch
	return fixture
}

func prepareCreativeDeliveryEnvelope(
	t *testing.T,
	includeSecondTarget bool,
) (*k12storage.Store, creativeDeliveryEnvelopeFixture) {
	t.Helper()
	store := setupDeliveryStore(t)
	fixture := newCreativeDeliveryEnvelopeFixture(includeSecondTarget)
	stored, created, err := store.PrepareDeliveryBatch(context.Background(), fixture.batch)
	if err != nil || !created {
		t.Fatalf("prepare creative delivery envelope: created=%v batch=%+v err=%v", created, stored, err)
	}
	fixture.batch = stored
	for index, deliveryID := range fixture.imageDelivery {
		if _, err := store.SaveDeliveryPreparedResource(
			context.Background(), fixture.batch.AgentName, deliveryID, "@creative-image-media-"+string(rune('a'+index)),
		); err != nil {
			t.Fatalf("prepare creative image resource %s: %v", deliveryID, err)
		}
	}
	return store, fixture
}

func deliverySnapshots(
	t *testing.T,
	store *k12storage.Store,
	agentName string,
	deliveryIDs []string,
) map[string]deliveryMutableSnapshot {
	t.Helper()
	out := make(map[string]deliveryMutableSnapshot, len(deliveryIDs))
	for _, deliveryID := range deliveryIDs {
		receipt, err := store.GetDeliveryReceipt(context.Background(), agentName, deliveryID)
		if err != nil {
			t.Fatalf("get receipt %s: %v", deliveryID, err)
		}
		out[deliveryID] = deliveryMutableSnapshot{
			status:            receipt.Status,
			attempt:           receipt.Attempt,
			externalMessageID: receipt.ExternalMessageID,
			lastError:         receipt.LastError,
		}
	}
	return out
}

func deliveryGroupSnapshot(
	t *testing.T,
	store *k12storage.Store,
	agentName string,
	deliveryIDs []string,
) []k12.DeliveryReceipt {
	t.Helper()
	receipts := make([]k12.DeliveryReceipt, 0, len(deliveryIDs))
	for _, deliveryID := range deliveryIDs {
		receipt, err := store.GetDeliveryReceipt(context.Background(), agentName, deliveryID)
		if err != nil {
			t.Fatalf("get group snapshot receipt %s: %v", deliveryID, err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts
}

func assertDeliverySnapshotsEqual(
	t *testing.T,
	got, want map[string]deliveryMutableSnapshot,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("snapshot size=%d, want %d", len(got), len(want))
	}
	for deliveryID, wantSnapshot := range want {
		if gotSnapshot, ok := got[deliveryID]; !ok || gotSnapshot != wantSnapshot {
			t.Errorf("receipt %s changed partially: got=%+v want=%+v", deliveryID, gotSnapshot, wantSnapshot)
		}
	}
}

func assertOrderedGroupState(
	t *testing.T,
	receipts []k12.DeliveryReceipt,
	wantIDs []string,
	wantStatus k12.DeliveryReceiptStatus,
	wantAttempt int,
	wantExternalID string,
) {
	t.Helper()
	if len(receipts) != len(wantIDs) {
		t.Fatalf("group receipt count=%d, want %d: %+v", len(receipts), len(wantIDs), receipts)
	}
	for index, receipt := range receipts {
		if receipt.DeliveryID != wantIDs[index] {
			t.Errorf("group order[%d]=%q, want %q", index, receipt.DeliveryID, wantIDs[index])
		}
		if receipt.Status != wantStatus || receipt.Attempt != wantAttempt ||
			receipt.ExternalMessageID != wantExternalID {
			t.Errorf("receipt %s state=%s attempt=%d external=%q, want %s/%d/%q",
				receipt.DeliveryID, receipt.Status, receipt.Attempt, receipt.ExternalMessageID,
				wantStatus, wantAttempt, wantExternalID)
		}
	}
}

func TestCreativeDeliveryEnvelopeBeginsBothRowsInOneAttempt(t *testing.T) {
	store, fixture := prepareCreativeDeliveryEnvelope(t, false)
	ctx := context.Background()
	expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)

	receipts, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, expected)
	if err != nil || !began {
		t.Fatalf("begin creative delivery group: began=%v receipts=%+v err=%v", began, receipts, err)
	}
	assertOrderedGroupState(t, receipts, fixture.targetA, k12.DeliverySending, 1, "")

	replay, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, receipts)
	if err != nil || began {
		t.Fatalf("begin replay must be idempotent: began=%v receipts=%+v err=%v", began, replay, err)
	}
	assertOrderedGroupState(t, replay, fixture.targetA, k12.DeliverySending, 1, "")
}

func TestCreativeDeliveryEnvelopeAcceptedAndDeliveredShareOneExternalID(t *testing.T) {
	store, fixture := prepareCreativeDeliveryEnvelope(t, false)
	ctx := context.Background()
	expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
	if _, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, expected); err != nil || !began {
		t.Fatalf("begin creative delivery group: began=%v err=%v", began, err)
	}

	accepted, err := store.MarkDeliveryGroupAccepted(
		ctx, fixture.batch.AgentName, fixture.targetA, "process-query-key-1",
	)
	if err != nil {
		t.Fatalf("accept creative delivery group: %v", err)
	}
	assertOrderedGroupState(t, accepted, fixture.targetA, k12.DeliverySending, 1, "process-query-key-1")

	acceptedReplay, err := store.MarkDeliveryGroupAccepted(
		ctx, fixture.batch.AgentName, fixture.targetA, "process-query-key-1",
	)
	if err != nil {
		t.Fatalf("accepted replay must be idempotent: %v", err)
	}
	assertOrderedGroupState(t, acceptedReplay, fixture.targetA, k12.DeliverySending, 1, "process-query-key-1")

	delivered, err := store.MarkDeliveryGroupDelivered(ctx, fixture.batch.AgentName, fixture.targetA)
	if err != nil {
		t.Fatalf("deliver creative delivery group: %v", err)
	}
	assertOrderedGroupState(t, delivered, fixture.targetA, k12.DeliveryDelivered, 1, "process-query-key-1")

	deliveredReplay, err := store.MarkDeliveryGroupDelivered(ctx, fixture.batch.AgentName, fixture.targetA)
	if err != nil {
		t.Fatalf("delivered replay must be idempotent: %v", err)
	}
	assertOrderedGroupState(t, deliveredReplay, fixture.targetA, k12.DeliveryDelivered, 1, "process-query-key-1")
}

func TestCreativeDeliveryEnvelopeFailureAndUnknownAreWholeGroupTransitions(t *testing.T) {
	tests := []struct {
		name       string
		transition func(context.Context, *k12storage.Store, string, []string) ([]k12.DeliveryReceipt, error)
		wantStatus k12.DeliveryReceiptStatus
		wantDetail string
	}{
		{
			name: "failed",
			transition: func(ctx context.Context, store *k12storage.Store, agent string, ids []string) ([]k12.DeliveryReceipt, error) {
				return store.MarkDeliveryGroupFailed(ctx, agent, ids, "provider rejected envelope")
			},
			wantStatus: k12.DeliveryFailed,
			wantDetail: "provider rejected envelope",
		},
		{
			name: "outcome unknown",
			transition: func(ctx context.Context, store *k12storage.Store, agent string, ids []string) ([]k12.DeliveryReceipt, error) {
				return store.MarkDeliveryGroupOutcomeUnknown(ctx, agent, ids, "provider response was lost")
			},
			wantStatus: k12.DeliveryOutcomeUnknown,
			wantDetail: "provider response was lost",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, fixture := prepareCreativeDeliveryEnvelope(t, false)
			ctx := context.Background()
			expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
			if _, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, expected); err != nil || !began {
				t.Fatalf("begin creative delivery group: began=%v err=%v", began, err)
			}
			receipts, err := tt.transition(ctx, store, fixture.batch.AgentName, fixture.targetA)
			if err != nil {
				t.Fatalf("transition creative delivery group: %v", err)
			}
			assertOrderedGroupState(t, receipts, fixture.targetA, tt.wantStatus, 1, "")
			for _, receipt := range receipts {
				if receipt.LastError != tt.wantDetail {
					t.Errorf("receipt %s last_error=%q, want %q", receipt.DeliveryID, receipt.LastError, tt.wantDetail)
				}
			}
			replay, err := tt.transition(ctx, store, fixture.batch.AgentName, fixture.targetA)
			if err != nil {
				t.Fatalf("terminal replay must be idempotent: %v", err)
			}
			assertOrderedGroupState(t, replay, fixture.targetA, tt.wantStatus, 1, "")
		})
	}
}

func TestCreativeDeliveryEnvelopeReconcilesEveryRowAtomically(t *testing.T) {
	store, fixture := prepareCreativeDeliveryEnvelope(t, false)
	ctx := context.Background()
	expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
	if _, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, expected); err != nil || !began {
		t.Fatalf("begin creative delivery group: began=%v err=%v", began, err)
	}
	if _, err := store.MarkDeliveryGroupOutcomeUnknown(
		ctx, fixture.batch.AgentName, fixture.targetA, "provider response was lost",
	); err != nil {
		t.Fatal(err)
	}

	reconciled, err := store.ReconcileDeliveryGroup(
		ctx, fixture.batch.AgentName, fixture.targetA,
		k12.DeliveryDelivered, "process-query-key-reconciled", "",
	)
	if err != nil {
		t.Fatalf("reconcile creative delivery group: %v", err)
	}
	assertOrderedGroupState(
		t, reconciled, fixture.targetA, k12.DeliveryDelivered, 1, "process-query-key-reconciled",
	)

	replay, err := store.ReconcileDeliveryGroup(
		ctx, fixture.batch.AgentName, fixture.targetA,
		k12.DeliveryDelivered, "process-query-key-reconciled", "",
	)
	if err != nil {
		t.Fatalf("reconcile replay must be idempotent: %v", err)
	}
	assertOrderedGroupState(
		t, replay, fixture.targetA, k12.DeliveryDelivered, 1, "process-query-key-reconciled",
	)
}

func TestCreativeDeliveryEnvelopeRejectsNonExactGroupsWithoutPartialWrites(t *testing.T) {
	tests := []struct {
		name       string
		prepareIDs func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) []string
	}{
		{
			name: "mixed state",
			prepareIDs: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) []string {
				t.Helper()
				if _, began, err := store.BeginDeliveryAttempt(
					context.Background(), fixture.batch.AgentName, fixture.targetA[0],
				); err != nil || !began {
					t.Fatalf("prepare mixed state: began=%v err=%v", began, err)
				}
				return append([]string(nil), fixture.targetA...)
			},
		},
		{
			name: "cross target",
			prepareIDs: func(_ *testing.T, _ *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) []string {
				return []string{fixture.targetA[0], fixture.targetB[1]}
			},
		},
		{
			name: "reversed order",
			prepareIDs: func(_ *testing.T, _ *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) []string {
				return []string{fixture.targetA[1], fixture.targetA[0]}
			},
		},
		{
			name: "missing image row",
			prepareIDs: func(_ *testing.T, _ *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) []string {
				return []string{fixture.targetA[0]}
			},
		},
		{
			name: "duplicate markdown row",
			prepareIDs: func(_ *testing.T, _ *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) []string {
				return []string{fixture.targetA[0], fixture.targetA[0]}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, fixture := prepareCreativeDeliveryEnvelope(t, true)
			ids := tt.prepareIDs(t, store, fixture)
			expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, ids)
			allIDs := append(append([]string(nil), fixture.targetA...), fixture.targetB...)
			before := deliverySnapshots(t, store, fixture.batch.AgentName, allIDs)

			if receipts, began, err := store.BeginDeliveryGroupAttempt(
				context.Background(), fixture.batch, expected,
			); !errors.Is(err, k12storage.ErrDeliveryBatchConflict) || began {
				t.Fatalf("non-exact group must conflict: began=%v receipts=%+v err=%v", began, receipts, err)
			}

			after := deliverySnapshots(t, store, fixture.batch.AgentName, allIDs)
			assertDeliverySnapshotsEqual(t, after, before)
		})
	}
}

func TestCreativeDeliveryEnvelopeAcceptedConflictHasZeroPartialWrites(t *testing.T) {
	store, fixture := prepareCreativeDeliveryEnvelope(t, false)
	ctx := context.Background()
	expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
	if _, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, expected); err != nil || !began {
		t.Fatalf("begin creative delivery group: began=%v err=%v", began, err)
	}
	if _, err := store.MarkDeliveryAccepted(
		ctx, fixture.batch.AgentName, fixture.targetA[0], "preexisting-external-id",
	); err != nil {
		t.Fatal(err)
	}
	before := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)

	if receipts, err := store.MarkDeliveryGroupAccepted(
		ctx, fixture.batch.AgentName, fixture.targetA, "different-shared-id",
	); !errors.Is(err, k12storage.ErrDeliveryBatchConflict) {
		t.Fatalf("mixed accepted identity must conflict: receipts=%+v err=%v", receipts, err)
	}

	after := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)
	assertDeliverySnapshotsEqual(t, after, before)
}

func TestCreativeDeliveryEnvelopeTerminalConflictHasZeroPartialWrites(t *testing.T) {
	store, fixture := prepareCreativeDeliveryEnvelope(t, false)
	ctx := context.Background()
	expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
	if _, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, expected); err != nil || !began {
		t.Fatalf("begin creative delivery group: began=%v err=%v", began, err)
	}
	if _, err := store.MarkDeliveryGroupAccepted(
		ctx, fixture.batch.AgentName, fixture.targetA, "shared-external-id",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkDeliveryDelivered(
		ctx, fixture.batch.AgentName, fixture.targetA[0],
	); err != nil {
		t.Fatal(err)
	}
	before := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)

	if receipts, err := store.MarkDeliveryGroupDelivered(
		ctx, fixture.batch.AgentName, fixture.targetA,
	); !errors.Is(err, k12storage.ErrDeliveryBatchConflict) {
		t.Fatalf("mixed terminal state must conflict: receipts=%+v err=%v", receipts, err)
	}

	after := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)
	assertDeliverySnapshotsEqual(t, after, before)
}

func TestCreativeDeliveryEnvelopeReconcileRejectsDifferentExternalIDWithoutPartialWrites(t *testing.T) {
	store, fixture := prepareCreativeDeliveryEnvelope(t, false)
	ctx := context.Background()
	expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
	if _, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, expected); err != nil || !began {
		t.Fatalf("begin creative delivery group: began=%v err=%v", began, err)
	}
	if _, err := store.MarkDeliveryGroupAccepted(
		ctx, fixture.batch.AgentName, fixture.targetA, "shared-external-id-a",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkDeliveryGroupOutcomeUnknown(
		ctx, fixture.batch.AgentName, fixture.targetA, "provider response was lost",
	); err != nil {
		t.Fatal(err)
	}
	before := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)

	if receipts, err := store.ReconcileDeliveryGroup(
		ctx, fixture.batch.AgentName, fixture.targetA,
		k12.DeliveryDelivered, "shared-external-id-b", "",
	); !errors.Is(err, k12storage.ErrDeliveryBatchConflict) {
		t.Fatalf("different reconciliation external id must conflict: receipts=%+v err=%v", receipts, err)
	}

	after := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)
	assertDeliverySnapshotsEqual(t, after, before)
}

func TestCreativeDeliveryEnvelopeBeginUsesSingleCASAcrossTwoGoroutines(t *testing.T) {
	store, fixture := prepareCreativeDeliveryEnvelope(t, false)
	ctx := context.Background()
	expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
	start := make(chan struct{})
	type result struct {
		receipts []k12.DeliveryReceipt
		began    bool
		err      error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			receipts, began, err := store.BeginDeliveryGroupAttempt(
				ctx, fixture.batch, expected,
			)
			results <- result{receipts: receipts, began: began, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	beganCount := 0
	conflictCount := 0
	for result := range results {
		if result.err != nil {
			if errors.Is(result.err, k12storage.ErrDeliveryBatchConflict) && !result.began && len(result.receipts) == 0 {
				conflictCount++
				continue
			}
			t.Errorf("concurrent begin returned unexpected result: began=%v receipts=%+v err=%v",
				result.began, result.receipts, result.err)
			continue
		}
		if result.began {
			beganCount++
		}
		assertOrderedGroupState(t, result.receipts, fixture.targetA, k12.DeliverySending, 1, "")
	}
	if beganCount != 1 {
		t.Fatalf("concurrent group begin winners=%d, want exactly 1", beganCount)
	}
	if conflictCount != 1 {
		t.Fatalf("concurrent group begin stale-snapshot conflicts=%d, want exactly 1", conflictCount)
	}
	stored := make([]k12.DeliveryReceipt, 0, len(fixture.targetA))
	for _, deliveryID := range fixture.targetA {
		receipt, err := store.GetDeliveryReceipt(ctx, fixture.batch.AgentName, deliveryID)
		if err != nil {
			t.Fatal(err)
		}
		stored = append(stored, receipt)
	}
	assertOrderedGroupState(t, stored, fixture.targetA, k12.DeliverySending, 1, "")
}

func TestCreativeDeliveryEnvelopeBeginRejectsFrozenIdentityDriftBeforeCAS(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture)
	}{
		{
			name: "batch ordinal",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET batch_ordinal=99
					WHERE agent_name=? AND delivery_id=?`, fixture.batch.AgentName, fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "payload json",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET payload_json=?
					WHERE agent_name=? AND delivery_id=?`,
					`{"kind":"artifact","mime":"image/png","ordinal":99}`,
					fixture.batch.AgentName, fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "payload digest",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET payload_digest=?
					WHERE agent_name=? AND delivery_id=?`, "sha256:mutated-payload",
					fixture.batch.AgentName, fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "render manifest",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET render_manifest_json=?
					WHERE agent_name=? AND delivery_id=?`, `{"kind":"artifact","changed":true}`,
					fixture.batch.AgentName, fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "prepared resource",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET prepared_resource_id=?
					WHERE agent_name=? AND delivery_id=?`, "@different-provider-resource",
					fixture.batch.AgentName, fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "part digest",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET part_digest=?
					WHERE agent_name=? AND delivery_id=?`, "sha256:different-part",
					fixture.batch.AgentName, fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "dedupe key",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET dedupe_key=?
					WHERE agent_name=? AND delivery_id=?`, "different-image-dedupe",
					fixture.batch.AgentName, fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, fixture := prepareCreativeDeliveryEnvelope(t, false)
			expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
			tt.mutate(t, store, fixture)

			receipts, began, err := store.BeginDeliveryGroupAttempt(
				context.Background(), fixture.batch, expected,
			)
			if !errors.Is(err, k12storage.ErrDeliveryBatchConflict) || began {
				t.Errorf("frozen identity drift must conflict before CAS: began=%v receipts=%+v err=%v", began, receipts, err)
			}
			for _, deliveryID := range fixture.targetA {
				receipt, getErr := store.GetDeliveryReceipt(context.Background(), fixture.batch.AgentName, deliveryID)
				if getErr != nil {
					t.Fatal(getErr)
				}
				if receipt.Status != k12.DeliveryPending || receipt.Attempt != 0 || receipt.ExternalMessageID != "" {
					t.Errorf("receipt %s changed despite frozen identity conflict: status=%s attempt=%d external=%q",
						deliveryID, receipt.Status, receipt.Attempt, receipt.ExternalMessageID)
				}
			}
		})
	}
}

func TestCreativeDeliveryEnvelopeBeginRejectsStaleMutableSnapshotBeforeCAS(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture)
	}{
		{
			name: "status",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET status='pending'
					WHERE agent_name=? AND delivery_id IN (?,?)`, fixture.batch.AgentName,
					fixture.targetA[0], fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "attempt",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET attempt=2
					WHERE agent_name=? AND delivery_id IN (?,?)`, fixture.batch.AgentName,
					fixture.targetA[0], fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "external message id",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET external_message_id='newer-provider-id'
					WHERE agent_name=? AND delivery_id IN (?,?)`, fixture.batch.AgentName,
					fixture.targetA[0], fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "last error",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_receipts SET last_error='newer failure detail'
					WHERE agent_name=? AND delivery_id IN (?,?)`, fixture.batch.AgentName,
					fixture.targetA[0], fixture.targetA[1]); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, fixture := prepareCreativeDeliveryEnvelope(t, false)
			ctx := context.Background()
			initial := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
			if _, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, initial); err != nil || !began {
				t.Fatalf("begin initial delivery group: began=%v err=%v", began, err)
			}
			if _, err := store.MarkDeliveryGroupFailed(
				ctx, fixture.batch.AgentName, fixture.targetA, "provider rejected envelope",
			); err != nil {
				t.Fatal(err)
			}
			expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
			tt.mutate(t, store, fixture)
			before := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)

			receipts, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, expected)
			if !errors.Is(err, k12storage.ErrDeliveryBatchConflict) || began {
				t.Errorf("stale mutable snapshot must conflict before CAS: began=%v receipts=%+v err=%v", began, receipts, err)
			}
			after := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)
			assertDeliverySnapshotsEqual(t, after, before)
		})
	}
}

func TestCreativeDeliveryEnvelopeBeginRetriesExactCurrentFailedSnapshot(t *testing.T) {
	store, fixture := prepareCreativeDeliveryEnvelope(t, false)
	ctx := context.Background()
	initial := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
	if _, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, initial); err != nil || !began {
		t.Fatalf("begin initial delivery group: began=%v err=%v", began, err)
	}
	if _, err := store.MarkDeliveryGroupFailed(
		ctx, fixture.batch.AgentName, fixture.targetA, "provider rejected envelope",
	); err != nil {
		t.Fatal(err)
	}
	expected := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)

	retried, began, err := store.BeginDeliveryGroupAttempt(ctx, fixture.batch, expected)
	if err != nil || !began {
		t.Fatalf("retry exact current failed snapshot: began=%v receipts=%+v err=%v", began, retried, err)
	}
	assertOrderedGroupState(t, retried, fixture.targetA, k12.DeliverySending, 2, "")
	for _, receipt := range retried {
		if receipt.LastError != "" {
			t.Errorf("retried receipt %s retained last_error %q", receipt.DeliveryID, receipt.LastError)
		}
	}
}

func TestCreativeDeliveryEnvelopeBeginRejectsStaleBatchRootBeforeCAS(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture)
	}{
		{
			name: "content digest",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_batches SET content_digest=?
					WHERE agent_name=? AND batch_id=?`, "sha256:mutated-root-content",
					fixture.batch.AgentName, fixture.batch.BatchID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "dedupe key",
			mutate: func(t *testing.T, store *k12storage.Store, fixture creativeDeliveryEnvelopeFixture) {
				t.Helper()
				if _, err := store.DB().Exec(`UPDATE k12_delivery_batches SET dedupe_key=?
					WHERE agent_name=? AND batch_id=?`, "mutated-root-dedupe",
					fixture.batch.AgentName, fixture.batch.BatchID); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, fixture := prepareCreativeDeliveryEnvelope(t, false)
			ctx := context.Background()
			expectedBatch, err := store.GetDeliveryBatch(ctx, fixture.batch.AgentName, fixture.batch.BatchID)
			if err != nil {
				t.Fatal(err)
			}
			expectedGroup := deliveryGroupSnapshot(t, store, fixture.batch.AgentName, fixture.targetA)
			tt.mutate(t, store, fixture)
			before := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)

			receipts, began, err := store.BeginDeliveryGroupAttempt(
				ctx, expectedBatch, expectedGroup,
			)
			if !errors.Is(err, k12storage.ErrDeliveryBatchConflict) || began {
				t.Errorf("stale batch root must conflict before CAS: began=%v receipts=%+v err=%v", began, receipts, err)
			}
			after := deliverySnapshots(t, store, fixture.batch.AgentName, fixture.targetA)
			assertDeliverySnapshotsEqual(t, after, before)
		})
	}
}
