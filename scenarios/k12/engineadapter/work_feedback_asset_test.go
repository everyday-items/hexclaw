package engineadapter

// asset:// 资产载体接入美术点评视觉链（任务1：真实上传的资产 ID 经 assetstore 解析本地路径）。

import (
	"bytes"
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// TestGenerateWorkFeedback_Art_ResolvesAssetID 资产 ID 载体：POST /assets 落盘的
// asset://<agent>/<sha256>.<ext> 经 assetstore 解析并把原图 bytes 原样送达视觉闭包。
func TestGenerateWorkFeedback_Art_ResolvesAssetID(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	raw := encodeTestArtPNG(t)
	id, err := assetstore.Save("mingming", raw)
	if err != nil {
		t.Fatal(err)
	}

	var gotImage []byte
	a := NewSolveAdapter(nil,
		WithWorkFeedbackVision(func(ctx context.Context, image []byte, prompt string) (string, error) {
			gotImage = append([]byte(nil), image...)
			return "左上角的云用了两层灰蓝，很有雨后的感觉；试试把水洼里的倒影也画一笔。", nil
		}),
	)
	out, err := a.GenerateWorkFeedback(context.Background(), usecase.WorkFeedbackRequest{
		WorkType: k12.WorkTypeArt, Title: "雨后的校园", Task: "写生", SourceAssetID: id,
	})
	if err != nil {
		t.Fatalf("asset:// 载体应可解析: %v", err)
	}
	if !bytes.Equal(gotImage, raw) {
		t.Fatal("视觉闭包应收到落盘原图的原始字节")
	}
	if out.Feedback == "" {
		t.Fatal("应返回点评")
	}

	// 资产不存在（哈希名对但没这文件）→ 诚实报错，不虚构画面。
	_, err = a.GenerateWorkFeedback(context.Background(), usecase.WorkFeedbackRequest{
		WorkType: k12.WorkTypeArt, Title: "x", Task: "t",
		SourceAssetID: "asset://mingming/" + string(bytes.Repeat([]byte("0"), 64)) + ".png",
	})
	if err == nil {
		t.Fatal("不存在的资产必须诚实报错")
	}
}
