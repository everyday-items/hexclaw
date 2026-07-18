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
	// The independent anchorer returns a tight answer bbox. Put the glyph beside its
	// right edge and in the upper-middle answer band, never over the handwriting.
	if greenY/green < 70 || redY/red < 158 {
		t.Fatalf("glyphs sit above the answer area: green_avg_y=%d red_avg_y=%d", greenY/green, redY/red)
	}
	if greenX/green < 155 || redX/red < 155 {
		t.Fatalf("tight bbox did not put glyph beside its right edge: green_avg_x=%d red_avg_x=%d", greenX/green, redX/red)
	}
}

func TestPhotoAnnotator_PlacesGlyphAtAnswerSideInsteadOfQuestionStart(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 240, 240))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatal(err)
	}

	got, err := NewPhotoAnnotator().Annotate(context.Background(), raw.Bytes(), []usecase.PhotoAnnotation{{
		BBox: usecase.BBox{X: 0.20, Y: 0.20, W: 0.50, H: 0.25}, Correct: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := png.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	green, sumX := 0, 0
	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := 0; x < out.Bounds().Dx(); x++ {
			pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA)
			if int(pixel.G) > 130 && int(pixel.G) > int(pixel.R)+30 && int(pixel.G) > int(pixel.B)+30 {
				green++
				sumX += x
			}
		}
	}
	if green < 40 || sumX/green < 155 {
		t.Fatalf("verdict glyph should sit beside the answer at the bbox right edge, not beside the printed question: green=%d avg_x=%d", green, sumX/maxInt(1, green))
	}
}

func TestPhotoAnnotator_UsesVerifiedAnswerBBoxRightEdge(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1000, 400))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatal(err)
	}

	got, err := NewPhotoAnnotator().Annotate(context.Background(), raw.Bytes(), []usecase.PhotoAnnotation{{
		BBox: usecase.BBox{X: 0.10, Y: 0.20, W: 0.80, H: 0.30}, Correct: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := png.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	green, sumX := 0, 0
	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := 0; x < out.Bounds().Dx(); x++ {
			pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA)
			if int(pixel.G) > 130 && int(pixel.G) > int(pixel.R)+30 && int(pixel.G) > int(pixel.B)+30 {
				green++
				sumX += x
			}
		}
	}
	if green == 0 {
		t.Fatal("missing green verdict glyph")
	}
	avgX := sumX / green
	if avgX < 875 || avgX > 925 {
		t.Fatalf("verified answer bbox should place the glyph beside its right edge: avg_x=%d", avgX)
	}
}

func TestLayoutPhotoMarks_ResolvesActualPixelCollisionWithoutDroppingVerdicts(t *testing.T) {
	marks := []usecase.PhotoAnnotation{
		{QuestionNumber: 1, BBox: usecase.BBox{X: 0.50, Y: 0.20, W: 0.10, H: 0.08}, Correct: true},
		{QuestionNumber: 2, BBox: usecase.BBox{X: 0.50, Y: 0.20, W: 0.10, H: 0.08}, Correct: false},
	}
	placements := layoutPhotoMarks(image.Rect(0, 0, 1280, 1707), marks)
	if len(placements) != len(marks) {
		t.Fatalf("pixel layout dropped a verified verdict: got=%d want=%d", len(placements), len(marks))
	}
	if placements[0].bounds.Overlaps(placements[1].bounds) {
		t.Fatalf("pixel layout left verdict glyphs overlapping: first=%v second=%v", placements[0].bounds, placements[1].bounds)
	}
}

func TestPhotoAnnotator_LocatorTileUsesUpperAnswerBandInsteadOfFollowingSectionHeading(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1000, 1000))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatal(err)
	}
	got, err := NewPhotoAnnotator().Annotate(context.Background(), raw.Bytes(), []usecase.PhotoAnnotation{{
		BBox: usecase.BBox{X: 0.20, Y: 0.20, W: 0.30, H: 0.12}, Correct: false,
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := png.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	red, sumY := 0, 0
	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := 0; x < out.Bounds().Dx(); x++ {
			pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA)
			if int(pixel.R) > 180 && int(pixel.R) > int(pixel.G)+40 && int(pixel.R) > int(pixel.B)+40 {
				red++
				sumY += y
			}
		}
	}
	if red == 0 {
		t.Fatal("missing red verdict glyph")
	}
	avgY := sumY / red
	if avgY < 228 || avgY > 252 {
		t.Fatalf("locator tile glyph should sit in the upper answer band, not at a following heading: avg_y=%d", avgY)
	}
}

func TestPhotoAnnotator_UnpositionedVerifiedItemsDoNotAppendStatusRailOrGuessCoordinates(t *testing.T) {
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
	if out.Bounds() != src.Bounds() {
		t.Fatalf("unpositioned results must not resize the original worksheet: got=%v source=%v", out.Bounds(), src.Bounds())
	}

	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := 0; x < out.Bounds().Dx(); x++ {
			if pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA); pixel != (color.RGBA{255, 255, 255, 255}) {
				t.Fatalf("unpositioned verdict guessed a worksheet coordinate at (%d,%d): %#v", x, y, pixel)
			}
		}
	}
}

func TestPhotoAnnotator_UnpositionedVerdictDoesNotShiftTrustedBBoxOrDimensions(t *testing.T) {
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
	if out.Bounds() != src.Bounds() {
		t.Fatalf("mixed positioned/unpositioned marks changed worksheet dimensions: got=%v source=%v", out.Bounds(), src.Bounds())
	}
	greenAtOriginalAnswerSide := 0
	for y := 60; y <= 110; y++ {
		for x := 100; x <= 145; x++ {
			pixel := color.RGBAModel.Convert(out.At(x, y)).(color.RGBA)
			if int(pixel.G) > 130 && int(pixel.G) > int(pixel.R)+30 && int(pixel.G) > int(pixel.B)+30 {
				greenAtOriginalAnswerSide++
			}
		}
	}
	if greenAtOriginalAnswerSide < 40 {
		t.Fatalf("trusted bbox moved away from its original answer side: green_at_original_answer_side=%d", greenAtOriginalAnswerSide)
	}
}

func TestPhotoAnnotator_BenchmarkGlyphIsPlainColoredStrokeWithoutFilledBadge(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 240, 240))
	draw.Draw(src, src.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatal(err)
	}
	got, err := NewPhotoAnnotator().Annotate(context.Background(), raw.Bytes(), []usecase.PhotoAnnotation{{
		BBox: usecase.BBox{X: 0.20, Y: 0.20, W: 0.50, H: 0.25}, Correct: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := png.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	// A plain ✓ has no
	// colored circular badge above the stroke, matching mainstream homework-marking visuals.
	pixel := color.RGBAModel.Convert(out.At(138, 72)).(color.RGBA)
	if pixel != (color.RGBA{255, 255, 255, 255}) {
		t.Fatalf("verdict should be a plain colored stroke, not a filled badge: %#v", pixel)
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
