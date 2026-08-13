package k12_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestREGK12RecognitionManifest20260808001SharedCanonicalPageIsByteStable(t *testing.T) {
	source := image.NewRGBA(image.Rect(7, 11, 71, 59))
	for y := source.Bounds().Min.Y; y < source.Bounds().Max.Y; y++ {
		for x := source.Bounds().Min.X; x < source.Bounds().Max.X; x++ {
			source.SetRGBA(x, y, color.RGBA{
				R: uint8((x*7 + y*3) % 251),
				G: uint8((x*5 + y*11) % 253),
				B: uint8((x*13 + y*2) % 247),
				A: 255,
			})
		}
	}

	var jpegContainer bytes.Buffer
	if err := jpeg.Encode(&jpegContainer, source, &jpeg.Options{Quality: 97}); err != nil {
		t.Fatalf("encode JPEG fixture: %v", err)
	}
	jpegPixels, err := jpeg.Decode(bytes.NewReader(jpegContainer.Bytes()))
	if err != nil {
		t.Fatalf("decode JPEG fixture: %v", err)
	}
	pngContainer := func(level png.CompressionLevel) []byte {
		t.Helper()
		var encoded bytes.Buffer
		if encodeErr := (&png.Encoder{CompressionLevel: level}).Encode(&encoded, jpegPixels); encodeErr != nil {
			t.Fatalf("encode PNG fixture: %v", encodeErr)
		}
		return encoded.Bytes()
	}
	uncompressedPNG := pngContainer(png.NoCompression)
	compressedPNG := pngContainer(png.BestCompression)
	if bytes.Equal(uncompressedPNG, compressedPNG) {
		t.Fatal("fixture must have distinct PNG container bytes")
	}

	inputs := [][]byte{jpegContainer.Bytes(), uncompressedPNG, compressedPNG}
	canonical := make([]k12.CanonicalRecognitionPageV2, len(inputs))
	for index, input := range inputs {
		canonical[index], err = k12.CanonicalizeRecognitionPageV2(input)
		if err != nil {
			t.Fatalf("canonicalize equivalent input %d: %v", index, err)
		}
	}
	for index := 1; index < len(canonical); index++ {
		if !bytes.Equal(canonical[0].PNG, canonical[index].PNG) {
			t.Fatalf("equivalent decoded pixels produced different canonical bytes for input %d", index)
		}
		if canonical[0].Digest != canonical[index].Digest {
			t.Fatalf("equivalent decoded pixels produced different digest for input %d", index)
		}
	}

	wantRGBA := image.NewRGBA(image.Rect(0, 0, jpegPixels.Bounds().Dx(), jpegPixels.Bounds().Dy()))
	draw.Draw(wantRGBA, wantRGBA.Bounds(), jpegPixels, jpegPixels.Bounds().Min, draw.Src)
	var wantPNG bytes.Buffer
	if err := png.Encode(&wantPNG, wantRGBA); err != nil {
		t.Fatalf("encode expected canonical PNG: %v", err)
	}
	if !bytes.Equal(canonical[0].PNG, wantPNG.Bytes()) {
		t.Fatal("canonical bytes are not zero-origin RGBA encoded with the fixed png.Encode contract")
	}
	wantSum := sha256.Sum256(canonical[0].PNG)
	wantDigest := "sha256:" + hex.EncodeToString(wantSum[:])
	if canonical[0].Digest != wantDigest {
		t.Fatalf("canonical digest=%q want=%q", canonical[0].Digest, wantDigest)
	}
}

func TestREGK12RecognitionManifest20260808001SharedCanonicalPageRejectsInvalidImage(t *testing.T) {
	for _, input := range [][]byte{nil, []byte("not-an-image")} {
		got, err := k12.CanonicalizeRecognitionPageV2(input)
		if err == nil {
			t.Fatal("invalid source image was accepted")
		}
		if len(got.PNG) != 0 || got.Digest != "" {
			t.Fatalf("invalid source leaked canonical output: %+v", got)
		}
	}
}
