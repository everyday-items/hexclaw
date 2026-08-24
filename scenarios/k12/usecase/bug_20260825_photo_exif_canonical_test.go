package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestK12PhotoEXIF6ImageTaskUsesOneCanonicalPixelDigest(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	ctx := context.Background()
	raw := photoEXIF6JPEGFixture(t, 120, 80)
	rawDigest := sha256.Sum256(raw)

	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	repository := &PageAssetRepository{Records: coordinator.Records}
	coordinator.PageAssets = repository
	coordinator.ReadAsset = func(string, string) ([]byte, error) {
		t.Fatal("PageAsset-backed image task must not use the raw asset reader")
		return nil, nil
	}

	ready, err := repository.Persist(ctx, "guardian-1", "mingming", raw)
	if err != nil {
		t.Fatalf("persist EXIF 6 PageAsset: %v", err)
	}
	if ready.Metadata.PixelWidth != 80 || ready.Metadata.PixelHeight != 120 {
		t.Fatalf("display dimensions=%dx%d want=80x120", ready.Metadata.PixelWidth, ready.Metadata.PixelHeight)
	}
	expected, err := canonicalProblemSourceImage(ready)
	if err != nil {
		t.Fatalf("build expected canonical pixels: %v", err)
	}
	assertCanonicalPhotoDimensions(t, expected.Data, 80, 120)
	expectedDigest := imageBytesDigest([][]byte{expected.Data})

	input := testCreateImageTaskInput()
	input.OwnerScope = "guardian-1"
	input.SourceAssetRefs = []string{ready.Metadata.PageAssetID}
	view, created, err := createAndRunImageTask(t, coordinator, input)
	if err != nil || !created {
		t.Fatalf("run EXIF 6 image task: created=%v err=%v", created, err)
	}
	if view.Dispatch.SourceDigest != expectedDigest {
		t.Fatalf("frozen source digest=%q want canonical %q", view.Dispatch.SourceDigest, expectedDigest)
	}
	if !bytes.Equal(classifier.image, expected.Data) {
		t.Fatal("classifier did not consume the canonical metadata-free pixels")
	}
	if !bytes.Equal(grading.input.Photo.Image, expected.Data) {
		t.Fatal("grading did not consume the same canonical pixels as classification")
	}

	restarted := &ImageTaskCoordinator{
		Records: coordinator.Records, PageAssets: repository,
	}
	replayed, err := restarted.readDispatchImages(ctx, view.Dispatch)
	if err != nil || len(replayed) != 1 || !bytes.Equal(replayed[0], expected.Data) {
		t.Fatalf("restart canonical pixels drifted: images=%d err=%v", len(replayed), err)
	}
	if imageBytesDigest(replayed) != expectedDigest {
		t.Fatal("restart canonical digest drifted")
	}

	reopened, err := repository.OpenReady(
		ctx, "guardian-1", "mingming", ready.Metadata.PageAssetID,
	)
	if err != nil {
		t.Fatalf("reopen immutable raw PageAsset: %v", err)
	}
	if sha256.Sum256(reopened.Data) != rawDigest || !bytes.Equal(reopened.Data, raw) {
		t.Fatal("canonical consumption mutated immutable PageAsset bytes or SHA")
	}
}

func TestK12PhotoEXIF6RestartedRetryReusesCanonicalPixels(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	ctx := context.Background()
	raw := photoEXIF6JPEGFixture(t, 120, 80)

	firstClassifier := &imageTaskClassifierStub{
		err: definitiveImageTaskTestError{message: "provider unavailable"},
	}
	coordinator, _ := newImageTaskCoordinatorForTest(t, firstClassifier)
	repository := &PageAssetRepository{Records: coordinator.Records}
	coordinator.PageAssets = repository
	ready, err := repository.Persist(ctx, "guardian-1", "mingming", raw)
	if err != nil {
		t.Fatalf("persist EXIF 6 PageAsset: %v", err)
	}
	expected, err := canonicalProblemSourceImage(ready)
	if err != nil {
		t.Fatalf("build expected canonical pixels: %v", err)
	}

	input := testCreateImageTaskInput()
	input.OwnerScope = "guardian-1"
	input.SourceAssetRefs = []string{ready.Metadata.PageAssetID}
	prepared, created, err := coordinator.Create(ctx, input)
	if err != nil || !created {
		t.Fatalf("prepare EXIF 6 image task: created=%v err=%v", created, err)
	}
	if _, err := coordinator.Run(ctx, input.AgentName, prepared.Dispatch.DispatchID); err == nil {
		t.Fatal("first classification failure expected")
	}
	if !bytes.Equal(firstClassifier.image, expected.Data) {
		t.Fatal("first classification did not consume canonical pixels")
	}
	failed, err := coordinator.Get(ctx, input.AgentName, prepared.Dispatch.DispatchID)
	if err != nil {
		t.Fatalf("read failed dispatch: %v", err)
	}

	retryClassifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	restarted := restartImageTaskCoordinator(coordinator, retryClassifier)
	restarted.PageAssets = repository
	retried, err := restarted.Retry(
		ctx,
		input.AgentName,
		prepared.Dispatch.DispatchID,
		failed.Dispatch.Version,
	)
	if err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if !bytes.Equal(retryClassifier.image, expected.Data) {
		t.Fatal("restarted retry classification canonical pixels drifted")
	}
	grading := restarted.Grading.(*imageTaskGradingStub)
	if !bytes.Equal(grading.input.Photo.Image, expected.Data) {
		t.Fatal("restarted retry grading did not receive the canonical pixels")
	}
	if retried.Dispatch.SourceDigest != imageBytesDigest([][]byte{expected.Data}) {
		t.Fatal("restarted retry changed the frozen canonical digest")
	}
}

func photoEXIF6JPEGFixture(t *testing.T, width, height int) []byte {
	t.Helper()
	source := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source.SetRGBA(x, y, color.RGBA{
				R: uint8(20 + x*170/max(1, width-1)),
				G: uint8(30 + y*160/max(1, height-1)),
				B: uint8(40 + (x+y)*120/max(1, width+height-2)),
				A: 0xff,
			})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: 94}); err != nil {
		t.Fatal(err)
	}

	tiff := make([]byte, 26)
	copy(tiff[0:2], "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 1)
	binary.LittleEndian.PutUint16(tiff[10:12], 0x0112)
	binary.LittleEndian.PutUint16(tiff[12:14], 3)
	binary.LittleEndian.PutUint32(tiff[14:18], 1)
	binary.LittleEndian.PutUint16(tiff[18:20], 6)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segmentLength := len(payload) + 2
	segment := []byte{0xff, 0xe1, byte(segmentLength >> 8), byte(segmentLength)}
	segment = append(segment, payload...)
	raw := encoded.Bytes()
	result := make([]byte, 0, len(raw)+len(segment))
	result = append(result, raw[:2]...)
	result = append(result, segment...)
	return append(result, raw[2:]...)
}

func assertCanonicalPhotoDimensions(
	t *testing.T,
	data []byte,
	wantWidth, wantHeight int,
) {
	t.Helper()
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode canonical image config: %v", err)
	}
	if format != "png" || config.Width != wantWidth || config.Height != wantHeight {
		t.Fatalf("canonical image=%s %dx%d want=png %dx%d",
			format, config.Width, config.Height, wantWidth, wantHeight)
	}
}
