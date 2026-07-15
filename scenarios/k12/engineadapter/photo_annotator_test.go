package engineadapter

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
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
	// The worksheet itself stays untouched: annotations are compact status
	// glyphs beside each answer instead of translucent rectangles over handwriting.
	for _, point := range []image.Point{{X: 70, Y: 90}, {X: 130, Y: 185}} {
		if pixel := color.RGBAModel.Convert(out.At(point.X, point.Y)).(color.RGBA); pixel != (color.RGBA{255, 255, 255, 255}) {
			t.Fatalf("answer content at %v was tinted/boxed: %#v", point, pixel)
		}
	}
	green, red := 0, 0
	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := 0; x < out.Bounds().Dx(); x++ {
			pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA)
			if int(pixel.G) > 130 && int(pixel.G) > int(pixel.R)+30 && int(pixel.G) > int(pixel.B)+30 {
				green++
			}
			if int(pixel.R) > 180 && int(pixel.R) > int(pixel.G)+40 && int(pixel.R) > int(pixel.B)+40 {
				red++
			}
		}
	}
	if green < 40 || red < 40 {
		t.Fatalf("compact ✓/✗ glyph colors missing: green=%d red=%d", green, red)
	}
}

func TestPhotoAnnotator_BenchmarkStyleUsesCompactGlyphsWithoutTintingAnswerArea(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 240, 240))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatal(err)
	}

	got, err := NewPhotoAnnotator().Annotate(context.Background(), raw.Bytes(), []usecase.PhotoAnnotation{
		{BBox: usecase.BBox{X: 0.20, Y: 0.20, W: 0.50, H: 0.25}, Correct: true},
		{BBox: usecase.BBox{X: 0.20, Y: 0.60, W: 0.50, H: 0.20}, Correct: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := png.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}

	// 对标主流作业批注：不再用半透明色块/大矩形遮住孩子原笔迹，只在答案旁放状态符号。
	for _, point := range []image.Point{{X: 72, Y: 84}, {X: 72, Y: 168}} {
		if pixel := color.RGBAModel.Convert(out.At(point.X, point.Y)).(color.RGBA); pixel != (color.RGBA{255, 255, 255, 255}) {
			t.Fatalf("answer content at %v was tinted/boxed: %#v", point, pixel)
		}
	}
	green, red, greenX, redX, greenY, redY := 0, 0, 0, 0, 0, 0
	for y := 0; y < 240; y++ {
		for x := 0; x < 240; x++ {
			pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA)
			if int(pixel.G) > 130 && int(pixel.G) > int(pixel.R)+30 && int(pixel.G) > int(pixel.B)+30 {
				green++
				greenX += x
				greenY += y
			}
			if int(pixel.R) > 180 && int(pixel.R) > int(pixel.G)+40 && int(pixel.R) > int(pixel.B)+40 {
				red++
				redX += x
				redY += y
			}
		}
	}
	if green < 40 || red < 40 {
		t.Fatalf("compact ✓/✗ glyph colors missing: green=%d red=%d", green, red)
	}
	// A stabilized answer bbox may include the printed prompt above the actual handwriting.
	// Put the glyph near the lower answer/final-result area, never at the bbox's top edge.
	if greenY/green < 78 || redY/red < 168 {
		t.Fatalf("glyphs sit above the answer area: green_avg_y=%d red_avg_y=%d", greenY/green, redY/red)
	}
	if greenX/green > 50 || redX/red > 50 {
		t.Fatalf("broad bbox moved glyph away from its stable left rail: green_avg_x=%d red_avg_x=%d", greenX/green, redX/red)
	}
}

func TestPhotoAnnotator_UnpositionedVerifiedItemsAppendNumberedStatusRail(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 240, 320))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatal(err)
	}

	got, err := NewPhotoAnnotator().Annotate(context.Background(), raw.Bytes(), []usecase.PhotoAnnotation{
		{QuestionNumber: 1, Correct: true},
		{QuestionNumber: 12, Correct: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := png.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	if out.Bounds().Dx() <= src.Bounds().Dx() || out.Bounds().Dy() != src.Bounds().Dy() {
		t.Fatalf("unpositioned results need a separate side rail without resizing the worksheet: got=%v source=%v", out.Bounds(), src.Bounds())
	}

	green, red, dark := 0, 0, 0
	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := src.Bounds().Dx(); x < out.Bounds().Dx(); x++ {
			pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA)
			if int(pixel.G) > 130 && int(pixel.G) > int(pixel.R)+30 && int(pixel.G) > int(pixel.B)+30 {
				green++
			}
			if int(pixel.R) > 180 && int(pixel.R) > int(pixel.G)+40 && int(pixel.R) > int(pixel.B)+40 {
				red++
			}
			if pixel.R < 90 && pixel.G < 90 && pixel.B < 90 {
				dark++
			}
		}
	}
	if green < 40 || red < 40 || dark < 20 {
		t.Fatalf("numbered status rail is missing verdict glyphs or question-number ink: green=%d red=%d dark=%d", green, red, dark)
	}
	// No guessed coordinates are ever burned onto the worksheet itself.
	for y := 0; y < src.Bounds().Dy(); y++ {
		for x := 0; x < src.Bounds().Dx(); x++ {
			if pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA); pixel != (color.RGBA{255, 255, 255, 255}) {
				t.Fatalf("fallback annotation modified worksheet pixel (%d,%d): %#v", x, y, pixel)
			}
		}
	}
}

func TestPhotoAnnotator_StatusRailDoesNotShiftTrustedBBoxCoordinates(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 240, 320))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatal(err)
	}

	got, err := NewPhotoAnnotator().Annotate(context.Background(), raw.Bytes(), []usecase.PhotoAnnotation{
		{BBox: usecase.BBox{X: 0.50, Y: 0.20, W: 0.10, H: 0.10}, QuestionNumber: 1, Correct: true},
		{QuestionNumber: 2, Correct: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := png.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	greenAtOriginalAnchor := 0
	for y := 60; y <= 110; y++ {
		for x := 80; x <= 130; x++ {
			pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA)
			if int(pixel.G) > 130 && int(pixel.G) > int(pixel.R)+30 && int(pixel.G) > int(pixel.B)+30 {
				greenAtOriginalAnchor++
			}
		}
	}
	if greenAtOriginalAnchor < 40 {
		t.Fatalf("trusted bbox was shifted when the fallback rail expanded the canvas: green_at_original_anchor=%d", greenAtOriginalAnchor)
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
