package k12

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestREGK12RecognitionPlanVersion20260808001ClassifiesDenseGeometryAtFrozenThresholds(t *testing.T) {
	for _, test := range []struct {
		name          string
		width, height int
		want          RecognitionPageClass
	}{
		{name: "low resolution minimum", width: 800, height: 1200, want: RecognitionPageDense},
		{name: "low resolution width below minimum", width: 799, height: 1200, want: RecognitionPageOrdinary},
		{name: "low resolution height below minimum", width: 800, height: 1199, want: RecognitionPageOrdinary},
		{name: "low resolution aspect boundary", width: 900, height: 1200, want: RecognitionPageDense},
		{name: "low resolution aspect outside boundary", width: 901, height: 1200, want: RecognitionPageOrdinary},
		{name: "legacy tall aspect boundary", width: 1333, height: 1600, want: RecognitionPageDense},
		{name: "legacy tall aspect outside boundary", width: 1334, height: 1600, want: RecognitionPageOrdinary},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := recognitionPageClassificationPNG(t, test.width, test.height)
			if got := ClassifyRecognitionPage(raw); got != test.want {
				t.Fatalf("ClassifyRecognitionPage(%dx%d)=%q want=%q", test.width, test.height, got, test.want)
			}
			_, fallbackDense := DenseWorksheetFallbackPhysicalInputs(raw)
			if fallbackDense != (test.want == RecognitionPageDense) {
				t.Fatalf("legacy dense fallback=%v classification=%q", fallbackDense, test.want)
			}
		})
	}

	if got := ClassifyRecognitionPage([]byte("not-an-image")); got != RecognitionPageOrdinary {
		t.Fatalf("invalid image classification=%q want=%q", got, RecognitionPageOrdinary)
	}
}

func recognitionPageClassificationPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatalf("encode %dx%d classification fixture: %v", width, height, err)
	}
	return encoded.Bytes()
}
