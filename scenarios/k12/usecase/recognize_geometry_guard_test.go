package usecase

// 一次切换终局批（§6.14 · 2026-07-18）配套的覆盖保全：原 apihttp recognize_bbox_test
// 随 POST /recognize 端点删除，其核心不变量（INV-006 前置）搬到 usecase 层钉死——
// 核心识题绝不透出几何坐标：BBox 只能经独立锚点阶段进入值对象，任何 Recognizer
// 实现（含第三方替换）夹带的 bbox 必须被剥掉，否则统一 GradingJob 的停点回显
// 可能拿到未经核验的坐标去画确定性红叉。

import (
	"context"
	"testing"
)

type leakyGeometryRecognizer struct{}

func (leakyGeometryRecognizer) Recognize(context.Context, []byte) ([]RecognizedQuestion, error) {
	return []RecognizedQuestion{
		{
			Question: "3.8×3=?", KnowledgePoints: []string{"小数乘法"},
			AnswerState: AnswerStatePresent, StudentAnswer: "10.4",
			// 核心识题不被信任提供几何坐标；usecase 必须剥掉。
			BBox: &BBox{X: 0.12, Y: 0.34, W: 0.18, H: 0.05},
		},
		{Question: "简算 25×4", KnowledgePoints: []string{"乘法结合律"}, AnswerState: AnswerStateBlank},
	}, nil
}

func TestRecognizeHomework_StripsUntrustedRecognizerGeometry(t *testing.T) {
	d := Deps{Recognizer: leakyGeometryRecognizer{}}
	qs, err := d.RecognizeHomework(context.Background(), []byte("fake-image-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 {
		t.Fatalf("题数不符: %d", len(qs))
	}
	for i, q := range qs {
		if q.BBox != nil {
			t.Errorf("核心识题第 %d 题泄漏几何坐标 %+v——BBox 只能经独立锚点阶段进入", i, *q.BBox)
		}
	}
}
