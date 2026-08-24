package usecase

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var ErrProblemSourceImageInvalid = errors.New(
	"problem source image transform is invalid",
)

type problemSourceOCRImage struct {
	Data   []byte
	Width  int
	Height int
}

type problemSourceImageTransform struct {
	Operation   string `json:"operation"`
	Orientation int    `json:"orientation"`
}

// canonicalProblemSourceImage 为所有模型和批注阶段生成同一份无 EXIF PNG；
// 原始 PageAsset bytes 与内容摘要保持不变。
func canonicalProblemSourceImage(
	ready ReadyPageAsset,
) (problemSourceOCRImage, error) {
	return normalizeProblemSourceOCRImage(ready, nil)
}

// normalizeProblemSourceOCRImage converts immutable PageAsset bytes into the
// verified orientation-normalized source-pixel coordinate system. OCR always
// receives a metadata-free PNG so a provider cannot apply EXIF a second time.
// select_region crops only after normalization; retake passes nil and receives
// the whole normalized page.
func normalizeProblemSourceOCRImage(
	ready ReadyPageAsset,
	region *k12.SourcePixelRegion,
) (problemSourceOCRImage, error) {
	metadata := ready.Metadata
	if metadata.OrientationPolicy != k12storage.PageAssetOrientationVerified ||
		strings.TrimSpace(metadata.OrientationPolicyVersion) !=
			pageAssetOrientationPolicyVersion {
		return problemSourceOCRImage{}, fmt.Errorf(
			"%w: PageAsset orientation policy is not the supported verified policy",
			ErrProblemSourceImageInvalid,
		)
	}
	if len(ready.Data) == 0 || metadata.PixelWidth <= 0 ||
		metadata.PixelHeight <= 0 ||
		int64(metadata.PixelWidth)*int64(metadata.PixelHeight) >
			maxProblemSourceActionPixels {
		return problemSourceOCRImage{}, fmt.Errorf(
			"%w: PageAsset bytes or normalized dimensions are invalid",
			ErrProblemSourceImageInvalid,
		)
	}
	orientation, err := problemSourceEXIFOrientation(
		metadata.TransformChainJSON,
	)
	if err != nil {
		return problemSourceOCRImage{}, err
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(ready.Data))
	if err != nil || config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > maxProblemSourceActionPixels {
		return problemSourceOCRImage{}, fmt.Errorf(
			"%w: PageAsset bytes are not a bounded decodable image: %v",
			ErrProblemSourceImageInvalid,
			err,
		)
	}
	decoded, _, err := image.Decode(bytes.NewReader(ready.Data))
	if err != nil {
		return problemSourceOCRImage{}, fmt.Errorf(
			"%w: decode PageAsset pixels: %v",
			ErrProblemSourceImageInvalid,
			err,
		)
	}
	oriented, err := applyProblemSourceEXIFOrientation(decoded, orientation)
	if err != nil {
		return problemSourceOCRImage{}, err
	}
	if oriented.Bounds() != image.Rect(
		0, 0, metadata.PixelWidth, metadata.PixelHeight,
	) {
		return problemSourceOCRImage{}, fmt.Errorf(
			"%w: normalized pixels are %dx%d but durable metadata requires %dx%d",
			ErrProblemSourceImageInvalid,
			oriented.Bounds().Dx(),
			oriented.Bounds().Dy(),
			metadata.PixelWidth,
			metadata.PixelHeight,
		)
	}

	selected := image.Image(oriented)
	width, height := metadata.PixelWidth, metadata.PixelHeight
	if region != nil {
		candidate := problemSourceActionRegion{
			X: region.X, Y: region.Y,
			Width: region.Width, Height: region.Height,
		}
		if err := validateProblemSourceRegion(candidate, image.Config{
			Width: metadata.PixelWidth, Height: metadata.PixelHeight,
		}); err != nil {
			return problemSourceOCRImage{}, fmt.Errorf(
				"%w: %v", ErrProblemSourceImageInvalid, err,
			)
		}
		cropped := image.NewNRGBA(image.Rect(0, 0, region.Width, region.Height))
		draw.Draw(
			cropped,
			cropped.Bounds(),
			oriented,
			image.Pt(region.X, region.Y),
			draw.Src,
		)
		selected = cropped
		width, height = region.Width, region.Height
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, selected); err != nil {
		return problemSourceOCRImage{}, fmt.Errorf(
			"%w: encode normalized OCR input: %v",
			ErrProblemSourceImageInvalid,
			err,
		)
	}
	return problemSourceOCRImage{
		Data: encoded.Bytes(), Width: width, Height: height,
	}, nil
}

func problemSourceEXIFOrientation(transformChainJSON string) (int, error) {
	raw := strings.TrimSpace(transformChainJSON)
	if raw == "" {
		return 0, fmt.Errorf(
			"%w: PageAsset transform chain is empty",
			ErrProblemSourceImageInvalid,
		)
	}
	var operations []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &operations); err != nil {
		return 0, fmt.Errorf(
			"%w: decode PageAsset transform chain: %v",
			ErrProblemSourceImageInvalid,
			err,
		)
	}
	if len(operations) == 0 {
		return 1, nil
	}
	if len(operations) != 1 {
		return 0, fmt.Errorf(
			"%w: unsupported PageAsset transform count %d",
			ErrProblemSourceImageInvalid,
			len(operations),
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(operations[0]))
	decoder.DisallowUnknownFields()
	var operation problemSourceImageTransform
	if err := decoder.Decode(&operation); err != nil {
		return 0, fmt.Errorf(
			"%w: decode PageAsset transform: %v",
			ErrProblemSourceImageInvalid,
			err,
		)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return 0, fmt.Errorf(
			"%w: PageAsset transform contains trailing data",
			ErrProblemSourceImageInvalid,
		)
	}
	if operation.Operation != "exif_orientation" ||
		operation.Orientation < 2 || operation.Orientation > 8 {
		return 0, fmt.Errorf(
			"%w: unsupported PageAsset transform %q orientation %d",
			ErrProblemSourceImageInvalid,
			operation.Operation,
			operation.Orientation,
		)
	}
	return operation.Orientation, nil
}

func applyProblemSourceEXIFOrientation(
	source image.Image,
	orientation int,
) (*image.NRGBA, error) {
	if source == nil || orientation < 1 || orientation > 8 {
		return nil, fmt.Errorf(
			"%w: EXIF orientation must be in 1..8",
			ErrProblemSourceImageInvalid,
		)
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 ||
		int64(width)*int64(height) > maxProblemSourceActionPixels {
		return nil, fmt.Errorf(
			"%w: decoded image dimensions are invalid",
			ErrProblemSourceImageInvalid,
		)
	}
	destinationWidth, destinationHeight := width, height
	if orientation >= 5 {
		destinationWidth, destinationHeight = height, width
	}
	destination := image.NewNRGBA(
		image.Rect(0, 0, destinationWidth, destinationHeight),
	)
	for y := 0; y < destinationHeight; y++ {
		for x := 0; x < destinationWidth; x++ {
			sourceX, sourceY := problemSourceEXIFSourcePixel(
				orientation, width, height, x, y,
			)
			destination.Set(
				x,
				y,
				source.At(bounds.Min.X+sourceX, bounds.Min.Y+sourceY),
			)
		}
	}
	return destination, nil
}

func problemSourceEXIFSourcePixel(
	orientation, width, height, x, y int,
) (int, int) {
	switch orientation {
	case 1:
		return x, y
	case 2:
		return width - 1 - x, y
	case 3:
		return width - 1 - x, height - 1 - y
	case 4:
		return x, height - 1 - y
	case 5:
		return y, x
	case 6:
		return y, height - 1 - x
	case 7:
		return width - 1 - y, height - 1 - x
	case 8:
		return width - 1 - y, x
	default:
		panic("validated EXIF orientation outside 1..8")
	}
}
