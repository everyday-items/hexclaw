package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Bug 20260627（item ②）：桌面开放图片上传后，若用户配置的默认模型不具备视觉能力，
// 图片入库在运行时 captioner 调用阶段失败，错误信息只说「图像转写失败: <底层错误>」，
// 不告诉用户根因是「缺少视觉模型」——用户无从修复。AddImageDocument 的两条失败路径
// （未注入 captioner / caption 调用失败）都必须明确提示「视觉模型」，使前端能据此给出
// 可操作引导（并作为跨语言检测的稳定标记）。
func TestAddImageDocument_VisionModelHint(t *testing.T) {
	ctx := context.Background()
	png := []byte{0x89, 'P', 'N', 'G'}

	// 路径 1：未配置 captioner（router 为 nil → 不注入视觉转写器）。
	t.Run("no_captioner", func(t *testing.T) {
		mgr := NewManager(stubRepo{}, stubSearcher{}, nil)
		_, err := mgr.AddImageDocument(ctx, "", png, "image/png", "upload:x.png")
		if err == nil {
			t.Fatal("未配置 captioner 应报错")
		}
		if !strings.Contains(err.Error(), "视觉模型") {
			t.Fatalf("错误应提示「视觉模型」，得: %v", err)
		}
	})

	// 路径 2：captioner 调用失败（典型：配的模型不是视觉模型，上游 400/不支持图片）。
	t.Run("caption_call_fails", func(t *testing.T) {
		failing := CaptionerFunc(func(context.Context, []byte, string) (string, error) {
			return "", errors.New("model does not support image input")
		})
		mgr := NewManager(stubRepo{}, stubSearcher{}, nil, WithCaptioner(failing))
		_, err := mgr.AddImageDocument(ctx, "", png, "image/png", "upload:x.png")
		if err == nil {
			t.Fatal("caption 失败应报错")
		}
		if !strings.Contains(err.Error(), "视觉模型") {
			t.Fatalf("caption 失败错误应提示「视觉模型」，得: %v", err)
		}
		// 仍保留底层原因，便于排障。
		if !strings.Contains(err.Error(), "does not support image input") {
			t.Fatalf("错误应保留底层原因，得: %v", err)
		}
	})
}

// ─── 最小桩：只为构造 Manager，AddImageDocument 在 caption 阶段即返回，不触达写路径 ───

type stubRepo struct{}

func (stubRepo) Init(context.Context) error                     { return nil }
func (stubRepo) Add(context.Context, *Document, []*Chunk) error { return nil }
func (stubRepo) Get(context.Context, string) (*Document, error) { return nil, nil }
func (stubRepo) List(context.Context) ([]*Document, error)      { return nil, nil }
func (stubRepo) GetBySourceTitle(context.Context, string, string) (*Document, error) {
	return nil, nil
}
func (stubRepo) Replace(context.Context, *Document, []*Chunk) error { return nil }
func (stubRepo) Delete(context.Context, string) error               { return nil }

type stubSearcher struct{}

func (stubSearcher) VectorSearch(context.Context, []float32, int, Filter) ([]*SearchResult, error) {
	return nil, nil
}
func (stubSearcher) TextSearch(context.Context, string, int, Filter) ([]*SearchResult, error) {
	return nil, nil
}
