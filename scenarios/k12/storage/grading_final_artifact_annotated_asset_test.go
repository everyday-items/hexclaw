package k12storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

func annotatedArtifactPNG(t *testing.T, value uint8) []byte {
	t.Helper()
	var out bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.SetRGBA(x, y, color.RGBA{R: value, G: uint8(x * 40), B: uint8(y * 60), A: 255})
		}
	}
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func annotatedArtifactSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func openAnnotatedArtifactStore(
	t *testing.T,
	databasePath string,
) (*k12storage.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := migrate.Run(t.Context(), db, migrate.All); err != nil {
		_ = db.Close()
		t.Fatalf("migrate base: %v", err)
	}
	if err := migrate.Run(t.Context(), db, []migrate.Migration{
		migrate.K12GradingFinalAnnotatedAssetV89,
	}); err != nil {
		_ = db.Close()
		t.Fatalf("migrate annotated final artifact: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT OR IGNORE INTO agents(name) VALUES('mingming'),('lele')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return k12storage.NewStore(db, registry.Records), db
}

func commitAnnotatedArtifactFixture(
	t *testing.T,
	store *k12storage.Store,
	ownerScope string,
	original []byte,
	annotated []byte,
	sourceKey string,
) (k12.GradingFinalArtifact, usecase.ReadyPageAsset) {
	t.Helper()
	ctx := context.Background()
	repository := &usecase.PageAssetRepository{Records: store}
	if _, err := repository.Persist(ctx, ownerScope, "mingming", original); err != nil {
		t.Fatalf("persist original PageAsset: %v", err)
	}
	ready, err := repository.Persist(ctx, ownerScope, "mingming", annotated)
	if err != nil {
		t.Fatalf("persist annotated PageAsset: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", sourceKey)
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatalf("seed grading job: %v", err)
	}
	artifact := k12.GradingFinalArtifact{
		AgentName:                 "mingming",
		JobID:                     job.RecordID,
		StructureVersion:          k12.GradingFinalArtifactStructureVersion,
		CoverageStatus:            k12.GradingFinalArtifactCoverageComplete,
		TotalCount:                1,
		PublishedCount:            1,
		OrderedCurrentDigestsJSON: `["assessment-annotated"]`,
		CanonicalMarkdown:         "# 批改结果",
		SummaryInvocationID:       "summary-annotated",
		AnnotatedAssetOwnerScope:  ownerScope,
		AnnotatedAssetID:          ready.Metadata.PageAssetID,
		AnnotatedMIME:             ready.Metadata.MediaType,
		AnnotatedDigest:           ready.Metadata.ContentDigest,
		OriginalSourceDigest:      annotatedArtifactSHA256(original),
	}
	artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
	stored, replay, err := store.CommitGradingFinalArtifact(ctx, artifact, 0)
	if err != nil || replay {
		t.Fatalf("commit annotated final artifact: replay=%v err=%v", replay, err)
	}
	return stored, ready
}

func TestGradingFinalAnnotatedAssetSurvivesProcessResultLossAndSQLiteRestart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HEXCLAW_ASSET_ROOT", filepath.Join(root, "assets"))
	databasePath := filepath.Join(root, "hexclaw.db")
	original := annotatedArtifactPNG(t, 40)
	annotated := annotatedArtifactPNG(t, 180)

	store, db := openAnnotatedArtifactStore(t, databasePath)
	artifact, _ := commitAnnotatedArtifactFixture(
		t, store, "guardian-1", original, annotated, "annotated-restart",
	)
	// 进程内 PhotoResult 已不可用，下面的读取只能依赖 SQLite 和已冻结 PageAsset。
	var processLocalResult any = nil
	_ = processLocalResult
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, restartedDB := openAnnotatedArtifactStore(t, databasePath)
	t.Cleanup(func() { _ = restartedDB.Close() })
	loaded, err := restarted.GetGradingFinalArtifact(
		t.Context(), "mingming", artifact.ArtifactID,
	)
	if err != nil || loaded.ArtifactDigest != artifact.ArtifactDigest ||
		loaded.AnnotatedAssetID != artifact.AnnotatedAssetID {
		t.Fatalf("restart final artifact drifted: loaded=%+v err=%v", loaded, err)
	}
	opened, err := restarted.OpenGradingFinalAnnotatedAsset(
		t.Context(), "mingming", artifact.ArtifactID,
	)
	if err != nil {
		t.Fatalf("open restarted annotated asset: %v", err)
	}
	if opened.AssetID != artifact.AnnotatedAssetID ||
		opened.MIME != "image/png" ||
		opened.Digest != annotatedArtifactSHA256(annotated) ||
		opened.OriginalSourceDigest != annotatedArtifactSHA256(original) ||
		!bytes.Equal(opened.Data, annotated) {
		t.Fatalf("restarted annotated bytes drifted: %+v", opened)
	}
	if _, err := restarted.OpenGradingFinalAnnotatedAsset(
		t.Context(), "lele", artifact.ArtifactID,
	); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("cross-agent final artifact read must remain not found: %v", err)
	}

	replayed, replay, err := restarted.CommitGradingFinalArtifact(
		t.Context(), loaded, 0,
	)
	if err != nil || !replay || replayed.AnnotatedAssetID != artifact.AnnotatedAssetID {
		t.Fatalf("exact replay must converge: replay=%v artifact=%+v err=%v", replay, replayed, err)
	}
	drifted := loaded
	drifted.OriginalSourceDigest = strings.Repeat("f", 64)
	drifted.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(drifted)
	if _, _, err := restarted.CommitGradingFinalArtifact(
		t.Context(), drifted, 0,
	); !errors.Is(err, k12storage.ErrGradingFinalArtifactConflict) {
		t.Fatalf("source digest replay drift must fail closed: %v", err)
	}
	assetOwner, _, err := assetstore.Parse(artifact.AnnotatedAssetID)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := assetstore.Remove(assetOwner, artifact.AnnotatedAssetID); err != nil || !removed {
		t.Fatalf("remove committed annotated bytes: removed=%v err=%v", removed, err)
	}
	if _, err := restarted.OpenGradingFinalAnnotatedAsset(
		t.Context(), "mingming", artifact.ArtifactID,
	); !errors.Is(err, k12storage.ErrGradingFinalAnnotatedAssetUnavailable) {
		t.Fatalf("committed artifact with missing bytes must fail closed: %v", err)
	}
}

func TestCommitGradingFinalAnnotatedAssetRejectsMissingCrossOwnerAndDigestDrift(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	store, db := openAnnotatedArtifactStore(t, ":memory:")
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	repository := &usecase.PageAssetRepository{Records: store}
	annotated := annotatedArtifactPNG(t, 200)
	ready, err := repository.Persist(ctx, "guardian-1", "mingming", annotated)
	if err != nil {
		t.Fatal(err)
	}
	job := newGradingJobRecord(t, "mingming", "annotated-fail-closed")
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	base := k12.GradingFinalArtifact{
		AgentName: "mingming", JobID: job.RecordID,
		StructureVersion: k12.GradingFinalArtifactStructureVersion,
		CoverageStatus:   k12.GradingFinalArtifactCoverageComplete,
		TotalCount:       1, PublishedCount: 1,
		OrderedCurrentDigestsJSON: `["assessment"]`,
		CanonicalMarkdown:         "# 批改结果",
		SummaryInvocationID:       "summary",
		AnnotatedAssetOwnerScope:  "guardian-1",
		AnnotatedAssetID:          ready.Metadata.PageAssetID,
		AnnotatedMIME:             ready.Metadata.MediaType,
		AnnotatedDigest:           ready.Metadata.ContentDigest,
		OriginalSourceDigest:      strings.Repeat("d", 64),
	}

	crossOwner := base
	crossOwner.AnnotatedAssetOwnerScope = "guardian-2"
	crossOwner.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(crossOwner)
	if _, _, err := store.CommitGradingFinalArtifact(ctx, crossOwner, 0); !errors.Is(err, k12storage.ErrGradingFinalArtifactConflict) {
		t.Fatalf("cross-owner annotated asset must fail closed: %v", err)
	}

	digestDrift := base
	digestDrift.AnnotatedDigest = strings.Repeat("e", 64)
	digestDrift.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(digestDrift)
	if _, _, err := store.CommitGradingFinalArtifact(ctx, digestDrift, 0); !errors.Is(err, k12storage.ErrGradingFinalArtifactConflict) &&
		!errors.Is(err, k12.ErrGradingFinalArtifactInvariant) {
		t.Fatalf("annotated digest drift must fail closed: %v", err)
	}

	owner, file, err := assetstore.Parse(ready.Metadata.PageAssetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assetstore.Remove(owner, ready.Metadata.PageAssetID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CommitGradingFinalArtifact(ctx, func() k12.GradingFinalArtifact {
		missing := base
		missing.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(missing)
		return missing
	}(), 0); !errors.Is(err, k12storage.ErrGradingFinalArtifactConflict) {
		t.Fatalf("missing annotated bytes must fail closed (file=%s): %v", file, err)
	}
}
