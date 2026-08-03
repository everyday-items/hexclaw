package usecase

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var problemSourceOrientationPalette = map[byte]color.NRGBA{
	'A': {R: 0x10, A: 0xff},
	'B': {R: 0x30, A: 0xff},
	'C': {R: 0x50, A: 0xff},
	'D': {R: 0x70, A: 0xff},
	'E': {R: 0x90, A: 0xff},
	'F': {R: 0xb0, A: 0xff},
}

func problemSourceOrientationFixture(t *testing.T) ([]byte, image.Image) {
	t.Helper()
	source := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	labels := []string{"ABC", "DEF"}
	for y, row := range labels {
		for x := range row {
			source.SetNRGBA(x, y, problemSourceOrientationPalette[row[x]])
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes(), source
}

func problemSourcePixelLabel(t *testing.T, value color.Color) byte {
	t.Helper()
	got := color.NRGBAModel.Convert(value).(color.NRGBA)
	for label, want := range problemSourceOrientationPalette {
		if got == want {
			return label
		}
	}
	t.Fatalf("unexpected orientation fixture pixel %+v", got)
	return 0
}

func problemSourceImageRows(t *testing.T, value image.Image) []string {
	t.Helper()
	bounds := value.Bounds()
	rows := make([]string, 0, bounds.Dy())
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		row := make([]byte, 0, bounds.Dx())
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			row = append(row, problemSourcePixelLabel(t, value.At(x, y)))
		}
		rows = append(rows, string(row))
	}
	return rows
}

func TestProblemSourceEXIFOrientationOneThroughEight(t *testing.T) {
	_, source := problemSourceOrientationFixture(t)
	tests := []struct {
		orientation int
		rows        []string
	}{
		{1, []string{"ABC", "DEF"}},
		{2, []string{"CBA", "FED"}},
		{3, []string{"FED", "CBA"}},
		{4, []string{"DEF", "ABC"}},
		{5, []string{"AD", "BE", "CF"}},
		{6, []string{"DA", "EB", "FC"}},
		{7, []string{"FC", "EB", "DA"}},
		{8, []string{"CF", "BE", "AD"}},
	}
	for _, tt := range tests {
		t.Run(string(rune('0'+tt.orientation)), func(t *testing.T) {
			got, err := applyProblemSourceEXIFOrientation(source, tt.orientation)
			if err != nil {
				t.Fatalf("apply orientation %d: %v", tt.orientation, err)
			}
			rows := problemSourceImageRows(t, got)
			if !slicesEqual(rows, tt.rows) {
				t.Fatalf("orientation %d rows=%v want=%v", tt.orientation, rows, tt.rows)
			}
		})
	}
}

func TestProblemSourceOCRImageCropsNormalizedSourcePixelsForEveryEXIFOrientation(t *testing.T) {
	data, _ := problemSourceOrientationFixture(t)
	topLeft := []byte{'A', 'C', 'F', 'D', 'A', 'D', 'F', 'C'}
	for orientation := 1; orientation <= 8; orientation++ {
		t.Run(string(rune('0'+orientation)), func(t *testing.T) {
			width, height := 3, 2
			chain := []map[string]any{}
			if orientation != 1 {
				chain = append(chain, map[string]any{
					"operation": "exif_orientation", "orientation": orientation,
				})
			}
			if orientation >= 5 {
				width, height = height, width
			}
			chainJSON, err := json.Marshal(chain)
			if err != nil {
				t.Fatal(err)
			}
			ready := ReadyPageAsset{
				Metadata: k12storage.PageAssetMetadata{
					OrientationPolicy:        k12storage.PageAssetOrientationVerified,
					OrientationPolicyVersion: pageAssetOrientationPolicyVersion,
					TransformChainJSON:       string(chainJSON),
					PixelWidth:               width,
					PixelHeight:              height,
				},
				Data: data,
			}
			got, err := normalizeProblemSourceOCRImage(
				ready,
				&k12.SourcePixelRegion{X: 0, Y: 0, Width: 1, Height: 1},
			)
			if err != nil {
				t.Fatalf("normalize/crop orientation %d: %v", orientation, err)
			}
			decoded, err := png.Decode(bytes.NewReader(got.Data))
			if err != nil {
				t.Fatalf("decode normalized OCR PNG: %v", err)
			}
			if got.Width != 1 || got.Height != 1 || decoded.Bounds() != image.Rect(0, 0, 1, 1) {
				t.Fatalf("orientation %d crop dimensions=%dx%d bounds=%v", orientation, got.Width, got.Height, decoded.Bounds())
			}
			if label := problemSourcePixelLabel(t, decoded.At(0, 0)); label != topLeft[orientation-1] {
				t.Fatalf("orientation %d top-left=%c want=%c", orientation, label, topLeft[orientation-1])
			}
		})
	}
}

func TestProblemSourceOCRImageFailsClosedOnUntrustedTransformOrRegion(t *testing.T) {
	data, _ := problemSourceOrientationFixture(t)
	base := ReadyPageAsset{
		Metadata: k12storage.PageAssetMetadata{
			OrientationPolicy:        k12storage.PageAssetOrientationVerified,
			OrientationPolicyVersion: pageAssetOrientationPolicyVersion,
			TransformChainJSON:       `[]`, PixelWidth: 3, PixelHeight: 2,
		},
		Data: data,
	}
	tests := []struct {
		name   string
		mutate func(*ReadyPageAsset)
		region *k12.SourcePixelRegion
	}{
		{
			name: "unverified orientation",
			mutate: func(ready *ReadyPageAsset) {
				ready.Metadata.OrientationPolicy = k12storage.PageAssetOrientationUnverified
			},
		},
		{
			name: "unknown policy version",
			mutate: func(ready *ReadyPageAsset) {
				ready.Metadata.OrientationPolicyVersion = "future-v2"
			},
		},
		{
			name: "unknown transform",
			mutate: func(ready *ReadyPageAsset) {
				ready.Metadata.TransformChainJSON = `[{"operation":"rotate_guess","orientation":6}]`
			},
		},
		{
			name: "dimension drift",
			mutate: func(ready *ReadyPageAsset) {
				ready.Metadata.PixelWidth++
			},
		},
		{
			name:   "region outside normalized pixels",
			mutate: func(*ReadyPageAsset) {},
			region: &k12.SourcePixelRegion{X: 2, Y: 0, Width: 2, Height: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready := base
			tt.mutate(&ready)
			if _, err := normalizeProblemSourceOCRImage(ready, tt.region); err == nil {
				t.Fatal("untrusted source image facts were accepted")
			}
		})
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
