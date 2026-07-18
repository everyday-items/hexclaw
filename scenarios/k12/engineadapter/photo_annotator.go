package engineadapter

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const (
	maxPhotoPixels        int64 = 30_000_000
	maxRenderedPhotoBytes       = 18 << 20 // leave headroom under DingTalk's 20MB media limit
)

// PhotoAnnotator 使用标准库在原图答案旁画矢量勾/叉。没有可靠坐标的结论只保留在
// Markdown 明细中：既不猜测作答位置，也不改变原图尺寸追加题号栏。
type PhotoAnnotator struct{}

func NewPhotoAnnotator() *PhotoAnnotator { return &PhotoAnnotator{} }

var _ usecase.PhotoAnnotator = (*PhotoAnnotator)(nil)

func (*PhotoAnnotator) Annotate(ctx context.Context, raw []byte, marks []usecase.PhotoAnnotation) (usecase.RenderedPhoto, error) {
	if err := ctx.Err(); err != nil {
		return usecase.RenderedPhoto{}, err
	}
	if len(raw) == 0 {
		return usecase.RenderedPhoto{}, fmt.Errorf("photo annotator: 空图片")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return usecase.RenderedPhoto{}, fmt.Errorf("photo annotator: 读取图片尺寸: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPhotoPixels {
		return usecase.RenderedPhoto{}, fmt.Errorf("photo annotator: 图片像素 %dx%d 超出 %d 上限", cfg.Width, cfg.Height, maxPhotoPixels)
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return usecase.RenderedPhoto{}, fmt.Errorf("photo annotator: 解码图片: %w", err)
	}
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, image.Rect(0, 0, bounds.Dx(), bounds.Dy()), src, bounds.Min, draw.Src)
	for _, placement := range layoutPhotoMarks(dst.Bounds(), marks) {
		if err := ctx.Err(); err != nil {
			return usecase.RenderedPhoto{}, err
		}
		drawVerdictGlyph(
			dst,
			placement.cx,
			placement.cy,
			placement.radius,
			placement.correct,
			placement.stroke,
			placement.base,
		)
	}
	rendered, err := encodeAnnotatedPhoto(dst, maxRenderedPhotoBytes)
	if err != nil {
		return usecase.RenderedPhoto{}, fmt.Errorf("photo annotator: 编码批改图: %w", err)
	}
	return rendered, nil
}

// encodeAnnotatedPhoto prefers lossless PNG for worksheets. Noisy high-resolution
// phone photos can expand dramatically when converted from JPEG to PNG, so it
// falls back to a bounded JPEG and, only when necessary, progressively scales
// down the image. This keeps the final media below DingTalk's upload ceiling.
func encodeAnnotatedPhoto(src image.Image, maxBytes int) (usecase.RenderedPhoto, error) {
	if src == nil || maxBytes <= 0 {
		return usecase.RenderedPhoto{}, fmt.Errorf("图片或输出上限无效")
	}
	var pngOut bytes.Buffer
	if err := png.Encode(&pngOut, src); err != nil {
		return usecase.RenderedPhoto{}, fmt.Errorf("编码 PNG: %w", err)
	}
	if pngOut.Len() <= maxBytes {
		return usecase.RenderedPhoto{Data: pngOut.Bytes(), MIME: "image/png"}, nil
	}

	for _, quality := range []int{90, 82, 74, 66, 58} {
		data, err := encodePhotoJPEG(src, quality)
		if err != nil {
			return usecase.RenderedPhoto{}, err
		}
		if len(data) <= maxBytes {
			return usecase.RenderedPhoto{Data: data, MIME: "image/jpeg"}, nil
		}
	}

	current := copyPhotoRGBA(src)
	for current.Bounds().Dx() > 320 || current.Bounds().Dy() > 320 {
		newWidth := maxInt(1, current.Bounds().Dx()*4/5)
		newHeight := maxInt(1, current.Bounds().Dy()*4/5)
		current = resizePhotoNearest(current, newWidth, newHeight)
		data, err := encodePhotoJPEG(current, 78)
		if err != nil {
			return usecase.RenderedPhoto{}, err
		}
		if len(data) <= maxBytes {
			return usecase.RenderedPhoto{Data: data, MIME: "image/jpeg"}, nil
		}
	}
	data, err := encodePhotoJPEG(current, 50)
	if err != nil {
		return usecase.RenderedPhoto{}, err
	}
	if len(data) > maxBytes {
		return usecase.RenderedPhoto{}, fmt.Errorf("批改图压缩后仍有 %d 字节，超过 %d 字节上限", len(data), maxBytes)
	}
	return usecase.RenderedPhoto{Data: data, MIME: "image/jpeg"}, nil
}

func encodePhotoJPEG(src image.Image, quality int) ([]byte, error) {
	var out bytes.Buffer
	if err := jpeg.Encode(&out, src, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("编码 JPEG: %w", err)
	}
	return out.Bytes(), nil
}

func copyPhotoRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

func resizePhotoNearest(src *image.RGBA, width, height int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	srcWidth, srcHeight := src.Bounds().Dx(), src.Bounds().Dy()
	for y := 0; y < height; y++ {
		sy := minInt(srcHeight-1, y*srcHeight/height)
		for x := 0; x < width; x++ {
			sx := minInt(srcWidth-1, x*srcWidth/width)
			dst.SetRGBA(x, y, src.RGBAAt(sx, sy))
		}
	}
	return dst
}

type photoMarkPlacement struct {
	cx      int
	cy      int
	radius  int
	stroke  int
	correct bool
	base    color.RGBA
	bounds  image.Rectangle
}

func layoutPhotoMarks(bounds image.Rectangle, marks []usecase.PhotoAnnotation) []photoMarkPlacement {
	placements := make([]photoMarkPlacement, 0, len(marks))
	occupied := make([]image.Rectangle, 0, len(marks))
	for _, mark := range marks {
		base, ok := basePhotoMarkPlacement(bounds, mark)
		if !ok {
			continue
		}
		candidates := photoMarkPlacementCandidates(bounds, base)
		selected := candidates[0]
		bestOverlap := photoMarkOverlapArea(selected.bounds, occupied)
		for _, candidate := range candidates {
			overlap := photoMarkOverlapArea(candidate.bounds, occupied)
			if overlap == 0 {
				selected = candidate
				bestOverlap = 0
				break
			}
			if overlap < bestOverlap {
				selected = candidate
				bestOverlap = overlap
			}
		}
		placements = append(placements, selected)
		occupied = append(occupied, selected.bounds)
	}
	return placements
}

func basePhotoMarkPlacement(bounds image.Rectangle, mark usecase.PhotoAnnotation) (photoMarkPlacement, bool) {
	b := mark.BBox
	if !validPhotoBBox(b) {
		return photoMarkPlacement{}, false
	}
	w, h := bounds.Dx(), bounds.Dy()
	x0, y0 := int(math.Round(b.X*float64(w))), int(math.Round(b.Y*float64(h)))
	x1, y1 := int(math.Round((b.X+b.W)*float64(w))), int(math.Round((b.Y+b.H)*float64(h)))
	if x1 <= x0 || y1 <= y0 {
		return photoMarkPlacement{}, false
	}
	stroke := maxInt(3, minInt(12, minInt(w, h)/240))
	base := color.RGBA{R: 239, G: 68, B: 68, A: 255}
	if mark.Correct {
		base = color.RGBA{R: 22, G: 163, B: 74, A: 255}
	}

	// 独立锚定阶段返回的是通过本地几何与墨迹门禁的紧作答框，不再是带题干上下文的粗定位块。
	// 因此勾叉直接放在答案框右缘、纵向上部 40% 的书写带附近；只画紧凑笔画，
	// 不以大色块或矩形覆盖孩子原笔迹。靠近页边时再向内夹紧。
	radius := minInt(42, maxInt(18, minInt(w, h)/45))
	cx := x1
	cx = minInt(w-radius-1, maxInt(radius+1, cx))
	cy := y0 + (y1-y0)*2/5
	cy = maxInt(radius+1, cy)
	if cy+radius >= h {
		cy = h - radius - 1
	}
	placement := photoMarkPlacement{
		cx: cx, cy: cy, radius: radius, stroke: stroke,
		correct: mark.Correct, base: base,
	}
	placement.bounds = verdictGlyphBounds(placement)
	return placement, true
}

func photoMarkPlacementCandidates(bounds image.Rectangle, base photoMarkPlacement) []photoMarkPlacement {
	horizontal := base.bounds.Dx() + 6
	vertical := base.bounds.Dy() + 6
	offsets := []image.Point{
		{},
		{X: horizontal},
		{X: horizontal, Y: vertical},
		{X: horizontal, Y: -vertical},
		{Y: vertical},
		{Y: -vertical},
		{X: -horizontal},
		{X: -horizontal, Y: vertical},
		{X: -horizontal, Y: -vertical},
		{X: 2 * horizontal},
		{X: -2 * horizontal},
	}
	candidates := make([]photoMarkPlacement, 0, len(offsets))
	seen := make(map[image.Point]struct{}, len(offsets))
	for _, offset := range offsets {
		candidate := base
		candidate.cx += offset.X
		candidate.cy += offset.Y
		candidate.bounds = verdictGlyphBounds(candidate)
		candidate = clampPhotoMarkPlacement(bounds, candidate)
		center := image.Pt(candidate.cx, candidate.cy)
		if _, duplicate := seen[center]; duplicate {
			continue
		}
		seen[center] = struct{}{}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func clampPhotoMarkPlacement(bounds image.Rectangle, placement photoMarkPlacement) photoMarkPlacement {
	rect := placement.bounds
	dx, dy := 0, 0
	if rect.Min.X < bounds.Min.X {
		dx = bounds.Min.X - rect.Min.X
	} else if rect.Max.X > bounds.Max.X {
		dx = bounds.Max.X - rect.Max.X
	}
	if rect.Min.Y < bounds.Min.Y {
		dy = bounds.Min.Y - rect.Min.Y
	} else if rect.Max.Y > bounds.Max.Y {
		dy = bounds.Max.Y - rect.Max.Y
	}
	placement.cx += dx
	placement.cy += dy
	placement.bounds = verdictGlyphBounds(placement)
	return placement
}

func verdictGlyphBounds(placement photoMarkPlacement) image.Rectangle {
	pad := (placement.stroke+4)/2 + 2
	radius := placement.radius
	if placement.correct {
		return image.Rect(
			placement.cx-radius*3/4-pad,
			placement.cy-radius*2/3-pad,
			placement.cx+radius+pad+1,
			placement.cy+radius/2+pad+1,
		)
	}
	return image.Rect(
		placement.cx-radius*2/3-pad,
		placement.cy-radius*2/3-pad,
		placement.cx+radius*2/3+pad+1,
		placement.cy+radius*2/3+pad+1,
	)
}

func photoMarkOverlapArea(candidate image.Rectangle, occupied []image.Rectangle) int {
	area := 0
	for _, used := range occupied {
		intersection := candidate.Intersect(used)
		if !intersection.Empty() {
			area += intersection.Dx() * intersection.Dy()
		}
	}
	return area
}

func drawVerdictGlyph(dst *image.RGBA, cx, cy, radius int, correct bool, stroke int, base color.RGBA) {
	// 对标常见作业批注：直接画绿色 ✓ / 红色 ✕，不套圆形徽章。白色底描只沿着
	// 笔画本身走一遍，保证压在印刷线或铅笔字上仍清楚，但不盖住周围答案。
	underlay := color.RGBA{255, 255, 255, 255}
	draw := func(x0, y0, x1, y1 int) {
		drawThickLine(dst, x0, y0, x1, y1, underlay, stroke+4)
		drawThickLine(dst, x0, y0, x1, y1, base, stroke)
	}
	if correct {
		draw(cx-radius*3/4, cy, cx-radius/4, cy+radius/2)
		draw(cx-radius/4, cy+radius/2, cx+radius, cy-radius*2/3)
	} else {
		draw(cx-radius*2/3, cy-radius*2/3, cx+radius*2/3, cy+radius*2/3)
		draw(cx+radius*2/3, cy-radius*2/3, cx-radius*2/3, cy+radius*2/3)
	}
}

func validPhotoBBox(b usecase.BBox) bool {
	values := []float64{b.X, b.Y, b.W, b.H}
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return b.X >= 0 && b.Y >= 0 && b.W > 0 && b.H > 0 && b.X+b.W <= 1.005 && b.Y+b.H <= 1.005
}

func drawThickLine(dst *image.RGBA, x0, y0, x1, y1 int, c color.RGBA, thick int) {
	dx, dy := x1-x0, y1-y0
	steps := maxInt(absInt(dx), absInt(dy))
	if steps == 0 {
		steps = 1
	}
	for i := 0; i <= steps; i++ {
		x := x0 + dx*i/steps
		y := y0 + dy*i/steps
		r := maxInt(1, thick/2)
		for oy := -r; oy <= r; oy++ {
			for ox := -r; ox <= r; ox++ {
				if ox*ox+oy*oy <= r*r && image.Pt(x+ox, y+oy).In(dst.Bounds()) {
					dst.SetRGBA(x+ox, y+oy, c)
				}
			}
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
