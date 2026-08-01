package k12

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

const (
	DenseWorksheetSegmentCount           = 5
	DenseWorksheetSemanticBlockFraction  = 0.12
	DenseWorksheetSegmentOverlapFraction = 0.14
)

// DenseWorksheetPhysicalInput is one deterministic image input from the
// approved whole-page protocol fallback. Keeping this constructor in the K12
// domain lets both the Provider adapter and durable reconciliation rebuild the
// exact same bytes without a reverse dependency on engineadapter.
type DenseWorksheetPhysicalInput struct {
	Unit  RecognitionPhysicalUnit
	Image []byte
}

func DenseWorksheetRanges() [DenseWorksheetSegmentCount][2]float64 {
	var ranges [DenseWorksheetSegmentCount][2]float64
	span := (1 +
		float64(DenseWorksheetSegmentCount-1)*
			DenseWorksheetSegmentOverlapFraction) /
		float64(DenseWorksheetSegmentCount)
	step := span - DenseWorksheetSegmentOverlapFraction
	for index := range ranges {
		start := float64(index) * step
		end := start + span
		if index == len(ranges)-1 {
			end = 1
		}
		ranges[index] = [2]float64{start, end}
	}
	return ranges
}

// DenseWorksheetFallbackPhysicalInputs returns the exact five PNG segments
// followed by the original whole-page bytes used for printed inventory.
func DenseWorksheetFallbackPhysicalInputs(
	raw []byte,
) ([]DenseWorksheetPhysicalInput, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	legacyTall := cfg.Height >= 1600 && cfg.Height*5 >= cfg.Width*6
	lowResolutionWorksheet :=
		cfg.Height >= 1200 &&
			cfg.Width >= 800 &&
			cfg.Height*3 >= cfg.Width*4
	if !legacyTall && !lowResolutionWorksheet {
		return nil, false
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	bounds := src.Bounds()
	ranges := DenseWorksheetRanges()
	inputs := make(
		[]DenseWorksheetPhysicalInput,
		0,
		DenseWorksheetSegmentCount+1,
	)
	for index, pageRange := range ranges {
		y0 := bounds.Min.Y +
			int(math.Floor(float64(bounds.Dy())*pageRange[0]))
		y1 := bounds.Min.Y +
			int(math.Ceil(float64(bounds.Dy())*pageRange[1]))
		if y1 > bounds.Max.Y {
			y1 = bounds.Max.Y
		}
		if y1 <= y0 {
			return nil, false
		}
		dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), y1-y0))
		draw.Draw(
			dst,
			dst.Bounds(),
			&image.Uniform{C: color.White},
			image.Point{},
			draw.Src,
		)
		draw.Draw(
			dst,
			dst.Bounds(),
			src,
			image.Pt(bounds.Min.X, y0),
			draw.Over,
		)
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, dst); err != nil {
			return nil, false
		}
		unit, ok := RecognitionPhysicalSegmentUnit(index + 1)
		if !ok {
			return nil, false
		}
		inputs = append(inputs, DenseWorksheetPhysicalInput{
			Unit:  unit,
			Image: encoded.Bytes(),
		})
	}
	inputs = append(inputs, DenseWorksheetPhysicalInput{
		Unit:  RecognitionPhysicalUnitPrintedInventory,
		Image: append([]byte(nil), raw...),
	})
	return inputs, true
}

func ValidateDenseWorksheetFallbackPhysicalInputs(
	inputs []DenseWorksheetPhysicalInput,
) error {
	if len(inputs) != DenseWorksheetSegmentCount+1 {
		return fmt.Errorf(
			"dense worksheet physical inputs=%d, want %d",
			len(inputs),
			DenseWorksheetSegmentCount+1,
		)
	}
	for index := range DenseWorksheetSegmentCount {
		want, _ := RecognitionPhysicalSegmentUnit(index + 1)
		if inputs[index].Unit != want || len(inputs[index].Image) == 0 {
			return fmt.Errorf(
				"dense worksheet physical input %d is invalid",
				index,
			)
		}
	}
	last := inputs[len(inputs)-1]
	if last.Unit != RecognitionPhysicalUnitPrintedInventory ||
		len(last.Image) == 0 {
		return fmt.Errorf("dense worksheet printed inventory input is invalid")
	}
	return nil
}
