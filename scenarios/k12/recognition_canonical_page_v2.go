package k12

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"

	_ "golang.org/x/image/webp"
)

// CanonicalRecognitionPageV2 是所有 V2 识别边界对同一解码页面使用的唯一字节标识。
type CanonicalRecognitionPageV2 struct {
	PNG    []byte
	Digest string
}

// CanonicalizeRecognitionPageV2 将任何受支持且可解码的图像规范化为零原点 RGBA，
// 并生成 png.Encode 输出的确定性字节。
func CanonicalizeRecognitionPageV2(raw []byte) (CanonicalRecognitionPageV2, error) {
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return CanonicalRecognitionPageV2{}, fmt.Errorf(
			"%w: decode source image: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	bounds := decoded.Bounds()
	if bounds.Empty() {
		return CanonicalRecognitionPageV2{}, fmt.Errorf(
			"%w: source image has empty bounds",
			ErrRecognitionLayoutPlanInvalid,
		)
	}

	normalized := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(normalized, normalized.Bounds(), decoded, bounds.Min, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, normalized); err != nil {
		return CanonicalRecognitionPageV2{}, fmt.Errorf(
			"%w: encode canonical source PNG: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	pngBytes := encoded.Bytes()
	return CanonicalRecognitionPageV2{
		PNG:    pngBytes,
		Digest: recognitionLayoutSHA256(pngBytes),
	}, nil
}
