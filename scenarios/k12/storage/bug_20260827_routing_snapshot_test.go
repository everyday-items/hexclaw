package k12storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

func TestInboundPhotoRoutingSnapshotSelectionIsDurableAndIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil)
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name,display_name) VALUES('child','Child')`); err != nil {
		t.Fatal(err)
	}
	// 该测试先验证存储 API 的快照/选择语义；真实图片字段沿用 V88 最小合法值。
	bundle, _, err := store.AdmitInboundPhoto(ctx, InboundPhotoAdmission{
		OwnerScope: "owner", AgentName: "child", BindingID: "binding",
		Identity:  InboundPhotoIdentity{Platform: "dingtalk", InstanceID: "bot", ChatID: "chat", ProviderMessageID: "photo"},
		AssetName: "photo.png", AssetMIME: "image/png", AssetBytes: []byte("png"), CommandJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := InboundPhotoRoutingSnapshot{
		Stage: InboundPhotoRoutingStageCandidate,
		Candidates: []InboundPhotoRoutingCandidate{
			{PracticeSetID: "set-1", PaperNo: "P-2601-01", Title: "第一套", SentAt: 100},
			{PracticeSetID: "set-2", PaperNo: "P-2601-02", Title: "第二套", SentAt: 90},
		},
	}
	dispatch, err := store.SaveInboundPhotoRoutingSnapshot(ctx, "child", bundle.Receipt.ReceiptID, bundle.Dispatch.Version, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.RoutingDecision != InboundPhotoRouteAskedUser || dispatch.Version != bundle.Dispatch.Version+1 {
		t.Fatalf("snapshot did not move dispatch to waiting: %+v", dispatch)
	}
	if _, err := store.ConfirmInboundPhotoRoutingSelection(
		ctx, "child", bundle.Receipt.ReceiptID, dispatch.Version,
		InboundPhotoRouteNewSubmission, "set-1",
	); err == nil {
		t.Fatal("candidate stage must reject new_submission and remain waiting")
	}
	selected, err := store.ConfirmInboundPhotoRoutingSelection(ctx, "child", bundle.Receipt.ReceiptID, dispatch.Version, InboundPhotoRouteRegrade, "set-2")
	if err != nil {
		t.Fatal(err)
	}
	if selected.RoutingDecision != InboundPhotoRouteRegrade || selected.ConfirmationStatus != InboundPhotoConfirmationConfirmed {
		t.Fatalf("selection not confirmed: %+v", selected)
	}
	restarted, err := store.GetInboundPhotoRoutingSnapshot(ctx, "child", bundle.Receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.SelectedPracticeSetID != "set-2" || len(restarted.Candidates) != 2 {
		t.Fatalf("restart snapshot=%+v", restarted)
	}
	if len(restarted.RequestDigest) != 64 {
		t.Fatalf("route request digest must be persisted: %q", restarted.RequestDigest)
	}
	replay, err := store.ConfirmInboundPhotoRoutingSelection(ctx, "child", bundle.Receipt.ReceiptID, selected.Version, InboundPhotoRouteRegrade, "set-2")
	if err != nil {
		t.Fatal(err)
	}
	if replay.Version != selected.Version {
		t.Fatalf("same selection replay changed version: selected=%+v replay=%+v", selected, replay)
	}
}

func TestInboundPhotoRoutingSnapshotIntentAllowsEmptyCandidates(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil)
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name,display_name) VALUES('child-empty','Child')`); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := store.AdmitInboundPhoto(ctx, InboundPhotoAdmission{
		OwnerScope: "owner", AgentName: "child-empty", BindingID: "binding",
		Identity:  InboundPhotoIdentity{Platform: "dingtalk", InstanceID: "bot", ChatID: "chat", ProviderMessageID: "photo-empty"},
		AssetName: "photo.png", AssetMIME: "image/png", AssetBytes: []byte("png"), CommandJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveInboundPhotoRoutingSnapshot(
		ctx, "child-empty", bundle.Receipt.ReceiptID, bundle.Dispatch.Version,
		InboundPhotoRoutingSnapshot{Stage: InboundPhotoRoutingStageIntent},
	); err != nil {
		t.Fatalf("empty intent snapshot must be durable: %v", err)
	}
}

func TestInboundPhotoRoutingSnapshotRejectsPersistedCandidateDigestDrift(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil)
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name,display_name) VALUES('child-drift','Child')`); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := store.AdmitInboundPhoto(ctx, InboundPhotoAdmission{
		OwnerScope: "owner", AgentName: "child-drift", BindingID: "binding",
		Identity:  InboundPhotoIdentity{Platform: "dingtalk", InstanceID: "bot", ChatID: "chat", ProviderMessageID: "photo-drift"},
		AssetName: "photo.png", AssetMIME: "image/png", AssetBytes: []byte("png"), CommandJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := InboundPhotoRoutingSnapshot{
		Stage: InboundPhotoRoutingStageCandidate,
		Candidates: []InboundPhotoRoutingCandidate{
			{PracticeSetID: "set-1", PaperNo: "P-2601-01", SentAt: 100},
			{PracticeSetID: "set-2", PaperNo: "P-2601-02", SentAt: 90},
		},
	}
	dispatch, err := store.SaveInboundPhotoRoutingSnapshot(
		ctx, "child-drift", bundle.Receipt.ReceiptID, bundle.Dispatch.Version, snapshot,
	)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.GetInboundPhotoRoutingSnapshot(ctx, "child-drift", bundle.Receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.RequestDigest) != 64 {
		t.Fatalf("request digest was not persisted: %q", persisted.RequestDigest)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE k12_im_inbound_routing_snapshots SET snapshot_digest=? WHERE receipt_id=?`,
		strings.Repeat("a", 64), bundle.Receipt.ReceiptID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetInboundPhotoRoutingSnapshot(ctx, "child-drift", bundle.Receipt.ReceiptID); err == nil {
		t.Fatal("persisted candidate digest drift must fail closed on read")
	}
	if _, err := store.ConfirmInboundPhotoRoutingSelection(
		ctx, "child-drift", bundle.Receipt.ReceiptID, dispatch.Version,
		InboundPhotoRouteRegrade, "set-1",
	); err == nil {
		t.Fatal("persisted candidate digest drift must fail closed on selection")
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE k12_im_inbound_routing_snapshots SET snapshot_digest=?,request_digest=? WHERE receipt_id=?`,
		persisted.SnapshotDigest, strings.Repeat("b", 64), bundle.Receipt.ReceiptID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetInboundPhotoRoutingSnapshot(ctx, "child-drift", bundle.Receipt.ReceiptID); err == nil {
		t.Fatal("request digest drift must fail closed on read")
	}
	if _, err := store.ConfirmInboundPhotoRoutingSelection(
		ctx, "child-drift", bundle.Receipt.ReceiptID, dispatch.Version,
		InboundPhotoRouteRegrade, "set-1",
	); err == nil {
		t.Fatal("request digest drift must fail closed on selection")
	}
}
