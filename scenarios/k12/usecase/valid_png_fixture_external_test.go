package usecase_test

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/png"
	"testing"
)

// validPNGFixture mirrors the internal-package fixture helper for black-box
// tests. Test packages cannot share unexported helpers, and production code
// must not expose test-only image generation.
func validPNGFixture(t *testing.T, label string) []byte {
	t.Helper()
	digest := sha256.Sum256([]byte(label))
	source := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for pixel := 0; pixel < 4; pixel++ {
		offset := pixel * 4
		source.Pix[offset] = digest[pixel*3]
		source.Pix[offset+1] = digest[pixel*3+1]
		source.Pix[offset+2] = digest[pixel*3+2]
		source.Pix[offset+3] = 0xff
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatalf("encode valid PNG fixture %q: %v", label, err)
	}
	return encoded.Bytes()
}
