package engineadapter

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestREGK12CorrectWithProcessIssue20260809001AnnotatorUsesPurpleWarning(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 480, 480))
	for y := 0; y < source.Bounds().Dy(); y++ {
		for x := 0; x < source.Bounds().Dx(); x++ {
			source.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, source); err != nil {
		t.Fatal(err)
	}

	rendered, err := NewPhotoAnnotator().Annotate(context.Background(), raw.Bytes(), []usecase.PhotoAnnotation{{
		BBox:   usecase.BBox{X: 0.2, Y: 0.2, W: 0.35, H: 0.2},
		Status: usecase.PhotoItemStatus("correct_with_process_issue"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	annotated, err := png.Decode(bytes.NewReader(rendered.Data))
	if err != nil {
		t.Fatal(err)
	}

	warningPurple := color.RGBA{R: 165, G: 107, B: 214, A: 255}
	wrongRed := color.RGBA{R: 239, G: 68, B: 68, A: 255}
	purplePixels, redPixels := 0, 0
	for y := annotated.Bounds().Min.Y; y < annotated.Bounds().Max.Y; y++ {
		for x := annotated.Bounds().Min.X; x < annotated.Bounds().Max.X; x++ {
			pixel := color.RGBAModel.Convert(annotated.At(x, y)).(color.RGBA)
			switch pixel {
			case warningPurple:
				purplePixels++
			case wrongRed:
				redPixels++
			}
		}
	}
	if purplePixels == 0 || redPixels != 0 {
		t.Fatalf("process issue annotation must be a purple warning with no wrong red: purple=%d red=%d", purplePixels, redPixels)
	}
}
