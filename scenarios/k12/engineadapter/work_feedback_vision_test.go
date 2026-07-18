package engineadapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// encodeTestArtPNG 生成一张最小真实 PNG（魔数校验必须过——载体约定要求真图片，不允许占位字节）。
func encodeTestArtPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: 200, B: uint8(y * 30), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

// TestGenerateWorkFeedback_Art_UsesVisionChannel 美术带图（本地文件路径载体）→ 走多模态
// 视觉路径：原图 bytes 原样送达视觉闭包，提示词是观察式点评且红线随任务下发；
// 纯文本生成闭包绝不能被触发。
func TestGenerateWorkFeedback_Art_UsesVisionChannel(t *testing.T) {
	raw := encodeTestArtPNG(t)
	path := filepath.Join(t.TempDir(), "art.png")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var gotImage []byte
	var gotPrompt string
	textCalls := 0
	a := NewSolveAdapter(nil,
		WithWorkFeedbackGen(func(ctx context.Context, subject, prompt, grade string) (string, error) {
			textCalls++
			return "不该走这里", nil
		}),
		WithWorkFeedbackVision(func(ctx context.Context, image []byte, prompt string) (string, error) {
			gotImage = append([]byte(nil), image...)
			gotPrompt = prompt
			return "画面左侧的大树用了三种绿色，层次很清楚；建议把地面的影子也画出来，太阳的方向会更明确。", nil
		}),
	)

	out, err := a.GenerateWorkFeedback(context.Background(), usecase.WorkFeedbackRequest{
		WorkType:      k12.WorkTypeArt,
		Title:         "《雨后的校园》",
		Task:          "画一处雨后场景",
		Intent:        "想画出雨后安静的感觉",
		SourceAssetID: path,
		Grade:         "三年级上",
	})
	if err != nil {
		t.Fatalf("美术带图点评应走视觉通道成功: %v", err)
	}
	if textCalls != 0 {
		t.Fatalf("美术点评不得走纯文本生成闭包，被调用 %d 次", textCalls)
	}
	if !bytes.Equal(gotImage, raw) {
		t.Fatalf("视觉闭包收到的图片必须是原图 bytes：got %d bytes want %d", len(gotImage), len(raw))
	}
	for _, want := range []string{"观察描述式点评", "不打分", "不评级", "不排名", "不替孩子重画", "只依据图中可见证据", "画一处雨后场景", "想画出雨后安静的感觉", "三年级上"} {
		if !strings.Contains(gotPrompt, want) {
			t.Fatalf("视觉提示词缺少 %q：\n%s", want, gotPrompt)
		}
	}
	if !strings.Contains(out.Feedback, "大树") {
		t.Fatalf("点评输出应原样返回: %q", out.Feedback)
	}
	// 美术走内嵌快照基座（测试进程未注入盘上 loader）→ 来源戳应为 embedded。
	if !strings.Contains(out.SkillStamp, "art-feedback") || !strings.HasSuffix(out.SkillStamp, "/embedded") {
		t.Fatalf("美术点评来源戳应为 art-feedback@…/embedded，got %q", out.SkillStamp)
	}
}

// TestGenerateWorkFeedback_Art_DataURLAsset data URL 内联载体同样走视觉路径。
func TestGenerateWorkFeedback_Art_DataURLAsset(t *testing.T) {
	raw := encodeTestArtPNG(t)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)

	var gotImage []byte
	a := NewSolveAdapter(nil, WithWorkFeedbackVision(func(ctx context.Context, image []byte, prompt string) (string, error) {
		gotImage = append([]byte(nil), image...)
		return "画面中央的房子占了大半张纸，颜色涂得很满；建议下次留一点天空。", nil
	}))
	if _, err := a.GenerateWorkFeedback(context.Background(), usecase.WorkFeedbackRequest{
		WorkType: k12.WorkTypeArt, Title: "《我的家》", Task: "画自己的家", SourceAssetID: dataURL,
	}); err != nil {
		t.Fatalf("data URL 载体应可用: %v", err)
	}
	if !bytes.Equal(gotImage, raw) {
		t.Fatal("data URL 解码后的图片必须与原图一致")
	}
}

// TestGenerateWorkFeedback_Art_HonestErrors 图缺失/读不到/非图片/视觉未接入 → 诚实报错，
// 视觉闭包与纯文本闭包都不得被触发（不虚构观察）。
func TestGenerateWorkFeedback_Art_HonestErrors(t *testing.T) {
	notImage := filepath.Join(t.TempDir(), "not-image.txt")
	if err := os.WriteFile(notImage, []byte("这不是图片"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		assetID string
		wantErr string
	}{
		{"asset 为空", "", "未携带图片资产"},
		{"文件不存在", filepath.Join(t.TempDir(), "missing.png"), "读取原图失败"},
		{"非图片文件", notImage, "不是图片"},
		{"data URL 缺载荷", "data:image/png,raw-not-base64", "缺少 base64 载荷"},
		{"data URL 坏 base64", "data:image/png;base64,!!!!", "base64 解码失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			visionCalls := 0
			a := NewSolveAdapter(nil, WithWorkFeedbackVision(func(ctx context.Context, image []byte, prompt string) (string, error) {
				visionCalls++
				return "x", nil
			}))
			_, err := a.GenerateWorkFeedback(context.Background(), usecase.WorkFeedbackRequest{
				WorkType: k12.WorkTypeArt, Title: "t", Task: "t", SourceAssetID: tc.assetID,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("应诚实报错（含 %q），got %v", tc.wantErr, err)
			}
			if visionCalls != 0 {
				t.Fatal("图片不可用时不得调用视觉模型")
			}
		})
	}
}

// TestGenerateWorkFeedback_Art_VisionUnwired 视觉闭包未注入 → 「需要视觉通道」诚实报错。
func TestGenerateWorkFeedback_Art_VisionUnwired(t *testing.T) {
	a := NewSolveAdapter(nil, WithWorkFeedbackGen(func(ctx context.Context, subject, prompt, grade string) (string, error) {
		return "不该走这里", nil
	}))
	_, err := a.GenerateWorkFeedback(context.Background(), usecase.WorkFeedbackRequest{
		WorkType: k12.WorkTypeArt, Title: "t", Task: "t", SourceAssetID: "whatever.png",
	})
	if err == nil || !strings.Contains(err.Error(), "视觉通道") {
		t.Fatalf("未接视觉闭包应诚实报错「需要视觉通道」，got %v", err)
	}
}

// TestGenerateWorkFeedback_Writing_StaysTextChannel 写作分支保持纯文本生成，不碰视觉闭包。
func TestGenerateWorkFeedback_Writing_StaysTextChannel(t *testing.T) {
	visionCalls := 0
	var gotSubject, gotPrompt string
	a := NewSolveAdapter(nil,
		WithWorkFeedbackGen(func(ctx context.Context, subject, prompt, grade string) (string, error) {
			gotSubject, gotPrompt = subject, prompt
			return "「柳枝像绿色的丝带」比喻贴切；建议结尾补一个听觉细节。", nil
		}),
		WithWorkFeedbackVision(func(ctx context.Context, image []byte, prompt string) (string, error) {
			visionCalls++
			return "x", nil
		}),
	)
	out, err := a.GenerateWorkFeedback(context.Background(), usecase.WorkFeedbackRequest{
		WorkType: k12.WorkTypeWriting, Title: "《春天的校园》", Task: "观察春景写一段",
		ContentMarkdown: "柳枝像绿色的丝带，随风轻轻摆动。",
	})
	if err != nil {
		t.Fatalf("写作点评: %v", err)
	}
	if visionCalls != 0 {
		t.Fatal("写作点评不得触发视觉闭包")
	}
	if gotSubject != "语文" || !strings.Contains(gotPrompt, "绿色的丝带") {
		t.Fatalf("写作纯文本请求异常: subject=%q prompt=%q", gotSubject, gotPrompt)
	}
	if !strings.Contains(out.Feedback, "比喻") {
		t.Fatalf("写作点评输出异常: %q", out.Feedback)
	}
	if !strings.Contains(out.SkillStamp, "writing-feedback") || !strings.HasSuffix(out.SkillStamp, "/embedded") {
		t.Fatalf("写作点评来源戳应为 writing-feedback@…/embedded，got %q", out.SkillStamp)
	}
}
