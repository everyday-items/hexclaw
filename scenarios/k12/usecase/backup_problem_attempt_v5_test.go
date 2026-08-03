package usecase

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

func TestBackupV5PacksProblemAttemptLedgerAndPageAssetExactSet(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	d, store := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	ctx := context.Background()
	image := validPNGFixture(t, "problem-attempt-v5")
	assetID, err := assetstore.Save("mingming", image)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := v5ProblemAttemptSnapshot("mingming", photoSubmissionID(image), assetID)
	if err := store.PutProblemAttemptSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}

	bak, err := d.Backup(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if bak.Version != 6 || len(bak.ProblemAttempts) != 1 || len(bak.Assets) != 1 {
		t.Fatalf("backup scope incomplete: version=%d problem_attempts=%d assets=%d", bak.Version, len(bak.ProblemAttempts), len(bak.Assets))
	}
	if bak.ProblemAttempts[0].Problems[0].PageAssetID != assetID || bak.Assets[0].AssetID != assetID {
		t.Fatalf("page asset not packed with ledger: snapshot=%+v assets=%+v", bak.ProblemAttempts, bak.Assets)
	}
	if err := VerifyHexbak(bak); err != nil {
		t.Fatalf("valid v5 archive: %v", err)
	}

	tampered := cloneHexbak(bak)
	tampered.ProblemAttempts[0].Problems[0].StemRaw = "被篡改的 OCR 原文"
	if err := VerifyHexbak(tampered); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Problem/Attempt must be checksum-covered, got %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Hexbak)
	}{
		{name: "missing ledger", mutate: func(b *Hexbak) { b.ProblemAttempts = nil }},
		{name: "cross owner", mutate: func(b *Hexbak) { b.ProblemAttempts[0].Attempts[0].AgentName = "other-child" }},
		{name: "unpacked page", mutate: func(b *Hexbak) { b.Assets = nil }},
		{name: "extra unreferenced asset", mutate: func(b *Hexbak) {
			extraImage := validPNGFixture(t, "extra-v5")
			id, mime, digest, describeErr := assetstore.Describe("mingming", extraImage)
			if describeErr != nil {
				t.Fatal(describeErr)
			}
			b.Assets = append(b.Assets, HexbakAsset{AssetID: id, OwnerAgent: "mingming", SHA256: digest, MIME: mime, Data: extraImage})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := cloneHexbak(bak)
			tc.mutate(candidate)
			if err := SealHexbak(candidate); err != nil {
				t.Fatal(err)
			}
			if err := VerifyHexbak(candidate); err == nil {
				t.Fatal("semantic tamper passed after attacker recomputed checksum")
			}
		})
	}
}

func TestHexbakV4RejectsUnsignedProblemAttemptPayloadAndKeepsChecksumCompatibility(t *testing.T) {
	legacy := &Hexbak{Version: 4, ArchiveID: "legacy-v4", AgentName: "mingming", ExportedAt: 100}
	if err := SealHexbak(legacy); err != nil {
		t.Fatal(err)
	}
	legacyChecksum := legacy.Checksum
	legacy.ProblemAttempts = []k12.ProblemAttemptSnapshot{{}}
	if err := SealHexbak(legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Checksum != legacyChecksum {
		t.Fatalf("v4 checksum bytes changed after adding a v5-only field: got=%s want=%s", legacy.Checksum, legacyChecksum)
	}
	if err := VerifyHexbak(legacy); !errors.Is(err, ErrHexbakProblemAttempt) {
		t.Fatalf("v4 must reject unsigned Problem/Attempt payload, got %v", err)
	}
}

func TestMigrateHexbakOwnerKeepsStableProblemAttemptIDsAndRewritesOnlyOwnerAndPageAsset(t *testing.T) {
	source, image := v5ProblemAttemptArchive(t, "source-child")
	sourceSnapshot := cloneHexbak(source).ProblemAttempts[0]

	migrated, err := MigrateHexbakOwner(source, "target-child")
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != 6 || len(migrated.ProblemAttempts) != 1 || len(migrated.Assets) != 1 {
		t.Fatalf("migrated archive incomplete: %+v", migrated)
	}
	got := migrated.ProblemAttempts[0]
	want := sourceSnapshot
	if got.Problems[0].ProblemID != want.Problems[0].ProblemID ||
		got.Attempts[0].AttemptID != want.Attempts[0].AttemptID ||
		got.Problems[0].SubmissionID != want.Problems[0].SubmissionID ||
		got.Attempts[0].SubmissionID != want.Attempts[0].SubmissionID {
		t.Fatalf("stable V19 IDs must not be remapped: source=%+v target=%+v", want, got)
	}
	if got.Problems[0].AgentName != "target-child" || got.Attempts[0].AgentName != "target-child" ||
		got.Problems[0].PageAssetID == want.Problems[0].PageAssetID ||
		got.Problems[0].PageAssetID != migrated.Assets[0].AssetID {
		t.Fatalf("owner/page asset migration invalid: source=%+v target=%+v", want, got)
	}
	if got.Problems[0].SubmissionID != photoSubmissionID(image) {
		t.Fatalf("photo submission digest identity was broken: got=%q want=%q", got.Problems[0].SubmissionID, photoSubmissionID(image))
	}
	if err := VerifyHexbak(migrated); err != nil {
		t.Fatalf("migrated v5 archive must verify: %v", err)
	}
	if source.ProblemAttempts[0].Problems[0].AgentName != "source-child" ||
		source.ProblemAttempts[0].Problems[0].PageAssetID != want.Problems[0].PageAssetID {
		t.Fatal("source archive was mutated")
	}
}

func v5ProblemAttemptArchive(t *testing.T, agent string) (*Hexbak, []byte) {
	t.Helper()
	image := validPNGFixture(t, "problem-attempt-v5-archive")
	assetID, mime, digest, err := assetstore.Describe(agent, image)
	if err != nil {
		t.Fatal(err)
	}
	bak := &Hexbak{
		Version: HexbakVersion, AgentName: agent, ExportedAt: 200,
		Assets: []HexbakAsset{{AssetID: assetID, OwnerAgent: agent, SHA256: digest, MIME: mime, Data: image}},
		ProblemAttempts: []k12.ProblemAttemptSnapshot{
			v5ProblemAttemptSnapshot(agent, photoSubmissionID(image), assetID),
		},
	}
	if err := SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	return bak, image
}

func v5ProblemAttemptSnapshot(agent, submissionID, pageAssetID string) k12.ProblemAttemptSnapshot {
	return k12.ProblemAttemptSnapshot{
		Problems: []k12.Problem{{
			ProblemID: "problem-stable-1", AgentName: agent, SubmissionID: submissionID,
			PageAssetID: pageAssetID, Ordinal: 0, ProblemKind: k12.ProblemKindStandalone,
			Subject: "数学", StemRaw: "12÷3=?", StemMarkdown: "12\\div3=?",
			ConceptIDs: []string{"整数除法"}, CanonicalVersion: 1, CreatedAt: 100, UpdatedAt: 100,
		}},
		Attempts: []k12.Attempt{{
			AttemptID: "attempt-stable-1", AgentName: agent, SubmissionID: submissionID,
			ProblemID: "problem-stable-1", AnswerState: "present", AnswerRaw: "4",
			AnswerMarkdown: "4", CreatedAt: 100, UpdatedAt: 100,
		}},
	}
}

func photoSubmissionID(image []byte) string {
	sum := sha1.Sum(image)
	return "photo-" + hex.EncodeToString(sum[:])
}
