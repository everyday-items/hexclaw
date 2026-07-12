package engineadapter

import (
	"context"
	"testing"
)

// TestRecognize_RecoversStudentAnswer 识题回收学生作答信号（单一真相源上游）：
// 视觉模型逐题回收孩子手写作答，已答题带出 student_answer，空白题留空字符串。
// 这是「空白卷 vs 已答卷」显式分叉的数据基础——RED 若字段丢失，前端/用例无从区分。
func TestRecognize_RecoversStudentAnswer(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return "```json\n[" +
			"{\"question\":\"3.8×3=?\",\"knowledge_points\":[\"小数乘法\"],\"student_answer\":\"10.4\"}," +
			"{\"question\":\"2x+15=43\",\"knowledge_points\":[\"一元一次方程\"],\"student_answer\":\"\"}" +
			"]\n```", nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("识题报错: %v", err)
	}
	if len(qs) != 2 {
		t.Fatalf("应识出 2 题, got %d", len(qs))
	}
	if qs[0].StudentAnswer != "10.4" {
		t.Errorf("已答题应回收作答 10.4, got %q", qs[0].StudentAnswer)
	}
	if qs[1].StudentAnswer != "" {
		t.Errorf("空白题作答应为空串, got %q", qs[1].StudentAnswer)
	}
}

// TestRecognizePrompt_AsksForStudentAnswerNoFabricate 提示词契约：显式要求逐题回收作答、
// 空白留空、绝不编造。防回归——prompt 退回旧口径会让视觉模型不回收作答信号。
func TestRecognizePrompt_AsksForStudentAnswerNoFabricate(t *testing.T) {
	for _, kw := range []string{"student_answer", "手写作答", "留空", "绝不"} {
		if !contains(recognizePrompt, kw) {
			t.Errorf("识题 prompt 缺关键约束 %q", kw)
		}
	}
}

// TestRecognize_AutoDetectsSubject Polish-2：识题自动判定学科（家长不必手选）。
// 视觉模型逐题回填 subject（白名单 5 科），越界/未知归零，供前端预填学科下拉。
func TestRecognize_AutoDetectsSubject(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return "[" +
			"{\"question\":\"3.8×3=?\",\"subject\":\"数学\",\"knowledge_points\":[\"小数乘法\"],\"student_answer\":\"\"}," +
			"{\"question\":\"翻译 apple\",\"subject\":\"英语\",\"knowledge_points\":[\"单词\"],\"student_answer\":\"\"}," +
			"{\"question\":\"看图说话\",\"subject\":\"生物\",\"knowledge_points\":[\"观察\"],\"student_answer\":\"\"}" +
			"]", nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("识题报错: %v", err)
	}
	if len(qs) != 3 {
		t.Fatalf("应识出 3 题, got %d", len(qs))
	}
	if qs[0].Subject != "数学" {
		t.Errorf("第 1 题应判定学科 数学, got %q", qs[0].Subject)
	}
	if qs[1].Subject != "英语" {
		t.Errorf("第 2 题应判定学科 英语, got %q", qs[1].Subject)
	}
	if qs[2].Subject != "" {
		t.Errorf("越界学科(生物)应归零, got %q", qs[2].Subject)
	}
}

// TestRecognizePrompt_AsksForSubject 提示词契约：显式要求逐题判定学科（5 科白名单）。
func TestRecognizePrompt_AsksForSubject(t *testing.T) {
	for _, kw := range []string{"subject", "判定题目学科", "数学 / 语文 / 英语 / 物理 / 化学"} {
		if !contains(recognizePrompt, kw) {
			t.Errorf("识题 prompt 缺学科判定约束 %q", kw)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
