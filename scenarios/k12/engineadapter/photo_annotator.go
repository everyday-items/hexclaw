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

// PhotoAnnotator 使用标准库在原图像素上画半透明框和矢量勾/叉，不依赖字体或图床。
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
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	for _, mark := range marks {
		if err := ctx.Err(); err != nil {
			return usecase.RenderedPhoto{}, err
		}
		drawPhotoMark(dst, mark)
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

func drawPhotoMark(dst *image.RGBA, mark usecase.PhotoAnnotation) {
	b := mark.BBox
	if !validPhotoBBox(b) {
		return
	}
	w, h := dst.Bounds().Dx(), dst.Bounds().Dy()
	x0, y0 := int(math.Round(b.X*float64(w))), int(math.Round(b.Y*float64(h)))
	x1, y1 := int(math.Round((b.X+b.W)*float64(w))), int(math.Round((b.Y+b.H)*float64(h)))
	if x1 <= x0 || y1 <= y0 {
		return
	}
	stroke := maxInt(3, minInt(12, minInt(w, h)/240))
	base := color.RGBA{R: 239, G: 68, B: 68, A: 255}
	fill := color.RGBA{R: 239, G: 68, B: 68, A: 38}
	if mark.Correct {
		base = color.RGBA{R: 22, G: 163, B: 74, A: 255}
		fill = color.RGBA{R: 22, G: 163, B: 74, A: 34}
	}
	rect := image.Rect(x0, y0, x1, y1).Intersect(dst.Bounds())
	draw.Draw(dst, rect, &image.Uniform{C: fill}, image.Point{}, draw.Over)
	drawRectBorder(dst, rect, base, stroke)

	radius := minInt(30, maxInt(12, (y1-y0)/2))
	cx := minInt(w-radius-1, maxInt(radius+1, x1-radius/3))
	cy := maxInt(radius+1, y0+radius/3)
	if cy+radius >= h {
		cy = h - radius - 1
	}
	drawFilledCircle(dst, cx, cy, radius, base)
	white := color.RGBA{255, 255, 255, 255}
	if mark.Correct {
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

func drawRectBorder(dst *image.RGBA, r image.Rectangle, c color.RGBA, n int) {
	for i := 0; i < n; i++ {
		draw.Draw(dst, image.Rect(r.Min.X, r.Min.Y+i, r.Max.X, r.Min.Y+i+1).Intersect(dst.Bounds()), &image.Uniform{C: c}, image.Point{}, draw.Src)
		draw.Draw(dst, image.Rect(r.Min.X, r.Max.Y-i-1, r.Max.X, r.Max.Y-i).Intersect(dst.Bounds()), &image.Uniform{C: c}, image.Point{}, draw.Src)
		draw.Draw(dst, image.Rect(r.Min.X+i, r.Min.Y, r.Min.X+i+1, r.Max.Y).Intersect(dst.Bounds()), &image.Uniform{C: c}, image.Point{}, draw.Src)
		draw.Draw(dst, image.Rect(r.Max.X-i-1, r.Min.Y, r.Max.X-i, r.Max.Y).Intersect(dst.Bounds()), &image.Uniform{C: c}, image.Point{}, draw.Src)
	}
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
