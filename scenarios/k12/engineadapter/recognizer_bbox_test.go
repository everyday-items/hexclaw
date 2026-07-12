package engineadapter

import (
	"context"
	"testing"
)

// TestRecognize_RecoversBBox 识题回收学生作答区域的归一化边界框（原图批改 Phase 1 数据基础）：
// 视觉模型逐题返回 bbox{x,y,w,h}(0~1)，供前端在原图上叠加 ✓/✗。合法框应原样带出。
// RED 若字段丢失，前端无从定位标记、原图叠加整条链断。
func TestRecognize_RecoversBBox(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return "[" +
			"{\"question\":\"3.8×3=?\",\"subject\":\"数学\",\"knowledge_points\":[\"小数乘法\"],\"student_answer\":\"10.4\",\"bbox\":{\"x\":0.12,\"y\":0.34,\"w\":0.18,\"h\":0.05}}" +
			"]", nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("识题报错: %v", err)
	}
	if len(qs) != 1 {
		t.Fatalf("应识出 1 题, got %d", len(qs))
	}
	b := qs[0].BBox
	if b == nil {
		t.Fatalf("合法 bbox 应被回收, got nil")
	}
	if b.X != 0.12 || b.Y != 0.34 || b.W != 0.18 || b.H != 0.05 {
		t.Errorf("bbox 应原样带出, got %+v", *b)
	}
}

// TestRecognize_BBoxDegradesWhenMissingOrIllegal 硬性诚实门（设计文档 §6）：
// bbox 缺失/非法（越界/零框/负值）一律降级为 nil——该题走纯文字批改，前端不叠加错位红叉。
// bbox 错位比不标更糟：这条测试钉死「宁可不叠加，绝不错位」的降级契约。
func TestRecognize_BBoxDegradesWhenMissingOrIllegal(t *testing.T) {
	cases := []struct {
		name string
		bbox string // 拼进 JSON 的 bbox 片段（含前导逗号），空串=省略 bbox
	}{
		{"缺失", ""},
		{"越界-右", `,"bbox":{"x":0.9,"y":0.1,"w":0.5,"h":0.1}`},
		{"越界-下", `,"bbox":{"x":0.1,"y":0.95,"w":0.1,"h":0.5}`},
		{"零框", `,"bbox":{"x":0.1,"y":0.1,"w":0,"h":0}`},
		{"负宽高", `,"bbox":{"x":0.1,"y":0.1,"w":-0.2,"h":0.1}`},
		{"负坐标", `,"bbox":{"x":-0.1,"y":0.1,"w":0.2,"h":0.1}`},
		{"坐标越界", `,"bbox":{"x":1.4,"y":0.1,"w":0.1,"h":0.1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vision := func(context.Context, []byte, string) (string, error) {
				return "[{\"question\":\"q\",\"subject\":\"数学\",\"knowledge_points\":[],\"student_answer\":\"\"" + tc.bbox + "}]", nil
			}
			qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
			if err != nil {
				t.Fatalf("识题报错: %v", err)
			}
			if len(qs) != 1 {
				t.Fatalf("应识出 1 题, got %d", len(qs))
			}
			if qs[0].BBox != nil {
				t.Errorf("非法/缺失 bbox 应降级为 nil（不叠加）, got %+v", *qs[0].BBox)
			}
		})
	}
}

// TestRecognize_BBoxAllowsEdgeTolerance 贴边框（x+w 略超 1 的浮点误差）应放行——
// 视觉模型归一化常有微小误差，过严会误杀合法框（诚实门要防错位，但别把好框也丢了）。
func TestRecognize_BBoxAllowsEdgeTolerance(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return "[{\"question\":\"q\",\"subject\":\"数学\",\"knowledge_points\":[],\"student_answer\":\"\",\"bbox\":{\"x\":0.7,\"y\":0.7,\"w\":0.302,\"h\":0.302}}]", nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("识题报错: %v", err)
	}
	if qs[0].BBox == nil {
		t.Errorf("贴边框（误差 <0.005）应放行, got nil")
	}
}

// TestRecognizePrompt_AsksForBBox 提示词契约：显式要求逐题返回归一化 bbox、定位不准时省略。
// 防回归——prompt 退回旧口径会让视觉模型不返回坐标，原图叠加无数据。
func TestRecognizePrompt_AsksForBBox(t *testing.T) {
	for _, kw := range []string{"bbox", "归一化", "0 到 1", "省略", "作答区域"} {
		if !contains(recognizePrompt, kw) {
			t.Errorf("识题 prompt 缺 bbox 定位约束 %q", kw)
		}
	}
}
