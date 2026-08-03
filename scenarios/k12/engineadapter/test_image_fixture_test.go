package engineadapter

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/png"
	"testing"
)

// validPNGFixture returns deterministic, fully decodable PNG bytes for archive
// fixtures. Labels preserve the prior tests' distinct-content semantics without
// treating a PNG signature plus arbitrary text as a valid uploaded image.
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
