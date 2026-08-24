package engineadapter

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func TestPhotoAnnotatorDoesNotApplyEXIFOrientationAtTheRenderingBoundary(t *testing.T) {
	raw := photoAnnotatorEXIF6Fixture(t, 120, 80)
	rendered, err := NewPhotoAnnotator().Annotate(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("annotate EXIF fixture: %v", err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(rendered.Data))
	if err != nil {
		t.Fatalf("decode annotated image: %v", err)
	}
	if config.Width != 120 || config.Height != 80 {
		t.Fatalf("annotator applied a second orientation transform: got=%dx%d want=120x80",
			config.Width, config.Height)
	}
}

func photoAnnotatorEXIF6Fixture(t *testing.T, width, height int) []byte {
	t.Helper()
	source := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source.SetRGBA(x, y, color.RGBA{
				R: uint8(20 + x*170/max(1, width-1)),
				G: uint8(30 + y*160/max(1, height-1)),
				B: 0x80,
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
