package engineadapter

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPhotoAnnotator_DrawsTrustedMarksAndKeepsDimensions(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 200, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 200; x++ {
			src.Set(x, y, color.White)
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatal(err)
	}

	got, err := NewPhotoAnnotator().Annotate(context.Background(), raw.Bytes(), []usecase.PhotoAnnotation{
		{BBox: usecase.BBox{X: 0.25, Y: 0.25, W: 0.25, H: 0.1}, Correct: true},
		{BBox: usecase.BBox{X: 0.55, Y: 0.55, W: 0.25, H: 0.1}, Correct: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.MIME != "image/png" || len(got.Data) == 0 {
		t.Fatalf("unexpected render result: %+v", got)
	}
	out, err := png.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("output is not PNG: %v", err)
	}
	if out.Bounds().Dx() != 200 || out.Bounds().Dy() != 300 {
		t.Fatalf("dimensions changed: %v", out.Bounds())
	}
	if color.RGBAModel.Convert(out.At(60, 80)).(color.RGBA) == (color.RGBA{255, 255, 255, 255}) {
		t.Fatal("correct bbox area was not annotated")
	}
	if color.RGBAModel.Convert(out.At(120, 175)).(color.RGBA) == (color.RGBA{255, 255, 255, 255}) {
		t.Fatal("wrong bbox area was not annotated")
	}
}

func TestPhotoAnnotator_FallsBackToBoundedJPEGWhenPhotoPNGIsTooLarge(t *testing.T) {
	// Deterministic high-entropy pixels model a noisy phone photo: PNG is large,
	// while a quality-controlled JPEG remains suitable for DingTalk upload.
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	var state uint32 = 1
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			state = state*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(state >> 24), G: uint8(state >> 16), B: uint8(state >> 8), A: 255})
		}
	}
	const budget = 100 << 10
	got, err := encodeAnnotatedPhoto(img, budget)
	if err != nil {
		t.Fatal(err)
	}
	if got.MIME != "image/jpeg" {
		t.Fatalf("large noisy photo MIME = %q, want JPEG fallback", got.MIME)
	}
	if len(got.Data) == 0 || len(got.Data) > budget {
		t.Fatalf("bounded output bytes = %d, want 1..%d", len(got.Data), budget)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("fallback output is not JPEG: %v", err)
	}
	if decoded.Bounds().Dx() <= 0 || decoded.Bounds().Dy() <= 0 {
		t.Fatalf("fallback dimensions invalid: %v", decoded.Bounds())
	}
}

func TestPhotoAnnotator_RejectsPixelBombBeforeDecode(t *testing.T) {
	// PNG signature + IHDR declares 100000×100000 without allocating the image.
	raw := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13, 'I', 'H', 'D', 'R',
		0, 1, 0x86, 0xa0, 0, 1, 0x86, 0xa0, 8, 2, 0, 0, 0, 0, 0, 0, 0}
	if _, err := NewPhotoAnnotator().Annotate(context.Background(), raw, nil); err == nil {
		t.Fatal("pixel bomb should be rejected")
	}
}
