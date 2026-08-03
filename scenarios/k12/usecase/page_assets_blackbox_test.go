package usecase_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPageAssetRepository_PersistOrdersStagingFileReady(t *testing.T) {
	repository, _, db := newPageAssetRepository(t)
	data := pageAssetPNG(t, 3, 2, color.NRGBA{R: 0x31, G: 0x71, B: 0xb1, A: 0xff})
	ctx := context.Background()
	var events []string
	inspectionCalls := 0
	repository.Inspect = func(agentName string, raw []byte) (assetstore.AssetInspection, error) {
		inspectionCalls++
		if inspectionCalls == 1 {
			events = append(events, "inspect")
		} else {
			events = append(events, "verify-ready")
		}
		return assetstore.Inspect(agentName, raw)
	}
	repository.Ensure = func(agentName string, raw []byte) (string, bool, error) {
		inspection, err := assetstore.Inspect(agentName, raw)
		if err != nil {
			return "", false, err
		}
		state, _ := pageAssetState(t, db, inspection.AssetID)
		if state != string(k12storage.PageAssetStorageStaging) {
			t.Fatalf("file persistence started before durable staging: state=%q", state)
		}
		events = append(events, "staging")
		id, created, err := assetstore.Ensure(agentName, raw)
		if err != nil {
			return "", false, err
		}
		if _, err := assetstore.PathFromID(id); err != nil {
			t.Fatalf("Ensure returned before final file publication: %v", err)
		}
		state, _ = pageAssetState(t, db, inspection.AssetID)
		if state != string(k12storage.PageAssetStorageStaging) {
			t.Fatalf("row advanced before Ensure returned: state=%q", state)
		}
		events = append(events, "file")
		return id, created, nil
	}

	ready, err := repository.Persist(ctx, "guardian-1", "mingming", data)
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	events = append(events, "ready")
	if !reflect.DeepEqual(events, []string{"inspect", "staging", "file", "verify-ready", "ready"}) {
		t.Fatalf("persist order=%v", events)
	}
	state, lastError := pageAssetState(t, db, ready.Metadata.PageAssetID)
	if state != string(k12storage.PageAssetStorageReady) || lastError != "" {
		t.Fatalf("durable row state=%q last_error=%q", state, lastError)
	}
	if ready.Metadata.OwnerScope != "guardian-1" || ready.Metadata.AgentName != "mingming" ||
		ready.Metadata.StorageState != k12storage.PageAssetStorageReady ||
		ready.Metadata.PixelWidth != 3 || ready.Metadata.PixelHeight != 2 ||
		ready.Metadata.SizeBytes != int64(len(data)) ||
		ready.Metadata.OrientationPolicy != k12storage.PageAssetOrientationVerified ||
		!bytes.Equal(ready.Data, data) {
		t.Fatalf("ready PageAsset fact drift: %+v", ready.Metadata)
	}
}

func TestPageAssetRepository_EnforcesOwnerAndAgentIsolation(t *testing.T) {
	repository, _, _ := newPageAssetRepository(t)
	data := pageAssetPNG(t, 2, 1, color.NRGBA{R: 0x90, G: 0x20, B: 0x10, A: 0xff})
	ctx := context.Background()
	ready, err := repository.Persist(ctx, "guardian-1", "mingming", data)
	if err != nil {
		t.Fatal(err)
	}

	for _, scope := range []struct {
		name  string
		owner string
		agent string
	}{
		{name: "cross owner", owner: "guardian-2", agent: "mingming"},
		{name: "cross agent", owner: "guardian-1", agent: "honghong"},
	} {
		t.Run(scope.name, func(t *testing.T) {
			if _, err := repository.OpenReady(ctx, scope.owner, scope.agent, ready.Metadata.PageAssetID); !errors.Is(err, k12storage.ErrPageAssetNotFound) {
				t.Fatalf("cross-scope OpenReady must be indistinguishable from missing: %v", err)
			}
		})
	}
	if _, err := repository.Persist(ctx, "guardian-2", "mingming", data); !errors.Is(err, k12storage.ErrPageAssetConflict) {
		t.Fatalf("same agent/content identity cannot be rebound to another owner: %v", err)
	}
	other, err := repository.Persist(ctx, "guardian-1", "honghong", data)
	if err != nil {
		t.Fatalf("same bytes in another registered agent scope: %v", err)
	}
	if other.Metadata.PageAssetID == ready.Metadata.PageAssetID ||
		!strings.HasPrefix(other.Metadata.PageAssetID, "asset://honghong/") {
		t.Fatalf("agent-scoped content identity drift: first=%q other=%q", ready.Metadata.PageAssetID, other.Metadata.PageAssetID)
	}
}

func TestPageAssetRepository_ReadyDriftMarksCorrupt(t *testing.T) {
	t.Run("bytes drift", func(t *testing.T) {
		repository, store, db := newPageAssetRepository(t)
		ctx := context.Background()
		ready, err := repository.Persist(
			ctx,
			"guardian-1",
			"mingming",
			pageAssetPNG(t, 2, 2, color.NRGBA{R: 0x22, A: 0xff}),
		)
		if err != nil {
			t.Fatal(err)
		}
		path, err := assetstore.PathFromID(ready.Metadata.PageAssetID)
		if err != nil {
			t.Fatal(err)
		}
		replacement := pageAssetPNG(t, 2, 2, color.NRGBA{G: 0xdd, A: 0xff})
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := repository.OpenReady(ctx, "guardian-1", "mingming", ready.Metadata.PageAssetID); !errors.Is(err, usecase.ErrPageAssetIntegrity) {
			t.Fatalf("ready bytes drift must fail with integrity error: %v", err)
		}
		assertPageAssetCorrupt(t, db, store, ready.Metadata)
	})

	t.Run("metadata drift", func(t *testing.T) {
		repository, store, db := newPageAssetRepository(t)
		ctx := context.Background()
		data := pageAssetPNG(t, 4, 3, color.NRGBA{B: 0xcc, A: 0xff})
		ready, err := repository.Persist(ctx, "guardian-1", "mingming", data)
		if err != nil {
			t.Fatal(err)
		}
		repository.Inspect = func(agentName string, raw []byte) (assetstore.AssetInspection, error) {
			inspection, err := assetstore.Inspect(agentName, raw)
			inspection.PixelWidth++
			return inspection, err
		}

		if _, err := repository.OpenReady(ctx, "guardian-1", "mingming", ready.Metadata.PageAssetID); !errors.Is(err, usecase.ErrPageAssetIntegrity) {
			t.Fatalf("ready metadata drift must fail with integrity error: %v", err)
		}
		assertPageAssetCorrupt(t, db, store, ready.Metadata)
	})
}

func TestPageAssetRepository_JPEGEXIFOrientationsFiveThroughEightSwapDimensions(t *testing.T) {
	repository, _, _ := newPageAssetRepository(t)
	ctx := context.Background()
	for orientation := 5; orientation <= 8; orientation++ {
		t.Run(string(rune('0'+orientation)), func(t *testing.T) {
			data := pageAssetJPEGWithEXIF(t, 3, 2, orientation)
			inspection, err := assetstore.Inspect("mingming", data)
			if err != nil || inspection.PixelWidth != 3 || inspection.PixelHeight != 2 {
				t.Fatalf("raw JPEG fixture must fully decode as 3x2: inspection=%+v err=%v", inspection, err)
			}
			ready, err := repository.Persist(ctx, "guardian-1", "mingming", data)
			if err != nil {
				t.Fatalf("orientation %d Persist: %v", orientation, err)
			}
			if ready.Metadata.PixelWidth != 2 || ready.Metadata.PixelHeight != 3 {
				t.Fatalf("orientation %d displayed dimensions=%dx%d, want 2x3",
					orientation, ready.Metadata.PixelWidth, ready.Metadata.PixelHeight)
			}
			if ready.Metadata.OrientationPolicy != k12storage.PageAssetOrientationVerified ||
				ready.Metadata.OrientationPolicyVersion != "source-pixel-exif-v1" ||
				!strings.Contains(ready.Metadata.TransformChainJSON, `"orientation":`+string(rune('0'+orientation))) {
				t.Fatalf("orientation %d policy facts=%+v", orientation, ready.Metadata)
			}
		})
	}
}

func TestPageAssetRepository_MalformedJPEGEXIFFailsClosedBeforeStaging(t *testing.T) {
	repository, _, db := newPageAssetRepository(t)
	data := pageAssetJPEGWithAPP1(t, 3, 2, []byte("Exif\x00\x00II"))
	inspection, err := assetstore.Inspect("mingming", data)
	if err != nil {
		t.Fatalf("fixture must be a fully decodable JPEG before EXIF policy validation: %v", err)
	}

	if _, err := repository.Persist(context.Background(), "guardian-1", "mingming", data); err == nil ||
		!strings.Contains(err.Error(), "EXIF") {
		t.Fatalf("malformed EXIF must fail closed: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_page_assets WHERE page_asset_id=?`, inspection.AssetID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("malformed EXIF reached staging: rows=%d", count)
	}
	if _, err := assetstore.PathFromID(inspection.AssetID); err == nil {
		t.Fatal("malformed EXIF reached filesystem persistence")
	}
}

func TestPageAssetRepository_FailedPreReadyAttemptKeepsSharedFinalAndRetryReusesIt(t *testing.T) {
	repository, _, db := newPageAssetRepository(t)
	ctx := context.Background()
	data := pageAssetPNG(t, 2, 3, color.NRGBA{R: 0xaa, G: 0x44, A: 0xff})
	inspection, err := assetstore.Inspect("mingming", data)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected failure after durable final publication")
	repository.Ensure = func(agentName string, raw []byte) (string, bool, error) {
		id, created, err := assetstore.Ensure(agentName, raw)
		if err != nil {
			return "", false, err
		}
		return id, created, injected
	}

	if _, err := repository.Persist(ctx, "guardian-1", "mingming", data); !errors.Is(err, injected) {
		t.Fatalf("pre-ready failure must surface: %v", err)
	}
	if _, err := assetstore.PathFromID(inspection.AssetID); err != nil {
		t.Fatalf("failed attempt deleted shared content-addressed final: %v", err)
	}
	state, lastError := pageAssetState(t, db, inspection.AssetID)
	if state != string(k12storage.PageAssetStorageFailed) || !strings.Contains(lastError, injected.Error()) {
		t.Fatalf("failed attempt state=%q last_error=%q", state, lastError)
	}

	repository.Ensure = nil
	ready, err := repository.Persist(ctx, "guardian-1", "mingming", data)
	if err != nil {
		t.Fatalf("retry existing shared final: %v", err)
	}
	if ready.Metadata.PageAssetID != inspection.AssetID ||
		ready.Metadata.StorageState != k12storage.PageAssetStorageReady {
		t.Fatalf("retry ready fact drift: %+v", ready.Metadata)
	}
	state, lastError = pageAssetState(t, db, inspection.AssetID)
	if state != string(k12storage.PageAssetStorageReady) || lastError != "" {
		t.Fatalf("retry did not clear failed state: state=%q last_error=%q", state, lastError)
	}
}

func newPageAssetRepository(
	t *testing.T,
) (*usecase.PageAssetRepository, *k12storage.Store, *sql.DB) {
	t.Helper()
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	db := openMigratedTestDB(t)
	for _, agentName := range []string{"mingming", "honghong"} {
		if _, err := db.Exec(`INSERT INTO agents(name) VALUES(?)`, agentName); err != nil {
			t.Fatal(err)
		}
	}
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		t.Fatal(err)
	}
	store := k12storage.NewStore(db, registry.Records)
	return &usecase.PageAssetRepository{Records: store}, store, db
}

func pageAssetState(t *testing.T, db *sql.DB, pageAssetID string) (string, string) {
	t.Helper()
	var state, lastError string
	if err := db.QueryRow(`
SELECT storage_state,last_error FROM k12_page_assets WHERE page_asset_id=?
`, pageAssetID).Scan(&state, &lastError); err != nil {
		t.Fatalf("read PageAsset state %q: %v", pageAssetID, err)
	}
	return state, lastError
}

func assertPageAssetCorrupt(
	t *testing.T,
	db *sql.DB,
	store *k12storage.Store,
	metadata k12storage.PageAssetMetadata,
) {
	t.Helper()
	state, lastError := pageAssetState(t, db, metadata.PageAssetID)
	if state != string(k12storage.PageAssetStorageCorrupt) || strings.TrimSpace(lastError) == "" {
		t.Fatalf("integrity drift state=%q last_error=%q, want corrupt with evidence", state, lastError)
	}
	if _, err := store.GetReadyPageAsset(
		context.Background(),
		metadata.OwnerScope,
		metadata.AgentName,
		metadata.PageAssetID,
	); !errors.Is(err, k12storage.ErrPageAssetNotFound) {
		t.Fatalf("corrupt PageAsset remained observable through ready-only lookup: %v", err)
	}
}

func pageAssetPNG(t *testing.T, width, height int, fill color.NRGBA) []byte {
	t.Helper()
	source := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source.SetNRGBA(x, y, fill)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, source); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func pageAssetJPEGWithEXIF(t *testing.T, width, height, orientation int) []byte {
	t.Helper()
	tiff := make([]byte, 26)
	copy(tiff[0:2], "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 1)
	binary.LittleEndian.PutUint16(tiff[10:12], 0x0112)
	binary.LittleEndian.PutUint16(tiff[12:14], 3)
	binary.LittleEndian.PutUint32(tiff[14:18], 1)
	binary.LittleEndian.PutUint16(tiff[18:20], uint16(orientation))
	return pageAssetJPEGWithAPP1(t, width, height, append([]byte("Exif\x00\x00"), tiff...))
}

func pageAssetJPEGWithAPP1(t *testing.T, width, height int, payload []byte) []byte {
	t.Helper()
	source := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source.SetRGBA(x, y, color.RGBA{
				R: uint8(30 + x*20),
				G: uint8(50 + y*30),
				B: 0x90,
				A: 0xff,
			})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	raw := encoded.Bytes()
	segmentLength := len(payload) + 2
	if segmentLength > 0xffff {
		t.Fatal("APP1 fixture payload is too large")
	}
	segment := []byte{0xff, 0xe1, byte(segmentLength >> 8), byte(segmentLength)}
	segment = append(segment, payload...)
	withAPP1 := make([]byte, 0, len(raw)+len(segment))
	withAPP1 = append(withAPP1, raw[:2]...)
	withAPP1 = append(withAPP1, segment...)
	return append(withAPP1, raw[2:]...)
}
