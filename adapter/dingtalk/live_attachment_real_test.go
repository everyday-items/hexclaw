package dingtalk

// 钉钉「批改图片出站」真机门（env-gated，默认 skip）。
//
// 该测试只验证出站闭环：本地批改图字节 → 钉钉 media/upload → mediaId →
// sampleMarkdown 图片引用 → BatchSendOTO。图片批改/合成本身由 K12 测试覆盖；
// 两段分开取证，能在失败时区分「生成失败」和「钉钉上传/发送失败」。
//
// 运行（会真实发送到钉钉）：
//
// 	DINGTALK_LIVE_SEND=1 \
// 	DINGTALK_LIVE_GRADED_IMAGE=/absolute/path/to/graded-homework.png \
// 	go test ./adapter/dingtalk -run TestLiveCorrectedPhotoAttachment_SendToDingtalk -v -count=1 -timeout 2m
//
// 必须显式设置 DINGTALK_LIVE_CONFIRM、DINGTALK_LIVE_INSTANCE 和
// DINGTALK_LIVE_USERID，防止真机门误发到最近会话。凭证只在进程内存中解密，不落盘。

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type liveCaptureDingtalkOpenAPI struct {
	dingtalkOpenAPI
	dingtalkMediaOpenAPI

	mu              sync.Mutex
	processQueryKey string
}

func (c *liveCaptureDingtalkOpenAPI) SendOTO(
	ctx context.Context,
	accessToken string,
	robotCode string,
	userID string,
	msg dingtalkOutboundMessage,
) (string, error) {
	key, err := c.dingtalkOpenAPI.SendOTO(ctx, accessToken, robotCode, userID, msg)
	if err == nil {
		c.mu.Lock()
		c.processQueryKey = strings.TrimSpace(key)
		c.mu.Unlock()
	}
	return key, err
}

func (c *liveCaptureDingtalkOpenAPI) ProcessQueryKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.processQueryKey
}

func TestLiveCorrectedPhotoAttachment_SendToDingtalk(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") != "1" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机（会真实上传批改图并发送到你的钉钉）")
	}
	imagePath := strings.TrimSpace(os.Getenv("DINGTALK_LIVE_GRADED_IMAGE"))
	if imagePath == "" {
		t.Skip("设 DINGTALK_LIVE_GRADED_IMAGE=<批改后图片绝对路径> 跑真实图片附件出站")
	}

	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("读取批改图片失败: %v", err)
	}
	if len(imageBytes) == 0 {
		t.Fatal("批改图片为空")
	}
	mime := http.DetectContentType(imageBytes)
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		t.Fatalf("DINGTALK_LIVE_GRADED_IMAGE 不是图片: MIME=%q", mime)
	}
	originalPath := strings.TrimSpace(os.Getenv("DINGTALK_LIVE_ORIGINAL_IMAGE"))
	if originalPath == "" {
		t.Fatal("真批改图语义门必须设置 DINGTALK_LIVE_ORIGINAL_IMAGE=<批改前原图>，禁止只凭附件非空判定批改成功")
	}
	originalBytes, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatalf("读取批改前原图失败: %v", err)
	}
	if err := validateCorrectedPhotoArtifact(originalBytes, imageBytes); err != nil {
		t.Fatal(err)
	}

	cfg, userID := loadLiveDingtalkConfig(t)
	adp := New(cfg)
	realAPI, err := adp.apiClient()
	if err != nil {
		t.Fatalf("初始化钉钉官方 SDK 失败: %v", err)
	}
	mediaAPI, ok := realAPI.(dingtalkMediaOpenAPI)
	if !ok {
		t.Fatal("钉钉官方 SDK 不支持图片上传")
	}
	captureAPI := &liveCaptureDingtalkOpenAPI{
		dingtalkOpenAPI:      realAPI,
		dingtalkMediaOpenAPI: mediaAPI,
	}
	adp.mu.Lock()
	adp.openAPI = captureAPI
	adp.mu.Unlock()

	trace := "TRACE-DINGTALK-GRADED-IMAGE-" + time.Now().Format("20060102-150405")
	content := "## 作业批改图片实发验证\n\n" +
		"下面应显示一张批改后的作业图片。\n\n" + trace
	if markdownPath := strings.TrimSpace(os.Getenv("DINGTALK_LIVE_MARKDOWN_FILE")); markdownPath != "" {
		markdownBytes, readErr := os.ReadFile(markdownPath)
		if readErr != nil {
			t.Fatalf("读取批改 Markdown 失败: %v", readErr)
		}
		content = strings.TrimSpace(string(markdownBytes))
		if content == "" {
			t.Fatal("批改 Markdown 为空")
		}
		content += "\n\n---\n\n" + trace
	}
	reply := &adapter.Reply{
		Content: content,
		Attachments: []adapter.Attachment{{
			Type: "image",
			Name: filepath.Base(imagePath),
			Mime: mime,
			Data: base64.StdEncoding.EncodeToString(imageBytes),
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := adp.Send(ctx, userID, reply); err != nil {
		t.Fatalf("批改图片上传并发送到钉钉失败: %v", err)
	}
	if captureAPI.ProcessQueryKey() == "" {
		t.Fatal("钉钉 BatchSendOTO 未返回 processQueryKey，不能把仅 HTTP 成功当成消息已受理")
	}
	t.Logf("✅ 已向 userId=%s 上传并发送批改图 %s（%d 字节，%s），BatchSendOTO 已返回消息标识；请在钉钉确认图片可见，追踪码=%s",
		userID, imagePath, len(imageBytes), mime, trace)
}

func validateCorrectedPhotoArtifact(original, corrected []byte) error {
	if len(original) == 0 {
		return fmt.Errorf("批改前原图为空")
	}
	if len(corrected) == 0 {
		return fmt.Errorf("批改图为空")
	}
	source, _, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		return fmt.Errorf("批改前原图无法解码: %w", err)
	}
	graded, _, err := image.Decode(bytes.NewReader(corrected))
	if err != nil {
		return fmt.Errorf("批改图无法解码: %w", err)
	}
	sourceBounds, gradedBounds := source.Bounds(), graded.Bounds()
	if sourceBounds.Dx() <= 0 || sourceBounds.Dy() <= 0 || gradedBounds.Dx() <= 0 || gradedBounds.Dy() <= 0 {
		return fmt.Errorf("批改前后图片尺寸无效")
	}
	sourceRatio := float64(sourceBounds.Dx()) / float64(sourceBounds.Dy())
	gradedRatio := float64(gradedBounds.Dx()) / float64(gradedBounds.Dy())
	if ratioDelta := absFloat(sourceRatio-gradedRatio) / sourceRatio; ratioDelta > 0.01 {
		return fmt.Errorf("批改图宽高比发生变化: before=%dx%d after=%dx%d",
			sourceBounds.Dx(), sourceBounds.Dy(), gradedBounds.Dx(), gradedBounds.Dy())
	}

	semanticPixels := countCorrectionStrokePixels(source, graded)
	totalPixels := gradedBounds.Dx() * gradedBounds.Dy()
	minimumStrokePixels := maxInt(24, totalPixels/200_000)
	if semanticPixels < minimumStrokePixels {
		return fmt.Errorf("批改图没有可验证的新增绿色勾/红色叉笔画: semantic_pixels=%d want>=%d",
			semanticPixels, minimumStrokePixels)
	}
	if semanticPixels > totalPixels/10 {
		return fmt.Errorf("批改色覆盖异常过大，疑似整图染色而非局部勾叉: semantic_pixels=%d total=%d",
			semanticPixels, totalPixels)
	}
	return nil
}

// countCorrectionStrokePixels 比较“解码后的图像语义”，而非比较编码字节。这样同一原图即使
// 被 PNG/JPEG 重新编码也不会冒充批改结果；只有原图中不存在、批改图中新增的绿/红高饱和
// 笔画像素才计入。批改图因媒体上限等比例缩小时，按归一化坐标回采原图。
func countCorrectionStrokePixels(source, graded image.Image) int {
	sourceBounds, gradedBounds := source.Bounds(), graded.Bounds()
	count := 0
	for y := 0; y < gradedBounds.Dy(); y++ {
		sourceY := sourceBounds.Min.Y + y*sourceBounds.Dy()/gradedBounds.Dy()
		for x := 0; x < gradedBounds.Dx(); x++ {
			gradedColor := color.NRGBAModel.Convert(graded.At(gradedBounds.Min.X+x, gradedBounds.Min.Y+y)).(color.NRGBA)
			gradedKind := correctionStrokeKind(gradedColor)
			if gradedKind == 0 {
				continue
			}
			sourceX := sourceBounds.Min.X + x*sourceBounds.Dx()/gradedBounds.Dx()
			sourceColor := color.NRGBAModel.Convert(source.At(sourceX, sourceY)).(color.NRGBA)
			if correctionStrokeKind(sourceColor) != gradedKind || colorDistance(sourceColor, gradedColor) > 45 {
				count++
			}
		}
	}
	return count
}

func correctionStrokeKind(c color.NRGBA) int {
	if c.A < 180 {
		return 0
	}
	r, g, b := int(c.R), int(c.G), int(c.B)
	if g >= 105 && g >= r+30 && g >= b+25 {
		return 1
	}
	if r >= 145 && r >= g+38 && r >= b+38 {
		return 2
	}
	return 0
}

func colorDistance(a, b color.NRGBA) int {
	return absInt(int(a.R)-int(b.R)) + absInt(int(a.G)-int(b.G)) + absInt(int(a.B)-int(b.B))
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestValidateCorrectedPhotoArtifact_RejectsUnchangedOriginal(t *testing.T) {
	original := correctionArtifactFixture(t, false, "png")
	if err := validateCorrectedPhotoArtifact(original, append([]byte(nil), original...)); err == nil {
		t.Fatal("unchanged original image must not pass the corrected-photo semantic gate")
	}
}

func TestValidateCorrectedPhotoArtifact_RejectsReencodingAndUnrelatedPixelChanges(t *testing.T) {
	original := correctionArtifactFixture(t, false, "png")
	reencoded := correctionArtifactFixture(t, false, "jpeg")
	if err := validateCorrectedPhotoArtifact(original, reencoded); err == nil {
		t.Fatal("mere PNG/JPEG re-encoding must not pass as a corrected worksheet")
	}

	unrelated := image.NewRGBA(image.Rect(0, 0, 120, 120))
	draw.Draw(unrelated, unrelated.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(unrelated, image.Rect(20, 20, 100, 100), image.NewUniform(color.RGBA{40, 80, 220, 255}), image.Point{}, draw.Src)
	var changed bytes.Buffer
	if err := png.Encode(&changed, unrelated); err != nil {
		t.Fatal(err)
	}
	if err := validateCorrectedPhotoArtifact(original, changed.Bytes()); err == nil {
		t.Fatal("arbitrary changed pixels without green/red grading strokes must not pass")
	}
}

func TestValidateCorrectedPhotoArtifact_AcceptsNewCompactGradingStroke(t *testing.T) {
	original := correctionArtifactFixture(t, false, "png")
	corrected := correctionArtifactFixture(t, true, "png")
	if err := validateCorrectedPhotoArtifact(original, corrected); err != nil {
		t.Fatalf("a decoded image with a new compact grading stroke should pass: %v", err)
	}
}

func correctionArtifactFixture(t *testing.T, annotated bool, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 120, 120))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(12, 58, 108, 60), image.NewUniform(color.RGBA{40, 40, 40, 255}), image.Point{}, draw.Src)
	if annotated {
		green := color.RGBA{22, 163, 74, 255}
		draw.Draw(img, image.Rect(70, 38, 76, 72), image.NewUniform(green), image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(70, 66, 104, 72), image.NewUniform(green), image.Point{}, draw.Src)
	}
	var out bytes.Buffer
	switch format {
	case "png":
		if err := png.Encode(&out, img); err != nil {
			t.Fatal(err)
		}
	case "jpeg":
		if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 82}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported fixture format %q", format)
	}
	return out.Bytes()
}
