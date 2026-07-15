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
	"strconv"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const (
	maxPhotoPixels        int64 = 30_000_000
	maxRenderedPhotoBytes       = 18 << 20 // leave headroom under DingTalk's 20MB media limit
)

// PhotoAnnotator 使用标准库在原图上画紧凑的矢量勾/叉；没有可靠坐标的已验证结论
// 放在独立的题号结果栏中，不依赖字体、图床，也不猜测作答位置。
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
	unpositioned := make([]usecase.PhotoAnnotation, 0, len(marks))
	for _, mark := range marks {
		if !validPhotoBBox(mark.BBox) && mark.QuestionNumber > 0 {
			unpositioned = append(unpositioned, mark)
		}
	}
	railWidth := 0
	if len(unpositioned) > 0 {
		railWidth = minInt(360, maxInt(144, bounds.Dx()/5))
	}
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx()+railWidth, bounds.Dy()))
	draw.Draw(dst, image.Rect(0, 0, bounds.Dx(), bounds.Dy()), src, bounds.Min, draw.Src)
	for _, mark := range marks {
		if err := ctx.Err(); err != nil {
			return usecase.RenderedPhoto{}, err
		}
		drawPhotoMark(dst, bounds.Dx(), mark)
	}
	if railWidth > 0 {
		drawPhotoStatusRail(dst, bounds.Dx(), unpositioned)
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

func drawPhotoMark(dst *image.RGBA, worksheetWidth int, mark usecase.PhotoAnnotation) {
	b := mark.BBox
	if !validPhotoBBox(b) {
		return
	}
	w, h := worksheetWidth, dst.Bounds().Dy()
	x0, y0 := int(math.Round(b.X*float64(w))), int(math.Round(b.Y*float64(h)))
	x1, y1 := int(math.Round((b.X+b.W)*float64(w))), int(math.Round((b.Y+b.H)*float64(h)))
	if x1 <= x0 || y1 <= y0 {
		return
	}
	stroke := maxInt(3, minInt(12, minInt(w, h)/240))
	base := color.RGBA{R: 239, G: 68, B: 68, A: 255}
	if mark.Correct {
		base = color.RGBA{R: 22, G: 163, B: 74, A: 255}
	}

	// 主流作业批注只放紧凑 ✓/✗，不以大色块和矩形覆盖孩子原笔迹。语义核验框
	// 可能为覆盖完整演算而较宽，因此使用框左侧的稳定标记轨道，避免漂到相邻题或页边。
	radius := minInt(28, maxInt(12, minInt(w, h)/72))
	margin := maxInt(3, radius/4)
	cx := x0 - radius - margin
	if cx-radius < 0 {
		cx = x0 + radius + margin
	}
	cx = minInt(w-radius-1, maxInt(radius+1, cx))
	cy := y0 + (y1-y0)*3/4
	cy = maxInt(radius+1, cy)
	if cy+radius >= h {
		cy = h - radius - 1
	}
	drawVerdictGlyph(dst, cx, cy, radius, mark.Correct, stroke, base)
}

func drawPhotoStatusRail(dst *image.RGBA, worksheetWidth int, marks []usecase.PhotoAnnotation) {
	rail := image.Rect(worksheetWidth, 0, dst.Bounds().Dx(), dst.Bounds().Dy())
	draw.Draw(dst, rail, &image.Uniform{C: color.RGBA{R: 248, G: 250, B: 252, A: 255}}, image.Point{}, draw.Src)
	divider := maxInt(2, minInt(6, dst.Bounds().Dy()/500))
	draw.Draw(dst, image.Rect(worksheetWidth, 0, worksheetWidth+divider, dst.Bounds().Dy()),
		&image.Uniform{C: color.RGBA{R: 148, G: 163, B: 184, A: 255}}, image.Point{}, draw.Src)
	if len(marks) == 0 {
		return
	}
	railWidth, height := rail.Dx(), rail.Dy()
	rowHeight := minInt(88, maxInt(24, height/(len(marks)+1)))
	contentHeight := rowHeight * len(marks)
	centerY := maxInt(rowHeight/2+4, (height-contentHeight)/2+rowHeight/2)
	for i, mark := range marks {
		cy := centerY + i*rowHeight
		if cy >= height {
			cy = height - maxInt(4, rowHeight/2)
		}
		digitScale := maxInt(1, minInt(6, rowHeight/10))
		drawPhotoNumber(dst, mark.QuestionNumber, worksheetWidth+maxInt(12, railWidth/12), cy, digitScale,
			color.RGBA{R: 30, G: 41, B: 59, A: 255})
		radius := minInt(25, maxInt(7, rowHeight/3))
		cx := worksheetWidth + railWidth*3/4
		base := color.RGBA{R: 239, G: 68, B: 68, A: 255}
		if mark.Correct {
			base = color.RGBA{R: 22, G: 163, B: 74, A: 255}
		}
		drawVerdictGlyph(dst, cx, cy, radius, mark.Correct, maxInt(2, radius/4), base)
	}
}

var photoDigitPixels = [10][5]string{
	{"111", "101", "101", "101", "111"},
	{"010", "110", "010", "010", "111"},
	{"111", "001", "111", "100", "111"},
	{"111", "001", "111", "001", "111"},
	{"101", "101", "111", "001", "001"},
	{"111", "100", "111", "001", "111"},
	{"111", "100", "111", "101", "111"},
	{"111", "001", "010", "010", "010"},
	{"111", "101", "111", "101", "111"},
	{"111", "101", "111", "001", "111"},
}

func drawPhotoNumber(dst *image.RGBA, number, x, centerY, scale int, ink color.RGBA) {
	text := strconv.Itoa(maxInt(1, number))
	digitWidth, gap := 3*scale, scale
	y0 := centerY - 5*scale/2
	for i, char := range text {
		digit := int(char - '0')
		if digit < 0 || digit > 9 {
			continue
		}
		x0 := x + i*(digitWidth+gap)
		for row, pixels := range photoDigitPixels[digit] {
			for column, pixel := range pixels {
				if pixel != '1' {
					continue
				}
				rect := image.Rect(x0+column*scale, y0+row*scale, x0+(column+1)*scale, y0+(row+1)*scale).Intersect(dst.Bounds())
				draw.Draw(dst, rect, &image.Uniform{C: ink}, image.Point{}, draw.Src)
			}
		}
	}
}

func drawVerdictGlyph(dst *image.RGBA, cx, cy, radius int, correct bool, stroke int, base color.RGBA) {
	// 白色细外圈让符号在印刷线/手写墨迹上仍清楚，但不改变答案区域底色。
	drawFilledCircle(dst, cx, cy, radius+maxInt(2, stroke/2), color.RGBA{255, 255, 255, 255})
	drawFilledCircle(dst, cx, cy, radius, base)
	white := color.RGBA{255, 255, 255, 255}
	if correct {
		drawThickLine(dst, cx-radius/2, cy, cx-radius/8, cy+radius/3, white, maxInt(3, stroke))
		drawThickLine(dst, cx-radius/8, cy+radius/3, cx+radius/2, cy-radius/3, white, maxInt(3, stroke))
	} else {
		drawThickLine(dst, cx-radius/3, cy-radius/3, cx+radius/3, cy+radius/3, white, maxInt(3, stroke))
		drawThickLine(dst, cx+radius/3, cy-radius/3, cx-radius/3, cy+radius/3, white, maxInt(3, stroke))
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

func drawFilledCircle(dst *image.RGBA, cx, cy, radius int, c color.RGBA) {
	for y := -radius; y <= radius; y++ {
		span := int(math.Sqrt(float64(radius*radius - y*y)))
		for x := -span; x <= span; x++ {
			px, py := cx+x, cy+y
			if image.Pt(px, py).In(dst.Bounds()) {
				dst.SetRGBA(px, py, c)
			}
		}
	}
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
