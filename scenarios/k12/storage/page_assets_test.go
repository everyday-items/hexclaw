package k12storage_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func pageAssetMetadata(owner, agent, hexDigit string) k12storage.PageAssetMetadata {
	digest := strings.Repeat(hexDigit, 64)
	return k12storage.PageAssetMetadata{
		OwnerScope:               owner,
		AgentName:                agent,
		PageAssetID:              "asset://" + agent + "/" + digest + ".png",
		ContentDigest:            digest,
		MediaType:                "image/png",
		SizeBytes:                68,
		PixelWidth:               1,
		PixelHeight:              1,
		OrientationPolicy:        k12storage.PageAssetOrientationUnverified,
		OrientationPolicyVersion: "unverified-v1",
		TransformChainJSON:       `[]`,
	}
}

func TestPageAssetPrepareIsOwnerScopedIdempotentAndImmutable(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	want := pageAssetMetadata("guardian-1", "mingming", "a")

	first, created, err := store.PreparePageAsset(ctx, want)
	if err != nil || !created {
		t.Fatalf("prepare PageAsset: asset=%+v created=%v err=%v", first, created, err)
	}
	if first.StorageState != k12storage.PageAssetStorageStaging ||
		first.ReadyAt != 0 || first.LastError != "" ||
		first.CreatedAt <= 0 || first.UpdatedAt != first.CreatedAt {
		t.Fatalf("prepared PageAsset did not freeze staging state: %+v", first)
	}

	replay := want
	replay.TransformChainJSON = "[ ]"
	second, created, err := store.PreparePageAsset(ctx, replay)
	if err != nil || created || second != first {
		t.Fatalf("exact replay: asset=%+v created=%v err=%v want=%+v", second, created, err, first)
	}

	changed := want
	changed.PixelWidth = 2
	if _, _, err := store.PreparePageAsset(ctx, changed); !errors.Is(err, k12storage.ErrPageAssetConflict) {
		t.Fatalf("same identity with changed dimensions must fail closed: %v", err)
	}
	changedDigest := want
	changedDigest.ContentDigest = strings.Repeat("b", 64)
	if _, _, err := store.PreparePageAsset(ctx, changedDigest); !errors.Is(err, k12storage.ErrPageAssetConflict) {
		t.Fatalf("existing PageAsset with request-internal digest drift must conflict: %v", err)
	}
	crossOwner := want
	crossOwner.OwnerScope = "guardian-2"
	if _, _, err := store.PreparePageAsset(ctx, crossOwner); !errors.Is(err, k12storage.ErrPageAssetConflict) {
		t.Fatalf("same Agent/PageAsset rebound to another owner must conflict: %v", err)
	}

	if _, err := store.GetReadyPageAsset(ctx, want.OwnerScope, want.AgentName, want.PageAssetID); !errors.Is(err, k12storage.ErrPageAssetNotFound) {
		t.Fatalf("staging asset leaked through ready-only lookup: %v", err)
	}
	ready, err := store.MarkPageAssetReady(ctx, want.OwnerScope, want.AgentName, want.PageAssetID)
	if err != nil || ready.StorageState != k12storage.PageAssetStorageReady || ready.ReadyAt <= 0 {
		t.Fatalf("mark ready: asset=%+v err=%v", ready, err)
	}
	got, err := store.GetReadyPageAsset(ctx, want.OwnerScope, want.AgentName, want.PageAssetID)
	if err != nil || got != ready {
		t.Fatalf("get ready: got=%+v err=%v want=%+v", got, err, ready)
	}
	for _, scope := range []struct {
		owner string
		agent string
	}{
		{owner: "guardian-2", agent: want.AgentName},
		{owner: want.OwnerScope, agent: "lele"},
	} {
		if _, err := store.GetReadyPageAsset(ctx, scope.owner, scope.agent, want.PageAssetID); !errors.Is(err, k12storage.ErrPageAssetNotFound) {
			t.Fatalf("cross-scope ready lookup owner=%q agent=%q: %v", scope.owner, scope.agent, err)
		}
	}

	afterReady, created, err := store.PreparePageAsset(ctx, want)
	if err != nil || created || afterReady != ready {
		t.Fatalf("prepare replay after ready: asset=%+v created=%v err=%v", afterReady, created, err)
	}
	if _, err := db.Exec(`
UPDATE k12_page_assets SET content_digest=?
WHERE owner_scope=? AND agent_name=? AND page_asset_id=?
`, strings.Repeat("b", 64), want.OwnerScope, want.AgentName, want.PageAssetID); err == nil {
		t.Fatal("database accepted in-place PageAsset identity mutation")
	}
	if _, err := db.Exec(`
UPDATE k12_page_assets SET orientation_policy_version='attacker-v2'
WHERE owner_scope=? AND agent_name=? AND page_asset_id=?
`, want.OwnerScope, want.AgentName, want.PageAssetID); err == nil {
		t.Fatal("database accepted in-place PageAsset orientation policy version mutation")
	}

	newInvalid := pageAssetMetadata("guardian-1", "mingming", "8")
	newInvalid.ContentDigest = strings.Repeat("b", 64)
	if _, _, err := store.PreparePageAsset(ctx, newInvalid); !errors.Is(err, k12storage.ErrPageAssetInvalid) {
		t.Fatalf("new PageAsset must still pass complete identity validation: %v", err)
	}

	verified := pageAssetMetadata("guardian-1", "mingming", "9")
	verified.OrientationPolicy = k12storage.PageAssetOrientationVerified
	verified.OrientationPolicyVersion = ""
	if _, _, err := store.PreparePageAsset(ctx, verified); !errors.Is(err, k12storage.ErrPageAssetInvalid) {
		t.Fatalf("verified orientation without a frozen version must be invalid: %v", err)
	}
	verified.OrientationPolicyVersion = "exif-normalized-v1"
	prepared, created, err := store.PreparePageAsset(ctx, verified)
	if err != nil || !created || prepared.OrientationPolicyVersion != "exif-normalized-v1" {
		t.Fatalf("prepare explicitly versioned orientation: asset=%+v created=%v err=%v", prepared, created, err)
	}
}

func TestPageAssetStateTransitionsAreCASAndReplaySafe(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()

	failedInput := pageAssetMetadata("guardian-1", "mingming", "c")
	if _, _, err := store.PreparePageAsset(ctx, failedInput); err != nil {
		t.Fatal(err)
	}
	failed, err := store.MarkPageAssetFailed(
		ctx,
		failedInput.OwnerScope,
		failedInput.AgentName,
		failedInput.PageAssetID,
		"durable write failed",
	)
	if err != nil || failed.StorageState != k12storage.PageAssetStorageFailed ||
		failed.LastError != "durable write failed" {
		t.Fatalf("mark failed: asset=%+v err=%v", failed, err)
	}
	replayedFailure, err := store.MarkPageAssetFailed(
		ctx,
		failedInput.OwnerScope,
		failedInput.AgentName,
		failedInput.PageAssetID,
		"durable write failed",
	)
	if err != nil || replayedFailure != failed {
		t.Fatalf("exact failed replay: asset=%+v err=%v", replayedFailure, err)
	}
	if _, err := store.MarkPageAssetFailed(
		ctx,
		failedInput.OwnerScope,
		failedInput.AgentName,
		failedInput.PageAssetID,
		"different failure",
	); !errors.Is(err, k12storage.ErrPageAssetConflict) {
		t.Fatalf("failed reason rewrite must conflict: %v", err)
	}
	if _, err := store.MarkPageAssetReady(ctx, failedInput.OwnerScope, failedInput.AgentName, failedInput.PageAssetID); !errors.Is(err, k12storage.ErrPageAssetConflict) {
		t.Fatalf("failed asset must not become ready: %v", err)
	}
	retried, err := store.RetryPageAssetStaging(
		ctx,
		failedInput.OwnerScope,
		failedInput.AgentName,
		failedInput.PageAssetID,
	)
	if err != nil || retried.StorageState != k12storage.PageAssetStorageStaging ||
		retried.LastError != "" || retried.ReadyAt != 0 || retried.UpdatedAt <= failed.UpdatedAt {
		t.Fatalf("retry failed -> staging: asset=%+v err=%v", retried, err)
	}
	replayedRetry, err := store.RetryPageAssetStaging(
		ctx,
		failedInput.OwnerScope,
		failedInput.AgentName,
		failedInput.PageAssetID,
	)
	if err != nil || replayedRetry != retried {
		t.Fatalf("exact staging retry replay: asset=%+v err=%v", replayedRetry, err)
	}

	readyInput := pageAssetMetadata("guardian-1", "mingming", "d")
	if _, _, err := store.PreparePageAsset(ctx, readyInput); err != nil {
		t.Fatal(err)
	}
	ready, err := store.MarkPageAssetReady(ctx, readyInput.OwnerScope, readyInput.AgentName, readyInput.PageAssetID)
	if err != nil {
		t.Fatal(err)
	}
	replayedReady, err := store.MarkPageAssetReady(ctx, readyInput.OwnerScope, readyInput.AgentName, readyInput.PageAssetID)
	if err != nil || replayedReady != ready {
		t.Fatalf("exact ready replay: asset=%+v err=%v", replayedReady, err)
	}
	retryReady, err := store.RetryPageAssetStaging(
		ctx,
		readyInput.OwnerScope,
		readyInput.AgentName,
		readyInput.PageAssetID,
	)
	if err != nil || retryReady != ready {
		t.Fatalf("ready retry must be idempotent: asset=%+v err=%v", retryReady, err)
	}
	corrupt, err := store.MarkPageAssetCorrupt(
		ctx,
		readyInput.OwnerScope,
		readyInput.AgentName,
		readyInput.PageAssetID,
		"digest mismatch on readback",
	)
	if err != nil || corrupt.StorageState != k12storage.PageAssetStorageCorrupt ||
		corrupt.LastError != "digest mismatch on readback" || corrupt.ReadyAt != ready.ReadyAt {
		t.Fatalf("mark corrupt: asset=%+v err=%v", corrupt, err)
	}
	replayedCorrupt, err := store.MarkPageAssetCorrupt(
		ctx,
		readyInput.OwnerScope,
		readyInput.AgentName,
		readyInput.PageAssetID,
		"digest mismatch on readback",
	)
	if err != nil || replayedCorrupt != corrupt {
		t.Fatalf("exact corrupt replay: asset=%+v err=%v", replayedCorrupt, err)
	}
	if _, err := store.GetReadyPageAsset(ctx, readyInput.OwnerScope, readyInput.AgentName, readyInput.PageAssetID); !errors.Is(err, k12storage.ErrPageAssetNotFound) {
		t.Fatalf("corrupt asset leaked through ready lookup: %v", err)
	}
	if _, err := store.RetryPageAssetStaging(
		ctx,
		readyInput.OwnerScope,
		readyInput.AgentName,
		readyInput.PageAssetID,
	); !errors.Is(err, k12storage.ErrPageAssetConflict) {
		t.Fatalf("corrupt asset must not auto-retry: %v", err)
	}

	missing := pageAssetMetadata("guardian-1", "mingming", "e")
	if _, err := store.MarkPageAssetReady(ctx, missing.OwnerScope, missing.AgentName, missing.PageAssetID); !errors.Is(err, k12storage.ErrPageAssetNotFound) {
		t.Fatalf("mark missing ready must use PageAsset sentinel: %v", err)
	}
	if _, err := store.RetryPageAssetStaging(ctx, missing.OwnerScope, missing.AgentName, missing.PageAssetID); !errors.Is(err, k12storage.ErrPageAssetNotFound) {
		t.Fatalf("retry missing PageAsset must use PageAsset sentinel: %v", err)
	}
	if _, err := store.MarkPageAssetFailed(ctx, missing.OwnerScope, missing.AgentName, missing.PageAssetID, ""); !errors.Is(err, k12storage.ErrPageAssetInvalid) {
		t.Fatalf("empty durable failure reason must be invalid: %v", err)
	}
}

func TestPageAssetConcurrentPrepareAndReadyHaveOneDurableIdentity(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	want := pageAssetMetadata("guardian-concurrent", "mingming", "f")
	const requests = 64

	var createdCount atomic.Int32
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	wg.Add(requests)
	for range requests {
		go func() {
			defer wg.Done()
			stored, created, err := store.PreparePageAsset(ctx, want)
			if err == nil && (stored.OwnerScope != want.OwnerScope || stored.PageAssetID != want.PageAssetID) {
				err = errors.New("concurrent prepare returned another PageAsset identity")
			}
			if created {
				createdCount.Add(1)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent prepare: %v", err)
		}
	}
	if got := createdCount.Load(); got != 1 {
		t.Fatalf("concurrent identical prepares created=%d rows, want 1", got)
	}
	var rowCount int
	if err := db.QueryRow(`
SELECT COUNT(*) FROM k12_page_assets
WHERE owner_scope=? AND agent_name=? AND page_asset_id=?
`, want.OwnerScope, want.AgentName, want.PageAssetID).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Fatalf("durable PageAsset identities=%d, want 1", rowCount)
	}

	errCh := make(chan error, requests)
	wg.Add(requests)
	for range requests {
		go func() {
			defer wg.Done()
			stored, err := store.MarkPageAssetReady(ctx, want.OwnerScope, want.AgentName, want.PageAssetID)
			if err == nil && stored.StorageState != k12storage.PageAssetStorageReady {
				err = errors.New("concurrent ready did not return ready state")
			}
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent mark ready: %v", err)
		}
	}
}
