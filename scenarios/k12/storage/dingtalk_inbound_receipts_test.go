package k12storage_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

func openInboundPhotoStore(t *testing.T, path string) (*sql.DB, *k12storage.Store) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	migrations := append([]migrate.Migration(nil), migrate.All...)
	registered := false
	for _, migration := range migrations {
		if migration.Version == migrate.K12IMInboundReceiptsV88.Version {
			registered = true
			break
		}
	}
	if !registered {
		migrations = append(migrations, migrate.K12IMInboundReceiptsV88)
	}
	if err := migrate.Run(context.Background(), db, migrations); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db, k12storage.NewStore(db, nil)
}

func seedInboundAgent(t *testing.T, db *sql.DB, agentName string) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO agents(name,display_name) VALUES(?,?)`, agentName, agentName); err != nil {
		t.Fatal(err)
	}
}

func inboundAdmission(messageID string, image []byte) k12storage.InboundPhotoAdmission {
	return k12storage.InboundPhotoAdmission{
		OwnerScope: "owner-a",
		AgentName:  "mingming",
		BindingID:  "binding-a",
		Identity: k12storage.InboundPhotoIdentity{
			Platform:          "dingtalk",
			InstanceID:        "robot-a",
			ChatID:            "parent-chat",
			ProviderMessageID: messageID,
		},
		AssetName:   "homework.jpg",
		AssetMIME:   "image/jpeg",
		AssetBytes:  append([]byte(nil), image...),
		CommandJSON: `{"schema_version":2,"kind":"k12_photo","intent":"grade_homework","provider":"hexclaw-gpt","model":"gpt-5.6-sol"}`,
	}
}

func tableCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestInboundPhotoAdmissionIsAtomicAndProviderIdentityIsTheDedupeOwner(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "inbound.sqlite")
	db, store := openInboundPhotoStore(t, dbPath)
	defer db.Close()
	seedInboundAgent(t, db, "mingming")

	firstBytes := []byte("first immutable image bytes")
	first, created, err := store.AdmitInboundPhoto(ctx, inboundAdmission("msg-1", firstBytes))
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first provider message admission was not created")
	}
	replayed, created, err := store.AdmitInboundPhoto(ctx, inboundAdmission("msg-1", firstBytes))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("same provider message and digest created a second admission")
	}
	if replayed.Receipt.ReceiptID != first.Receipt.ReceiptID ||
		replayed.Asset.AssetID != first.Asset.AssetID ||
		replayed.Dispatch.DispatchID != first.Dispatch.DispatchID {
		t.Fatalf("idempotent replay changed durable identities: first=%+v replay=%+v", first, replayed)
	}
	mutableReplay := inboundAdmission("msg-1", firstBytes)
	mutableReplay.OwnerScope = "owner-derived-after-restart"
	mutableReplay.AgentName = "derived-agent-after-restart"
	mutableReplay.BindingID = "binding-derived-after-restart"
	mutableReplay.AssetName = "provider-renamed.png"
	mutableReplay.AssetMIME = "image/png"
	mutableReplay.CommandJSON = `{"schema_version":2,"kind":"k12_photo","intent":"rederived_after_restart","provider":"other-provider","model":"other-model"}`
	replayed, created, err = store.AdmitInboundPhoto(ctx, mutableReplay)
	if err != nil {
		t.Fatalf("same provider identity and image digest must restore the frozen admission: %v", err)
	}
	if created || replayed.Receipt.ReceiptID != first.Receipt.ReceiptID ||
		replayed.Receipt.AgentName != first.Receipt.AgentName ||
		replayed.Receipt.BindingID != first.Receipt.BindingID ||
		replayed.Receipt.CommandJSON != first.Receipt.CommandJSON ||
		replayed.Asset.AssetID != first.Asset.AssetID || replayed.Asset.Name != first.Asset.Name {
		t.Fatalf("same-digest replay did not preserve the first frozen admission: first=%+v replay=%+v", first, replayed)
	}
	var derivedAgentCount int
	if err := db.QueryRow(`SELECT count(*) FROM agents WHERE name=?`, mutableReplay.AgentName).Scan(&derivedAgentCount); err != nil {
		t.Fatal(err)
	}
	if derivedAgentCount != 0 {
		t.Fatal("same-digest replay leaked a re-derived owner outside the frozen admission")
	}

	changed := inboundAdmission("msg-1", []byte("different image bytes"))
	if _, _, err := store.AdmitInboundPhoto(ctx, changed); !errors.Is(err, k12storage.ErrInboundPhotoConflict) {
		t.Fatalf("same provider identity with a different digest err=%v", err)
	}
	stored, err := store.GetInboundPhotoByIdentity(ctx, first.Receipt.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.Asset.Bytes, firstBytes) {
		t.Fatalf("conflicting replay replaced the first immutable asset: got=%q", stored.Asset.Bytes)
	}
	for _, table := range []string{
		"k12_im_inbound_receipts", "k12_im_inbound_assets", "k12_im_inbound_dispatches",
	} {
		if got := tableCount(t, db, table); got != 1 {
			t.Fatalf("%s rows=%d want 1 after replay/conflict", table, got)
		}
	}

	second, created, err := store.AdmitInboundPhoto(ctx, inboundAdmission("msg-2", firstBytes))
	if err != nil {
		t.Fatal(err)
	}
	if !created || second.Receipt.ReceiptID == first.Receipt.ReceiptID {
		t.Fatal("different provider MsgId with identical bytes was not an independent submission")
	}
	for _, table := range []string{
		"k12_im_inbound_receipts", "k12_im_inbound_assets", "k12_im_inbound_dispatches",
	} {
		if got := tableCount(t, db, table); got != 2 {
			t.Fatalf("%s rows=%d want 2 for two provider messages", table, got)
		}
	}
}

func TestInboundPhotoAdmissionRollsBackReceiptAndAssetWhenDispatchCannotCommit(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "atomic.sqlite")
	db, store := openInboundPhotoStore(t, dbPath)
	defer db.Close()
	seedInboundAgent(t, db, "mingming")
	if _, err := db.Exec(`CREATE TRIGGER reject_inbound_dispatch
		BEFORE INSERT ON k12_im_inbound_dispatches
		BEGIN SELECT RAISE(ABORT, 'forced dispatch failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdmitInboundPhoto(ctx, inboundAdmission("msg-atomic", []byte("image"))); err == nil {
		t.Fatal("forced dispatch failure unexpectedly committed")
	}
	for _, table := range []string{
		"k12_im_inbound_receipts", "k12_im_inbound_assets", "k12_im_inbound_dispatches",
	} {
		if got := tableCount(t, db, table); got != 0 {
			t.Fatalf("%s rows=%d after failed atomic admission", table, got)
		}
	}
}

func TestInboundPhotoDispatchRecoverySurvivesAdmissionImageTaskAndFinalArtifactRestarts(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "restart.sqlite")
	db, store := openInboundPhotoStore(t, dbPath)
	seedInboundAgent(t, db, "mingming")
	bundle, _, err := store.AdmitInboundPhoto(ctx, inboundAdmission("msg-restart", []byte("restart image")))
	if err != nil {
		t.Fatal(err)
	}
	receiptID := bundle.Receipt.ReceiptID
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, store = openInboundPhotoStore(t, dbPath)
	resumed, err := store.GetInboundPhoto(ctx, "mingming", receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Dispatch.ProcessingStatus != k12storage.InboundPhotoAdmitted ||
		resumed.Dispatch.ReplyStatus != k12storage.InboundPhotoReplyPending || resumed.Dispatch.Version != 0 {
		t.Fatalf("admission restart state=%+v", resumed.Dispatch)
	}
	if resumed.Receipt.BindingID != bundle.Receipt.BindingID ||
		resumed.Receipt.Identity != bundle.Receipt.Identity ||
		resumed.Receipt.CommandJSON != bundle.Receipt.CommandJSON ||
		resumed.Receipt.CommandDigest != bundle.Receipt.CommandDigest ||
		resumed.Asset.Digest != bundle.Asset.Digest ||
		!bytes.Equal(resumed.Asset.Bytes, bundle.Asset.Bytes) {
		t.Fatalf("atomic admission fields did not survive restart: admitted=%+v resumed=%+v", bundle, resumed)
	}

	next := resumed.Dispatch.State()
	next.ProcessingStatus = k12storage.InboundPhotoImageTaskSubmitted
	next.ImageTaskID = "image-task-1"
	next.RoutingDecision = k12storage.InboundPhotoRouteNewSubmission
	advanced, err := store.CompareAndSwapInboundPhotoDispatch(ctx, "mingming", receiptID, 0, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, store = openInboundPhotoStore(t, dbPath)
	resumed, err = store.GetInboundPhoto(ctx, "mingming", receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Dispatch.Version != advanced.Version || resumed.Dispatch.ImageTaskID != "image-task-1" ||
		resumed.Dispatch.RoutingDecision != k12storage.InboundPhotoRouteNewSubmission {
		t.Fatalf("image-task restart state=%+v", resumed.Dispatch)
	}

	next = resumed.Dispatch.State()
	next.ProcessingStatus = k12storage.InboundPhotoFinalArtifactReady
	next.FinalArtifactID = "artifact-1"
	next.ReplyStatus = k12storage.InboundPhotoReplyReady
	advanced, err = store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", receiptID, resumed.Dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, store = openInboundPhotoStore(t, dbPath)
	defer db.Close()
	resumed, err = store.GetInboundPhoto(ctx, "mingming", receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Dispatch.Version != advanced.Version || resumed.Dispatch.FinalArtifactID != "artifact-1" ||
		resumed.Dispatch.ReplyStatus != k12storage.InboundPhotoReplyReady {
		t.Fatalf("final-artifact restart state=%+v", resumed.Dispatch)
	}
	recoverable, err := store.ListRecoverableInboundPhotos(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 1 || recoverable[0].Receipt.ReceiptID != receiptID {
		t.Fatalf("recoverable=%+v want receipt %s", recoverable, receiptID)
	}
	if _, err := store.CompareAndSwapInboundPhotoDispatch(ctx, "mingming", receiptID, 0, next); !errors.Is(err, records.ErrVersionConflict) {
		t.Fatalf("stale dispatch CAS err=%v", err)
	}
	next = resumed.Dispatch.State()
	next.ReplyStatus = k12storage.InboundPhotoReplyBound
	next.DeliveryBatchID = "batch-1"
	bound, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", receiptID, resumed.Dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, store = openInboundPhotoStore(t, dbPath)
	defer db.Close()
	resumed, err = store.GetInboundPhoto(ctx, "mingming", receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Dispatch.Version != bound.Version ||
		resumed.Dispatch.ReplyStatus != k12storage.InboundPhotoReplyBound ||
		resumed.Dispatch.DeliveryBatchID != "batch-1" {
		t.Fatalf("bound reply obligation did not survive restart: %+v", resumed.Dispatch)
	}
	next = resumed.Dispatch.State()
	next.ReplyStatus = k12storage.InboundPhotoReplyDelivered
	delivered, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", receiptID, resumed.Dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", receiptID, delivered.Version, delivered.State(),
	); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("delivered reply accepted a second terminal CAS: err=%v", err)
	}
	stable, err := store.GetInboundPhoto(ctx, "mingming", receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if stable.Dispatch.Version != delivered.Version {
		t.Fatalf("terminal replay changed version: got=%d want=%d", stable.Dispatch.Version, delivered.Version)
	}
	recoverable, err = store.ListRecoverableInboundPhotos(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 0 {
		t.Fatalf("delivered receipt remained recoverable: %+v", recoverable)
	}
}

func TestInboundPhotoRoutingConfirmationIsDurableAndCASProtected(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "confirmation.sqlite")
	db, store := openInboundPhotoStore(t, dbPath)
	seedInboundAgent(t, db, "mingming")
	bundle, _, err := store.AdmitInboundPhoto(ctx, inboundAdmission("msg-confirm", []byte("ambiguous image")))
	if err != nil {
		t.Fatal(err)
	}
	next := bundle.Dispatch.State()
	next.RoutingDecision = k12storage.InboundPhotoRouteAskedUser
	next.ConfirmationStatus = k12storage.InboundPhotoConfirmationWaiting
	waiting, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, 0, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, store = openInboundPhotoStore(t, dbPath)
	resumed, err := store.GetInboundPhoto(ctx, "mingming", bundle.Receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Dispatch.Version != waiting.Version ||
		resumed.Dispatch.RoutingDecision != k12storage.InboundPhotoRouteAskedUser ||
		resumed.Dispatch.ConfirmationStatus != k12storage.InboundPhotoConfirmationWaiting {
		t.Fatalf("waiting confirmation did not survive restart: %+v", resumed.Dispatch)
	}
	next = resumed.Dispatch.State()
	next.RoutingDecision = k12storage.InboundPhotoRouteRegrade
	next.ConfirmationStatus = k12storage.InboundPhotoConfirmationConfirmed
	confirmed, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, resumed.Dispatch.Version, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapInboundPhotoDispatch(
		ctx, "mingming", bundle.Receipt.ReceiptID, resumed.Dispatch.Version, next,
	); !errors.Is(err, records.ErrVersionConflict) {
		t.Fatalf("confirmation CAS admitted two winners: err=%v", err)
	}
	if confirmed.RoutingDecision != k12storage.InboundPhotoRouteRegrade ||
		confirmed.ConfirmationStatus != k12storage.InboundPhotoConfirmationConfirmed {
		t.Fatalf("confirmed state=%+v", confirmed)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, store = openInboundPhotoStore(t, dbPath)
	defer db.Close()
	resumed, err = store.GetInboundPhoto(ctx, "mingming", bundle.Receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Dispatch.RoutingDecision != confirmed.RoutingDecision ||
		resumed.Dispatch.ConfirmationStatus != confirmed.ConfirmationStatus ||
		resumed.Dispatch.Version != confirmed.Version {
		t.Fatalf("confirmed routing did not survive restart: %+v", resumed.Dispatch)
	}
}
