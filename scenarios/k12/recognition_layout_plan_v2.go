package k12

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sort"
	"strconv"
	"strings"
)

const (
	// RecognitionPlanVersionV1 标识历史全页识别计划。
	// 它有意独立于模型请求策略版本。
	RecognitionPlanVersionV1 = 1
	// RecognitionPlanVersionV2 标识密集页清单与受限布局批次计划。
	RecognitionPlanVersionV2 = 2

	RecognitionLayoutBatchTargetLimitV2 = 4
	recognitionLayoutTargetLimitV2      = 32
	recognitionLayoutUnitOrdinalLimitV2 = 9999
	recognitionLayoutContactPaddingV2   = 8
	recognitionLayoutContactGapV2       = 8
	recognitionLayoutContactHeightV2    = 640
)

var ErrRecognitionLayoutPlanInvalid = errors.New("recognition layout plan invalid")

// RecognitionLayoutManifestSuccessV2 是成功紧凑清单物理调用的持久标识。
// 此纯计划器有意不接收结果文本；结果的持久摘要是权威边界。
type RecognitionLayoutManifestSuccessV2 struct {
	InvocationID string `json:"invocation_id"`
	ResultDigest string `json:"result_digest"`
}

// RecognitionLayoutManifestTargetV2 是从模型接收的受限语义输出。
// ManifestRef 仅为响应内引用，校验后即丢弃；持久 TargetID 由服务端派生。
type RecognitionLayoutManifestTargetV2 struct {
	ManifestRef      string            `json:"manifest_ref"`
	ManifestOrder    int               `json:"manifest_order"`
	SourceNumberPath []string          `json:"source_number_path"`
	DisplayLabel     string            `json:"display_label"`
	Region           SourcePixelRegion `json:"region"`
}

type RecognitionLayoutPlanInputV2 struct {
	PagePNG  []byte
	Manifest RecognitionLayoutManifestSuccessV2
	Targets  []RecognitionLayoutManifestTargetV2
}

// RecognitionLayoutTargetV2 仅包含本地派生的持久事实。
// 源裁剪字节或模型选择的持久标识均不会进入计划。
type RecognitionLayoutTargetV2 struct {
	TargetID         string            `json:"target_id"`
	SourceNumberPath []string          `json:"source_number_path"`
	DisplayLabel     string            `json:"display_label"`
	Region           SourcePixelRegion `json:"region"`
	CropDigest       string            `json:"crop_digest"`
}

type RecognitionLayoutBatchV2 struct {
	Unit        RecognitionPhysicalUnit `json:"unit"`
	TargetIDs   []string                `json:"target_ids"`
	InputDigest string                  `json:"input_digest"`
}

// RecognitionLayoutPlanV2 可安全持久化：它包含标识、源像素区域和摘要，
// 但绝不包含源图像或裁剪字节。
type RecognitionLayoutPlanV2 struct {
	Version              int                         `json:"version"`
	PageDigest           string                      `json:"page_digest"`
	ManifestInvocationID string                      `json:"manifest_invocation_id"`
	ManifestResultDigest string                      `json:"manifest_result_digest"`
	Targets              []RecognitionLayoutTargetV2 `json:"targets"`
	Batches              []RecognitionLayoutBatchV2  `json:"batches"`
	AuthorizedPlanDigest string                      `json:"authorized_plan_digest"`
}

// BuildRecognitionLayoutPlanV2 校验成功的紧凑清单并派生一个不可变精确集合计划。
// 所有顺序和持久标识均在本地生成，因此模型输出顺序和模型提供的标识都不会成为
// 存储标识。
func BuildRecognitionLayoutPlanV2(input RecognitionLayoutPlanInputV2) (RecognitionLayoutPlanV2, error) {
	if err := validateRecognitionLayoutManifestSuccessV2(input.Manifest); err != nil {
		return RecognitionLayoutPlanV2{}, err
	}
	if len(input.Targets) < 1 || len(input.Targets) > recognitionLayoutTargetLimitV2 {
		return RecognitionLayoutPlanV2{}, fmt.Errorf(
			"%w: manifest target count must be 1..%d",
			ErrRecognitionLayoutPlanInvalid,
			recognitionLayoutTargetLimitV2,
		)
	}

	page, err := png.Decode(bytes.NewReader(input.PagePNG))
	if err != nil {
		return RecognitionLayoutPlanV2{}, fmt.Errorf(
			"%w: source is not a decodable PNG: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	pageBounds := page.Bounds()
	if pageBounds.Min.X != 0 || pageBounds.Min.Y != 0 || pageBounds.Empty() {
		return RecognitionLayoutPlanV2{}, fmt.Errorf(
			"%w: source PNG has non-canonical bounds",
			ErrRecognitionLayoutPlanInvalid,
		)
	}

	targets := append([]RecognitionLayoutManifestTargetV2(nil), input.Targets...)
	if targetErr := validateRecognitionLayoutManifestTargetsV2(targets, pageBounds); targetErr != nil {
		return RecognitionLayoutPlanV2{}, targetErr
	}
	sort.SliceStable(targets, func(left, right int) bool {
		if targets[left].Region.Y != targets[right].Region.Y {
			return targets[left].Region.Y < targets[right].Region.Y
		}
		if targets[left].Region.X != targets[right].Region.X {
			return targets[left].Region.X < targets[right].Region.X
		}
		return targets[left].ManifestOrder < targets[right].ManifestOrder
	})

	pageDigest := recognitionLayoutSHA256(input.PagePNG)
	plan := RecognitionLayoutPlanV2{
		Version:              RecognitionPlanVersionV2,
		PageDigest:           pageDigest,
		ManifestInvocationID: input.Manifest.InvocationID,
		ManifestResultDigest: input.Manifest.ResultDigest,
		Targets:              make([]RecognitionLayoutTargetV2, 0, len(targets)),
	}
	for index, target := range targets {
		crop, cropErr := recognitionLayoutCropPNG(page, target.Region)
		if cropErr != nil {
			return RecognitionLayoutPlanV2{}, cropErr
		}
		plan.Targets = append(plan.Targets, RecognitionLayoutTargetV2{
			TargetID: recognitionLayoutTargetIDV2(
				pageDigest,
				index+1,
				target.Region,
			),
			SourceNumberPath: append([]string{}, target.SourceNumberPath...),
			DisplayLabel:     target.DisplayLabel,
			Region:           target.Region,
			CropDigest:       recognitionLayoutSHA256(crop),
		})
	}

	for start, ordinal := 0, 1; start < len(plan.Targets); ordinal++ {
		end := start
		height := 2 * recognitionLayoutContactPaddingV2
		for end < len(plan.Targets) && end-start < RecognitionLayoutBatchTargetLimitV2 {
			nextHeight := height + plan.Targets[end].Region.Height
			if end > start {
				nextHeight += recognitionLayoutContactGapV2
			}
			if end > start && nextHeight > recognitionLayoutContactHeightV2 {
				break
			}
			height = nextHeight
			end++
		}
		unit, unitErr := RecognitionLayoutBatchUnitV2(ordinal)
		if unitErr != nil {
			return RecognitionLayoutPlanV2{}, unitErr
		}
		targetIDs := make([]string, 0, end-start)
		for _, target := range plan.Targets[start:end] {
			targetIDs = append(targetIDs, target.TargetID)
		}
		batchImage, imageErr := recognitionLayoutContactSheetPNG(
			page,
			plan.Targets[start:end],
		)
		if imageErr != nil {
			return RecognitionLayoutPlanV2{}, imageErr
		}
		plan.Batches = append(plan.Batches, RecognitionLayoutBatchV2{
			Unit:        unit,
			TargetIDs:   targetIDs,
			InputDigest: recognitionLayoutSHA256(batchImage),
		})
		start = end
	}

	digest, err := recognitionLayoutAuthorizedPlanDigestV2(plan)
	if err != nil {
		return RecognitionLayoutPlanV2{}, err
	}
	plan.AuthorizedPlanDigest = digest
	return plan, nil
}

// BuildRecognitionLayoutBatchImageV2 根据源 PNG 和已授权计划重建临时 Provider 输入。
// 它校验每个持久化摘要，且绝不要求在计划中存储图像字节。
func BuildRecognitionLayoutBatchImageV2(
	pagePNG []byte,
	plan RecognitionLayoutPlanV2,
	unit RecognitionPhysicalUnit,
) ([]byte, error) {
	if plan.Version != RecognitionPlanVersionV2 ||
		recognitionLayoutSHA256(pagePNG) != plan.PageDigest {
		return nil, fmt.Errorf(
			"%w: source page does not match authorized plan",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	wantPlanDigest, err := recognitionLayoutAuthorizedPlanDigestV2(plan)
	if err != nil || wantPlanDigest != plan.AuthorizedPlanDigest {
		return nil, fmt.Errorf(
			"%w: authorized plan digest mismatch",
			ErrRecognitionLayoutPlanInvalid,
		)
	}

	var selected *RecognitionLayoutBatchV2
	for index := range plan.Batches {
		if plan.Batches[index].Unit == unit {
			selected = &plan.Batches[index]
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf(
			"%w: batch unit %q is not authorized",
			ErrRecognitionLayoutPlanInvalid,
			unit,
		)
	}
	if len(selected.TargetIDs) < 1 || len(selected.TargetIDs) > RecognitionLayoutBatchTargetLimitV2 {
		return nil, fmt.Errorf(
			"%w: batch target count is invalid",
			ErrRecognitionLayoutPlanInvalid,
		)
	}

	targetByID := make(map[string]RecognitionLayoutTargetV2, len(plan.Targets))
	for _, target := range plan.Targets {
		if target.TargetID == "" {
			return nil, fmt.Errorf(
				"%w: empty target identity",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		if _, duplicate := targetByID[target.TargetID]; duplicate {
			return nil, fmt.Errorf(
				"%w: duplicate target identity",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		targetByID[target.TargetID] = target
	}
	batchTargets := make([]RecognitionLayoutTargetV2, 0, len(selected.TargetIDs))
	seen := make(map[string]struct{}, len(selected.TargetIDs))
	for _, targetID := range selected.TargetIDs {
		if _, duplicate := seen[targetID]; duplicate {
			return nil, fmt.Errorf(
				"%w: duplicate target in batch",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		seen[targetID] = struct{}{}
		target, exists := targetByID[targetID]
		if !exists {
			return nil, fmt.Errorf(
				"%w: batch target is not authorized",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		batchTargets = append(batchTargets, target)
	}

	page, err := png.Decode(bytes.NewReader(pagePNG))
	if err != nil {
		return nil, fmt.Errorf(
			"%w: source is not a decodable PNG: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	for _, target := range batchTargets {
		if regionErr := validateRecognitionLayoutRegionV2(target.Region, page.Bounds()); regionErr != nil {
			return nil, regionErr
		}
		crop, cropErr := recognitionLayoutCropPNG(page, target.Region)
		if cropErr != nil || recognitionLayoutSHA256(crop) != target.CropDigest {
			return nil, fmt.Errorf(
				"%w: candidate crop digest mismatch",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
	}
	result, err := recognitionLayoutContactSheetPNG(page, batchTargets)
	if err != nil {
		return nil, err
	}
	if recognitionLayoutSHA256(result) != selected.InputDigest {
		return nil, fmt.Errorf(
			"%w: batch input digest mismatch",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	return result, nil
}

// BuildRecognitionLayoutRepairImageV2 重建获准用于单项修复的唯一 Provider 图像。
// 它将规范页面字节绑定到持久化计划，并在返回任何临时图像字节前重新校验本地派生的
// 裁剪摘要。
func BuildRecognitionLayoutRepairImageV2(
	pagePNG []byte,
	plan RecognitionLayoutPlanV2,
	candidateID string,
) ([]byte, error) {
	if recognitionLayoutSHA256(pagePNG) != plan.PageDigest ||
		ValidateRecognitionLayoutPlanV2(plan) != nil {
		return nil, fmt.Errorf(
			"%w: source page or authorized plan is invalid",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	var selected *RecognitionLayoutTargetV2
	for index := range plan.Targets {
		if plan.Targets[index].TargetID != candidateID {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf(
				"%w: repair candidate identity is duplicated",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		selected = &plan.Targets[index]
	}
	if selected == nil {
		return nil, fmt.Errorf(
			"%w: repair candidate is not authorized",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	page, err := png.Decode(bytes.NewReader(pagePNG))
	if err != nil || page.Bounds().Min.X != 0 || page.Bounds().Min.Y != 0 ||
		page.Bounds().Empty() {
		return nil, fmt.Errorf(
			"%w: source is not the canonical page PNG",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	if regionErr := validateRecognitionLayoutRegionV2(selected.Region, page.Bounds()); regionErr != nil {
		return nil, regionErr
	}
	crop, err := recognitionLayoutCropPNG(page, selected.Region)
	if err != nil || recognitionLayoutSHA256(crop) != selected.CropDigest {
		return nil, fmt.Errorf(
			"%w: repair candidate crop digest mismatch",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	return crop, nil
}

func RecognitionLayoutBatchUnitV2(ordinal int) (RecognitionPhysicalUnit, error) {
	return recognitionLayoutPhysicalUnitV2("layout_batch_", ordinal)
}

func RecognitionLayoutRepairUnitV2(ordinal int) (RecognitionPhysicalUnit, error) {
	return recognitionLayoutPhysicalUnitV2("layout_repair_", ordinal)
}

func recognitionLayoutPhysicalUnitV2(prefix string, ordinal int) (RecognitionPhysicalUnit, error) {
	if ordinal < 1 || ordinal > recognitionLayoutUnitOrdinalLimitV2 {
		return "", fmt.Errorf(
			"%w: physical unit ordinal must be 1..%d",
			ErrRecognitionLayoutPlanInvalid,
			recognitionLayoutUnitOrdinalLimitV2,
		)
	}
	unit := RecognitionPhysicalUnit(fmt.Sprintf("%s%04d", prefix, ordinal))
	if !unit.Valid() {
		return "", fmt.Errorf(
			"%w: non-canonical physical unit",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	return unit, nil
}

func validateRecognitionLayoutManifestSuccessV2(success RecognitionLayoutManifestSuccessV2) error {
	if success.InvocationID == "" || strings.TrimSpace(success.InvocationID) != success.InvocationID {
		return fmt.Errorf(
			"%w: manifest invocation identity is empty or non-canonical",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	if !validRecognitionLayoutSHA256(success.ResultDigest) {
		return fmt.Errorf(
			"%w: manifest result digest is non-canonical",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	return nil
}

func validateRecognitionLayoutManifestTargetsV2(
	targets []RecognitionLayoutManifestTargetV2,
	pageBounds image.Rectangle,
) error {
	refs := make(map[string]struct{}, len(targets))
	orders := make(map[int]struct{}, len(targets))
	for _, target := range targets {
		refOrder, ok := recognitionLayoutManifestRefOrderV2(target.ManifestRef)
		if !ok || refOrder > len(targets) {
			return fmt.Errorf(
				"%w: manifest reference is outside the canonical exact-set",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		if target.ManifestOrder < 1 || target.ManifestOrder > len(targets) {
			return fmt.Errorf(
				"%w: manifest order is outside the exact-set",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		if refOrder != target.ManifestOrder {
			return fmt.Errorf(
				"%w: manifest reference and order disagree",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		if _, duplicate := refs[target.ManifestRef]; duplicate {
			return fmt.Errorf(
				"%w: duplicate manifest reference",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		if _, duplicate := orders[target.ManifestOrder]; duplicate {
			return fmt.Errorf(
				"%w: duplicate manifest order",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
		refs[target.ManifestRef] = struct{}{}
		orders[target.ManifestOrder] = struct{}{}
		if err := validateRecognitionLayoutSourceNumberV2(
			target.SourceNumberPath,
			target.DisplayLabel,
		); err != nil {
			return err
		}
		if err := validateRecognitionLayoutRegionV2(target.Region, pageBounds); err != nil {
			return err
		}
	}
	return nil
}

func recognitionLayoutManifestRefOrderV2(ref string) (int, bool) {
	const prefix = "manifest_"
	if len(ref) != len(prefix)+4 || !strings.HasPrefix(ref, prefix) {
		return 0, false
	}
	ordinal, err := strconv.Atoi(ref[len(prefix):])
	return ordinal, err == nil && ordinal > 0
}

func validateRecognitionLayoutRegionV2(region SourcePixelRegion, bounds image.Rectangle) error {
	if region.X < 0 || region.Y < 0 || region.Width <= 0 || region.Height <= 0 ||
		region.X > bounds.Dx() || region.Y > bounds.Dy() ||
		region.Width > bounds.Dx()-region.X ||
		region.Height > bounds.Dy()-region.Y {
		return fmt.Errorf(
			"%w: target source-pixel region is outside the page",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	return nil
}

func validateRecognitionLayoutSourceNumberV2(path []string, label string) error {
	labelPresent := label != ""
	if labelPresent && strings.TrimSpace(label) != label {
		return fmt.Errorf(
			"%w: source display label is non-canonical",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	if (len(path) > 0) != labelPresent {
		return fmt.Errorf(
			"%w: source number path and display label must be paired",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	for _, token := range path {
		if token == "" || strings.TrimSpace(token) != token {
			return fmt.Errorf(
				"%w: source number path contains an empty or non-canonical token",
				ErrRecognitionLayoutPlanInvalid,
			)
		}
	}
	return nil
}

func recognitionLayoutTargetIDV2(
	pageDigest string,
	ordinal int,
	region SourcePixelRegion,
) string {
	identity := struct {
		Contract   string            `json:"contract"`
		PageDigest string            `json:"page_digest"`
		Ordinal    int               `json:"ordinal"`
		Region     SourcePixelRegion `json:"region"`
	}{
		Contract:   "recognition_layout_target_v2",
		PageDigest: pageDigest,
		Ordinal:    ordinal,
		Region:     region,
	}
	encoded, _ := json.Marshal(identity)
	digest := sha256.Sum256(encoded)
	return "layout_target_v2_" + hex.EncodeToString(digest[:])
}

func recognitionLayoutCropPNG(source image.Image, region SourcePixelRegion) ([]byte, error) {
	crop := image.NewRGBA(image.Rect(0, 0, region.Width, region.Height))
	draw.Draw(
		crop,
		crop.Bounds(),
		source,
		image.Pt(region.X, region.Y),
		draw.Src,
	)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, crop); err != nil {
		return nil, fmt.Errorf(
			"%w: encode candidate crop: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	return encoded.Bytes(), nil
}

func recognitionLayoutContactSheetPNG(
	source image.Image,
	targets []RecognitionLayoutTargetV2,
) ([]byte, error) {
	if len(targets) < 1 || len(targets) > RecognitionLayoutBatchTargetLimitV2 {
		return nil, fmt.Errorf(
			"%w: contact sheet target count must be 1..%d",
			ErrRecognitionLayoutPlanInvalid,
			RecognitionLayoutBatchTargetLimitV2,
		)
	}
	maxWidth, contentHeight := 0, 0
	for _, target := range targets {
		if target.Region.Width > maxWidth {
			maxWidth = target.Region.Width
		}
		contentHeight += target.Region.Height
	}
	width := maxWidth + 2*recognitionLayoutContactPaddingV2
	height := contentHeight + 2*recognitionLayoutContactPaddingV2 + recognitionLayoutContactGapV2*(len(targets)-1)
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf(
			"%w: contact sheet dimensions overflowed",
			ErrRecognitionLayoutPlanInvalid,
		)
	}
	sheet := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(sheet, sheet.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	top := recognitionLayoutContactPaddingV2
	for _, target := range targets {
		destination := image.Rect(
			recognitionLayoutContactPaddingV2,
			top,
			recognitionLayoutContactPaddingV2+target.Region.Width,
			top+target.Region.Height,
		)
		draw.Draw(
			sheet,
			destination,
			source,
			image.Pt(target.Region.X, target.Region.Y),
			draw.Src,
		)
		top += target.Region.Height + recognitionLayoutContactGapV2
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, sheet); err != nil {
		return nil, fmt.Errorf(
			"%w: encode batch contact sheet: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	return encoded.Bytes(), nil
}

func recognitionLayoutAuthorizedPlanDigestV2(plan RecognitionLayoutPlanV2) (string, error) {
	canonical := struct {
		Contract             string                      `json:"contract"`
		Version              int                         `json:"version"`
		PageDigest           string                      `json:"page_digest"`
		ManifestInvocationID string                      `json:"manifest_invocation_id"`
		ManifestResultDigest string                      `json:"manifest_result_digest"`
		Targets              []RecognitionLayoutTargetV2 `json:"targets"`
		Batches              []RecognitionLayoutBatchV2  `json:"batches"`
	}{
		Contract:             "recognition_layout_authorized_plan_v2",
		Version:              plan.Version,
		PageDigest:           plan.PageDigest,
		ManifestInvocationID: plan.ManifestInvocationID,
		ManifestResultDigest: plan.ManifestResultDigest,
		Targets:              plan.Targets,
		Batches:              plan.Batches,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf(
			"%w: encode authorized plan: %v",
			ErrRecognitionLayoutPlanInvalid,
			err,
		)
	}
	return recognitionLayoutSHA256(encoded), nil
}

func recognitionLayoutSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validRecognitionLayoutSHA256(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 ||
		!strings.HasPrefix(value, "sha256:") {
		return false
	}
	hexadecimal := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(hexadecimal) != hexadecimal {
		return false
	}
	_, err := hex.DecodeString(hexadecimal)
	return err == nil
}
