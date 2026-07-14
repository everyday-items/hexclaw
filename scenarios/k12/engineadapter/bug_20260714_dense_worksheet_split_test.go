package engineadapter

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"sync/atomic"
	"testing"
)

// BUG-20260714：密集长作业整页交给 glm-4v-flash 时，模型的 1024 输出上限会把 JSON
// 截断。长图必须拆成有重叠的纵向分片识别，再把分片 bbox 映射回原图坐标。
func TestBUG20260714_DenseTallWorksheetSplitsAndRemapsBBox(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 600, 1800))
	for y := 0; y < 1800; y++ {
		for x := 0; x < 600; x++ {
			img.Set(x, y, color.White)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	vision := func(_ context.Context, segment []byte, prompt string) (string, error) {
		calls.Add(1)
		if !strings.Contains(prompt, "纵向分片") {
			return "", fmt.Errorf("长图未带分片约束")
		}
		var part int
		if _, err := fmt.Sscanf(prompt[strings.LastIndex(prompt, "纵向分片"):], "纵向分片 %d/5", &part); err != nil {
			return "", fmt.Errorf("缺少可解析的分片编号: %w", err)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(segment))
		if err != nil || cfg.Height >= 1800 {
			return "", fmt.Errorf("没有真正裁剪长图: size=%dx%d err=%v", cfg.Width, cfg.Height, err)
		}
		return fmt.Sprintf(`[{"question":"分片%d题目","subject":"数学","knowledge_points":["测试"],"student_answer":"%d","bbox":{"x":0.1,"y":0.5,"w":0.2,"h":0.1}}]`, part, part), nil
	}

	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), encoded.Bytes())
	if err != nil {
		t.Fatalf("Recognize() error = %v", err)
	}
	if got := calls.Load(); got != 5 {
		t.Fatalf("vision calls = %d, want 5", got)
	}
	if len(qs) != 5 {
		t.Fatalf("questions = %d, want 5: %#v", len(qs), qs)
	}
	for i, q := range qs {
		if q.Question != fmt.Sprintf("分片%d题目", i+1) {
			t.Fatalf("question[%d] = %q", i, q.Question)
		}
		if q.BBox == nil || q.BBox.Y <= 0 || q.BBox.Y >= 1 || q.BBox.Y+q.BBox.H > 1.005 {
			t.Fatalf("question[%d] bbox not remapped to full image: %#v", i, q.BBox)
		}
		if i > 0 && qs[i-1].BBox != nil && q.BBox.Y <= qs[i-1].BBox.Y {
			t.Fatalf("bbox order not preserved: prev=%#v current=%#v", qs[i-1].BBox, q.BBox)
		}
	}
}

// 底部应用题通常跨图片 72%～95% 高度。最后一片若从 78% 才开始，会只读到“如果每平方米…”
// 后半句；倒数第二片又在 84% 截断答案，最终没有任何一片拥有完整题干 + 作答。
func TestBUG20260714_BottomSegmentCoversWholeWordProblem(t *testing.T) {
	last := denseWorksheetRanges[len(denseWorksheetRanges)-1]
	if last[0] > 0.72 || last[1] < 1 {
		t.Fatalf("最后分片必须完整覆盖底部应用题区，range=%v", last)
	}
}

func TestBUG20260714_CrossSegmentFragmentMergesIntoCompleteWordProblem(t *testing.T) {
	segments := []worksheetSegment{
		{image: []byte("part-4"), index: 4, total: 5, startY: 0.58, endY: 0.84},
		{image: []byte("part-5"), index: 5, total: 5, startY: 0.68, endY: 1.00},
	}
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		if strings.Contains(prompt, "纵向分片 4/5") {
			return `[{"question":"如果每平方米产鱼2.25千克，一共产鱼多少千克？","subject":"数学","knowledge_points":["长方形面积"],"student_answer":"5000×2.25=11250千克","bbox":{"x":0.48,"y":0.72,"w":0.42,"h":0.20}}]`, nil
		}
		if strings.Contains(prompt, "纵向分片 5/5") {
			return `[{"question":"一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？","subject":"数学","knowledge_points":["长方形面积"],"student_answer":"300÷6=50米；50×2=100米；5000×2.25=11250千克","bbox":{"x":0.48,"y":0.02,"w":0.42,"h":0.82}}]`, nil
		}
		return `[]`, nil
	}

	qs, err := NewRecognizerAdapter(vision).recognizeSegments(context.Background(), segments)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || !strings.Contains(qs[0].Question, "周长是300米") {
		t.Fatalf("cross-segment cropped/full variants were not merged: %#v", qs)
	}
}

// 顶部横排小口算在整宽分片中字号很小，glm-4v-flash 偶发只返回印刷题干、漏掉所有手写答案。
// 对“有题但整片 0 作答”的可疑分片必须做一次聚焦手写复核，再与首轮结果合并；否则已答卷会被误走空白解题。
func TestBUG20260714_SuspiciousSegmentRecoversHandwritingOnFocusedPass(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 600, 1800))
	for y := 0; y < 1800; y++ {
		for x := 0; x < 600; x++ {
			img.Set(x, y, color.White)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}

	var partCalls [5]atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		var part int
		if _, err := fmt.Sscanf(prompt[strings.LastIndex(prompt, "纵向分片"):], "纵向分片 %d/5", &part); err != nil {
			return "", fmt.Errorf("缺少可解析的分片编号: %w", err)
		}
		call := partCalls[part-1].Add(1)
		if part == 1 && call == 1 {
			return `[{"question":"4÷0.5=","subject":"数学","knowledge_points":["小数除法"],"student_answer":""}]`, nil
		}
		if part == 1 {
			if !strings.Contains(prompt, "复核手写答案") {
				return "", fmt.Errorf("第二遍没有聚焦手写复核约束")
			}
			return `[{"question":"4÷0.5=","subject":"数学","knowledge_points":["小数除法"],"student_answer":"8","bbox":{"x":0.1,"y":0.4,"w":0.2,"h":0.1}}]`, nil
		}
		return fmt.Sprintf(`[{"question":"分片%d题目","subject":"数学","knowledge_points":["测试"],"student_answer":"%d"}]`, part, part), nil
	}

	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), encoded.Bytes())
	if err != nil {
		t.Fatalf("Recognize() error = %v", err)
	}
	if got := partCalls[0].Load(); got != 2 {
		t.Fatalf("suspicious segment calls = %d, want focused second pass", got)
	}
	if len(qs) != 5 || qs[0].StudentAnswer != "8" || qs[0].BBox == nil {
		t.Fatalf("focused handwriting result not merged: %#v", qs)
	}
}

// 整宽分片里的横排口算仍可能因字号过小而漏列；当首段只识出少量短题时，必须追加横向重叠放大，
// 并把局部 bbox.x 映射回整图。这样不是依赖模型“再随机试一次”，而是实际给足视觉分辨率。
func TestBUG20260714_SparseShortSegmentUsesHorizontalZoom(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 900, 1800))
	for y := 0; y < 1800; y++ {
		for x := 0; x < 900; x++ {
			img.Set(x, y, color.White)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}

	var partCalls [5]atomic.Int32
	vision := func(_ context.Context, cropped []byte, prompt string) (string, error) {
		var part int
		if _, err := fmt.Sscanf(prompt[strings.LastIndex(prompt, "纵向分片"):], "纵向分片 %d/5", &part); err != nil {
			return "", err
		}
		partCalls[part-1].Add(1)
		if part == 1 && strings.Contains(prompt, "横向放大") {
			cfg, _, err := image.DecodeConfig(bytes.NewReader(cropped))
			if err != nil || cfg.Width >= 900 {
				return "", fmt.Errorf("横向放大没有真正裁图: %dx%d err=%v", cfg.Width, cfg.Height, err)
			}
			var zoom int
			if _, err := fmt.Sscanf(prompt[strings.LastIndex(prompt, "横向放大"):], "横向放大 %d/3", &zoom); err != nil {
				return "", err
			}
			return fmt.Sprintf(`[{"question":"放大%d题","subject":"数学","knowledge_points":["口算"],"student_answer":"%d","bbox":{"x":0.5,"y":0.4,"w":0.1,"h":0.1}}]`, zoom, zoom), nil
		}
		if part == 1 {
			return `[{"question":"1+1=","subject":"数学","knowledge_points":["口算"],"student_answer":"2"},{"question":"2+2=","subject":"数学","knowledge_points":["口算"],"student_answer":"4"}]`, nil
		}
		return fmt.Sprintf(`[{"question":"分片%d长题目内容用于避免横向放大","subject":"数学","knowledge_points":["测试"],"student_answer":"%d"}]`, part, part), nil
	}

	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := partCalls[0].Load(); got != 4 { // 整宽 1 次 + 横向 3 次
		t.Fatalf("first segment calls = %d, want 4", got)
	}
	if len(qs) != 9 { // 首轮 2 + 横向新增 3 + 其余 4 段
		t.Fatalf("questions = %d, want 9: %#v", len(qs), qs)
	}
	if qs[2].BBox == nil || qs[2].BBox.X <= 0 || qs[2].BBox.X >= 0.42 {
		t.Fatalf("left zoom bbox.x not remapped: %#v", qs[2].BBox)
	}
	if qs[4].BBox == nil || qs[4].BBox.X <= 0.58 {
		t.Fatalf("right zoom bbox.x not remapped: %#v", qs[4].BBox)
	}
}
