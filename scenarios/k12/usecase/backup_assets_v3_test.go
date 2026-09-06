package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

func TestBackupV3PacksReferencedAssetContentAndMigrateRewritesEveryNestedAssetOwner(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	image := validPNGFixture(t, "hexbak-v3-asset")
	sourceAssetID, err := assetstore.Save("mingming", image)
	if err != nil {
		t.Fatal(err)
	}

	d, store := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	work, err := k12.NewCreativeWorkRecord("mingming", "session-work", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt,
		Title:    "向日葵",
		Task:     "观察色彩",
		Versions: []k12.CreativeWorkVersion{{VersionID: "v1", SourceAssetID: sourceAssetID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	set, err := k12.NewPracticeSetRecord("mingming", "session-set", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual,
		Title:      "周末练习",
		Items:      []k12.PracticeItem{{ItemID: "item-1", QuestionMarkdown: "1+1=?"}},
		ReturnAssets: []k12.PracticeReturnAsset{{
			ReturnID: "return-1", AssetID: sourceAssetID, ItemIDs: []string{"item-1"}, ReturnedAt: 100,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []*records.AgentRecord{work, set} {
		if _, err := store.Put(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	currentImage := validPNGFixture(t, "current-work-asset")
	currentAssetID, err := assetstore.Save("mingming", currentImage)
	if err != nil {
		t.Fatal(err)
	}
	currentWork, err := k12.NewCreativeWorkRecord("mingming", "", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, GradeTerm: "五年级上", DisplayName: "当前美术作品",
	})
	if err != nil {
		t.Fatal(err)
	}
	currentGeneration, _, err := store.CreateCreativeWorkWithInitialGeneration(
		context.Background(), currentWork, "current-art-backup", "current-art-source",
		k12.CreativeWorkSourceSnapshot{WorkType: k12.WorkTypeArt, DisplayName: "当前美术作品", SourceAssetID: currentAssetID},
	)
	if err != nil {
		t.Fatal(err)
	}

	bak, err := d.Backup(context.Background(), "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(bak.Assets) != 2 {
		t.Fatalf("packed assets=%d want legacy and current source assets", len(bak.Assets))
	}
	var asset HexbakAsset
	for _, item := range bak.Assets {
		if item.AssetID == sourceAssetID {
			asset = item
		}
	}
	if asset.AssetID != sourceAssetID || asset.OwnerAgent != "mingming" || asset.SHA256 == "" || asset.MIME != "image/png" || !bytes.Equal(asset.Data, image) {
		t.Fatalf("packed asset=%+v", asset)
	}
	if err := VerifyHexbak(bak); err != nil {
		t.Fatalf("v3 archive with asset content must verify: %v", err)
	}
	encoded, err := json.Marshal(bak)
	if err != nil || !bytes.Contains(encoded, []byte(currentGeneration.GenerationID)) ||
		!bytes.Contains(encoded, []byte(currentAssetID)) {
		t.Fatalf("current creative source/generation is absent: err=%v", err)
	}

	migrated, err := MigrateHexbakOwner(bak, "target-child")
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated.Assets) != 2 || migrated.Assets[0].OwnerAgent != "target-child" || bytes.Equal(nil, migrated.Assets[0].Data) {
		t.Fatalf("migrated asset manifest=%+v", migrated.Assets)
	}
	targetAssetID, _, _, err := assetstore.Describe("target-child", image)
	if err != nil {
		t.Fatal(err)
	}
	if targetAssetID == sourceAssetID || targetAssetID == "" {
		t.Fatalf("asset owner was not rewritten: source=%q target=%q", sourceAssetID, targetAssetID)
	}
	refs, err := ReferencedHexbakAssetIDs(migrated.Records)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != targetAssetID {
		t.Fatalf("nested asset refs=%v want only %q", refs, targetAssetID)
	}
	if err := VerifyHexbak(migrated); err != nil {
		t.Fatalf("migrated v3 checksum/assets invalid: %v", err)
	}
	if asset.AssetID != sourceAssetID || !bytes.Equal(asset.Data, image) {
		t.Fatal("source archive asset payload was mutated")
	}
	if bak.CurrentCreativeWorks[0].AgentName != "mingming" || bak.CurrentCreativeWorks[0].InitialID != currentGeneration.GenerationID || bak.CurrentCreativeWorks[0].Generations[0].Source.SourceAssetID != currentAssetID {
		t.Fatal("owner migration mutated source current creative facts")
	}
	current := migrated.CurrentCreativeWorks[0]
	if current.AgentName != "target-child" || current.InitialID == currentGeneration.GenerationID || current.Generations[0].Source.SourceAssetID == currentAssetID {
		t.Fatal("current creative identity and source owner were not migrated")
	}
}

func TestRestoreAsRejectsMissingOrTamperedAssetPayloadBeforePersistence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Hexbak)
	}{
		{name: "missing content", mutate: func(b *Hexbak) { b.Assets = nil; _ = SealHexbak(b) }},
		{name: "tampered content", mutate: func(b *Hexbak) {
			b.Assets[0].Data = append([]byte(nil), b.Assets[0].Data...)
			b.Assets[0].Data[8] ^= 0xff
			_ = SealHexbak(b)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
			migrator := &captureArchiveMigrator{}
			d.ArchiveMigrator = migrator
			bak := archiveWithPackedAsset(t, "source-child")
			tc.mutate(bak)
			_, err := d.RestoreAs(context.Background(), RestoreAsRequest{
				Archive: bak, SourceAgent: "source-child", TargetAgent: "target-child",
				GuardianConfirmed: true, IdempotencyKey: "asset-preflight",
			})
			if err == nil {
				t.Fatal("invalid asset manifest must be rejected")
			}
			if migrator.calls != 0 {
				t.Fatalf("invalid assets reached persistence: %d", migrator.calls)
			}
		})
	}
}

func TestRestoreAsRejectsV2AssetReferenceBecauseContentWasNotSignedOrPacked(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	migrator := &captureArchiveMigrator{}
	d.ArchiveMigrator = migrator
	bak := archiveWithPackedAsset(t, "source-child")
	bak.Version = 2
	bak.ArchiveID = ""
	bak.Assets = nil
	if err := SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	_, err := d.RestoreAs(context.Background(), RestoreAsRequest{
		Archive: bak, SourceAgent: "source-child", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "asset-v2-fail-closed",
	})
	if !errors.Is(err, ErrHexbakAssetManifest) {
		t.Fatalf("v2 asset restore-as err=%v want asset manifest failure", err)
	}
	if migrator.calls != 0 {
		t.Fatalf("v2 unsigned asset reached persistence: %d", migrator.calls)
	}
}

func archiveWithPackedAsset(t *testing.T, agent string) *Hexbak {
	t.Helper()
	image := validPNGFixture(t, "restore-as-packed")
	id, mime, digest, err := assetstore.Describe(agent, image)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := k12.NewCreativeWorkRecord(agent, "session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "画", Task: "观察",
		Versions: []k12.CreativeWorkVersion{{VersionID: "v1", SourceAssetID: id}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bak := &Hexbak{
		Version: HexbakVersion, AgentName: agent, ExportedAt: 100,
		Records: []*records.AgentRecord{rec},
		Assets:  []HexbakAsset{{AssetID: id, OwnerAgent: agent, SHA256: digest, MIME: mime, Data: image}},
	}
	if err := SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	return bak
}
