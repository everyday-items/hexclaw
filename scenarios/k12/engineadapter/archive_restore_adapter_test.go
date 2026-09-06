package engineadapter

import (
	"bytes"
	"context"
	"database/sql"
	stdimage "image"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

func TestArchiveRestoreAdapterRestoresV3PackedAssetContent(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	bak, image := archiveForRestoreAsWithAsset(t)
	if err := f.restore.RestoreHexbak(context.Background(), bak); err != nil {
		t.Fatal(err)
	}
	refs, err := usecase.ReferencedHexbakAssetIDs(bak.Records)
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
	owner, file, err := assetstore.Parse(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := assetstore.Read(owner, file)
	if err != nil || !bytes.Equal(got, image) {
		t.Fatalf("restored asset=%q err=%v", got, err)
	}
	ctx := context.Background()
	_, mediaType, digest, err := assetstore.Describe("mingming", image)
	if err != nil {
		t.Fatal(err)
	}
	dimensions, _, err := stdimage.DecodeConfig(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	metadata, _, err := f.records.PreparePageAsset(ctx, k12storage.PageAssetMetadata{
		OwnerScope: "guardian", AgentName: "mingming", PageAssetID: refs[0], ContentDigest: digest,
		MediaType: mediaType, SizeBytes: int64(len(image)), PixelWidth: dimensions.Width, PixelHeight: dimensions.Height,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = f.records.MarkPageAssetReady(ctx, metadata.OwnerScope, "mingming", refs[0])
	if err != nil {
		t.Fatal(err)
	}
	work, err := k12.NewCreativeWorkRecord("mingming", "", k12.CreativeWorkFields{WorkType: k12.WorkTypeArt, DisplayName: "当前作品"})
	if err != nil {
		t.Fatal(err)
	}
	generation, _, err := f.records.CreateCreativeWorkWithInitialGeneration(ctx, work, "backup-current", "current-source", k12.CreativeWorkSourceSnapshot{WorkType: k12.WorkTypeArt, SourceAssetID: refs[0]})
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := f.records.PrepareImageTaskInvocation(ctx, k12.ImageTaskInvocation{
		InvocationID: "backup-current-invocation", AgentName: "mingming", WorkRecordID: work.RecordID,
		Operation: k12.ImageTaskOperationWorkFeedback, OperationKey: "work:" + work.RecordID + ":version:" + generation.GenerationID + ":feedback",
		RequestDigest: "current-feedback", Attempt: 1,
		RouteSnapshot: k12.ImageTaskRouteSnapshot{Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", Route: "hexclaw-gpt/gpt-5.6-sol", Capability: "vision", SelectionSource: "explicit", PolicyVersion: "image-task-routing-v1", PromptVersion: "work-feedback-v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := usecase.Deps{Records: f.records}
	older, err := d.Backup(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := f.records.ClaimImageTaskInvocationSend(ctx, "mingming", inv.InvocationID, "private-request-key", time.Now().Unix()); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := f.records.FailWorkFeedbackInvocation(ctx, "mingming", inv.InvocationID, "transport_unknown", true, false); err != nil {
		t.Fatal(err)
	}
	if err := f.restore.RestoreHexbak(ctx, older); err != nil {
		t.Fatal(err)
	}
	latest, err := f.records.GetLatestWorkFeedbackInvocation(ctx, "mingming", work.RecordID, inv.OperationKey)
	if err != nil || latest.Status != k12.ImageTaskInvocationOutcomeUnknown || latest.RetrySafe {
		t.Fatalf("older backup downgraded unknown invocation: status=%s retry=%v err=%v", latest.Status, latest.RetrySafe, err)
	}
	archived, err := d.Backup(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(archived.CurrentCreativeWorks) != 1 || archived.CurrentCreativeWorks[0].Invocations[0].ProviderRequestKey != "" {
		t.Fatal("current invocation missing or private request key escaped archive")
	}
	fresh := newArchiveRestoreFixture(t)
	var creativeRecords []*records.AgentRecord
	for _, record := range archived.Records {
		if record.Collection == k12.CollectionCreativeWork {
			creativeRecords = append(creativeRecords, record)
		}
	}
	archived.Records = creativeRecords
	if err := usecase.SealHexbak(archived); err != nil {
		t.Fatal(err)
	}
	if err := fresh.restore.RestoreHexbak(ctx, archived); err != nil {
		t.Fatal(err)
	}
	restored, err := fresh.records.GetWorkFeedbackGeneration(ctx, "mingming", generation.GenerationID)
	if err != nil || restored.Source.SourceAssetID != refs[0] {
		t.Fatalf("current source was not restored: err=%v", err)
	}
	restoredAsset, err := fresh.records.GetReadyPageAsset(ctx, metadata.OwnerScope, "mingming", refs[0])
	if err != nil || restoredAsset != metadata {
		t.Fatalf("current ready image metadata was not restored exactly: err=%v", err)
	}
	latest, err = fresh.records.GetLatestWorkFeedbackInvocation(ctx, "mingming", work.RecordID, inv.OperationKey)
	if err != nil || latest.Status != k12.ImageTaskInvocationOutcomeUnknown || latest.RetrySafe {
		t.Fatalf("fresh restore must keep unknown parked: status=%s err=%v", latest.Status, err)
	}
}

type archiveRestoreFixture struct {
	db            *sql.DB
	records       *k12storage.Store
	dispatcher    *router.Dispatcher
	agentStore    *router.SQLiteStore
	restore       *ArchiveRestoreAdapter
	oldRecordID   string
	nonK12MetaKey string
}

func newArchiveRestoreFixture(t *testing.T) *archiveRestoreFixture {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "archive-restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatal(err)
	}
	agentStore := router.NewSQLiteStore(db)
	if err := agentStore.Init(ctx); err != nil {
		t.Fatal(err)
	}
	dispatcher := router.New()
	cfg := router.AgentConfig{Name: "mingming", Metadata: map[string]string{
		"provider":           "glm",
		k12.MetaKeyChildName: "旧名字",
		k12.MetaKeyGradeTerm: "五年级上",
		k12.MetaKeyTextbook:  "旧教材",
	}}
	if err := dispatcher.Register(cfg); err != nil {
		t.Fatal(err)
	}
	if err := agentStore.SaveAgent(ctx, &cfg); err != nil {
		t.Fatal(err)
	}

	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(curriculum.New())); err != nil {
		t.Fatal(err)
	}
	recordStore := k12storage.NewStore(db, reg.Records)
	old, err := k12.NewMistakeRecord("mingming", "old-session", k12.MistakeFields{Question: "旧题"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordStore.Put(ctx, old); err != nil {
		t.Fatal(err)
	}
	return &archiveRestoreFixture{
		db: db, records: recordStore, dispatcher: dispatcher, agentStore: agentStore,
		restore:     NewArchiveRestoreAdapter(db, recordStore, dispatcher, agentStore),
		oldRecordID: old.RecordID, nonK12MetaKey: "provider",
	}
}

func validIncomingMistake(t *testing.T) *records.AgentRecord {
	t.Helper()
	r, err := k12.NewMistakeRecord("mingming", "new-session", k12.MistakeFields{Question: "新题"})
	if err != nil {
		t.Fatal(err)
	}
	r.RecordID = "incoming-record"
	r.SchemaVersion = 1
	r.Status = k12.StatusNew
	r.DedupeKey = "incoming-record"
	r.Tags = "[]"
	r.CreatedAt = 100
	r.UpdatedAt = 100
	return r
}

func TestArchiveRestoreAdapter_CommitsRecordsAndExactProfileDurably(t *testing.T) {
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	incoming := validIncomingMistake(t)
	if err := f.restore.RestoreArchive(ctx, "mingming", []*records.AgentRecord{incoming}, &k12.ChildProfile{GradeTerm: "六年级上"}); err != nil {
		t.Fatal(err)
	}

	recs, err := f.records.ExportAgent(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("restore must merge current + incoming records: %+v", recs)
	}
	foundOld, foundIncoming := false, false
	for _, rec := range recs {
		foundOld = foundOld || rec.RecordID == f.oldRecordID
		foundIncoming = foundIncoming || rec.RecordID == incoming.RecordID
	}
	if !foundOld || !foundIncoming {
		t.Fatalf("restore merge lost a record: old=%v incoming=%v records=%+v", foundOld, foundIncoming, recs)
	}
	agents, _, err := f.agentStore.LoadAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("persisted agents=%+v", agents)
	}
	meta := agents[0].Metadata
	if meta[k12.MetaKeyGradeTerm] != "六年级上" || meta[k12.MetaKeyChildName] != "" || meta[k12.MetaKeyTextbook] != "" {
		t.Fatalf("persisted profile was not exact-replaced: %v", meta)
	}
	if meta[f.nonK12MetaKey] != "glm" {
		t.Fatalf("non-K12 metadata lost: %v", meta)
	}
	inMemory, _ := f.dispatcher.GetAgent("mingming")
	if inMemory.Metadata[k12.MetaKeyGradeTerm] != "六年级上" {
		t.Fatalf("memory profile not published after commit: %v", inMemory.Metadata)
	}
}

func TestArchiveRestoreAdapter_ProfileWriteFailureRollsBackRecordsAndMemory(t *testing.T) {
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `CREATE TRIGGER reject_k12_profile
		BEFORE UPDATE OF metadata ON agents
		WHEN NEW.metadata <> OLD.metadata
		BEGIN SELECT RAISE(ABORT, 'profile write blocked'); END`); err != nil {
		t.Fatal(err)
	}
	err := f.restore.RestoreArchive(ctx, "mingming", []*records.AgentRecord{validIncomingMistake(t)}, &k12.ChildProfile{GradeTerm: "六年级上"})
	if err == nil {
		t.Fatal("profile write failure must abort restore")
	}

	recs, readErr := f.records.ExportAgent(ctx, "mingming")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(recs) != 1 || recs[0].RecordID != f.oldRecordID {
		t.Fatalf("record merge escaped rolled-back transaction: %+v", recs)
	}
	inMemory, _ := f.dispatcher.GetAgent("mingming")
	if inMemory.Metadata[k12.MetaKeyGradeTerm] != "五年级上" {
		t.Fatalf("failed transaction leaked profile into memory: %v", inMemory.Metadata)
	}
	agents, _, loadErr := f.agentStore.LoadAgents(ctx)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if agents[0].Metadata[k12.MetaKeyGradeTerm] != "五年级上" {
		t.Fatalf("failed transaction changed persisted profile: %v", agents[0].Metadata)
	}
}

type blockingAgentPersister struct {
	next    *router.SQLiteStore
	entered chan struct{}
	release chan struct{}
}

func (p *blockingAgentPersister) SaveAgent(ctx context.Context, cfg *router.AgentConfig) error {
	close(p.entered)
	<-p.release
	return p.next.SaveAgent(ctx, cfg)
}

func TestArchiveRestoreAdapter_SerializesWithConcurrentSaveProfile(t *testing.T) {
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	blocker := &blockingAgentPersister{
		next: f.agentStore, entered: make(chan struct{}), release: make(chan struct{}),
	}
	profiles := NewProfileAdapter(f.dispatcher, blocker)
	saveDone := make(chan error, 1)
	go func() {
		saveDone <- profiles.SaveProfile(ctx, "mingming", k12.ChildProfile{ChildName: "并发改名"})
	}()
	<-blocker.entered

	restoreDone := make(chan error, 1)
	incoming := validIncomingMistake(t)
	go func() {
		restoreDone <- f.restore.RestoreArchive(ctx, "mingming", []*records.AgentRecord{incoming}, &k12.ChildProfile{GradeTerm: "六年级上"})
	}()
	select {
	case err := <-restoreDone:
		t.Fatalf("restore bypassed in-flight profile persistence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(blocker.release)
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	if err := <-restoreDone; err != nil {
		t.Fatal(err)
	}

	inMemory, _ := f.dispatcher.GetAgent("mingming")
	if inMemory.Metadata[k12.MetaKeyGradeTerm] != "六年级上" || inMemory.Metadata[k12.MetaKeyChildName] != "" {
		t.Fatalf("later exact restore lost to concurrent profile patch: %v", inMemory.Metadata)
	}
	agents, _, err := f.agentStore.LoadAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].Metadata[k12.MetaKeyGradeTerm] != "六年级上" || agents[0].Metadata[k12.MetaKeyChildName] != "" {
		t.Fatalf("persisted profile diverged after concurrent writes: %v", agents[0].Metadata)
	}
}
