package engineadapter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
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

func TestRecognize_FiltersSectionHeadingAndDeduplicatesNumberedQuestion(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return `[{"question":"四、应用题","subject":"数学","student_answer":""},` +
			`{"question":"二、计算下面各题，能简算","subject":"数学","student_answer":"8.7×17.4"},` +
			`{"question":"-2","subject":"数学","student_answer":""},` +
			`{"question":"8的1/4是多少？","subject":"数学","student_answer":"2"},` +
			`{"question":"2、8的1/4是多少？","subject":"数学","student_answer":"2","bbox":{"x":0.1,"y":0.2,"w":0.2,"h":0.1}},` +
			`{"question":"4.7+2.3","subject":"数学","student_answer":"","bbox":{"x":0.4,"y":0.2,"w":0.1,"h":0.1}},` +
			`{"question":"4.7+2.3=7","subject":"数学","student_answer":""},` +
			`{"question":"一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？","subject":"数学","student_answer":"300÷6=50m 100×2.25=225kg"},` +
			`{"question":"如果每平方米产鱼2.25千克，一共产鱼多少千克？","subject":"数学","student_answer":"300÷6=50m 100×2.25=225kg 50×2=100m 答225kg","bbox":{"x":0.1,"y":0.7,"w":0.8,"h":0.1}}]`, nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 3 {
		t.Fatalf("section heading/numbered duplicate not normalized: %#v", qs)
	}
	if qs[0].BBox == nil {
		t.Fatalf("dedupe should keep richer numbered variant: %#v", qs[0])
	}
	if qs[1].Question != "4.7+2.3" || qs[1].StudentAnswer != "7" || qs[1].BBox == nil {
		t.Fatalf("equation variant should recover handwritten rhs without polluting question: %#v", qs[1])
	}
	if qs[2].Question != "一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？" ||
		!contains(qs[2].StudentAnswer, "答225kg") || qs[2].BBox == nil {
		t.Fatalf("overlapping word-problem fragment should merge into full question: %#v", qs[2])
	}
}

func TestBUG20260715_DeduplicatesBlankLongQuestionWithDuplicatedParticle(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return `[` +
			`{"question":"小明有张10至40排的电影票，这张票的排数和座位号的的最大公约数是13，最小公倍数是72，小明这张电影票是（）排（）号。","subject":"数学","student_answer":""},` +
			`{"question":"小明有张10至40排的电影票，这张票的排数和座位号的最大公约数是13，最小公倍数是72，小明这张电影票是（）排（）号。","subject":"数学","student_answer":""}` +
			`]`, nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 {
		t.Fatalf("one duplicated 的 split one blank movie-ticket question into %d: %#v", len(qs), qs)
	}
	if contains(qs[0].Question, "的的最大公约数") {
		t.Fatalf("dedupe kept duplicated particle instead of cleaner transcription: %q", qs[0].Question)
	}
}

func TestBUG20260715_BlankLongNearTextWithDifferentQuestionIsNotMerged(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return `[` +
			`{"question":"某校五年级有120名学生，男生占总人数的五分之三，男生有多少人？","subject":"数学","student_answer":""},` +
			`{"question":"某校五年级有120名学生，男生占总人数的五分之三，女生有多少人？","subject":"数学","student_answer":""}` +
			`]`, nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 {
		t.Fatalf("same stem with a different question must remain two questions: %#v", qs)
	}
}

// 单份视觉结果没有第二份独立证据时，不能凭规则猜测某个短算式是模型幻觉并静默删除。
// 家长可见的保守回显比误删真实题更诚实。
func TestBUG20260715_UncorroboratedArithmeticRemainsVisible(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return `[{"question":"7.2×12.8","subject":"数学","student_answer":""}]`, nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Question != "7.2×12.8" {
		t.Fatalf("single uncorroborated arithmetic item must be shown for parent review: %#v", qs)
	}
}

func TestRecognize_FiltersBlankCroppedArithmeticFragmentsAfterMerge(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return `[` +
			`{"question":"21×","subject":"数学","student_answer":""},` +
			`{"question":"4x+3","subject":"数学","student_answer":""},` +
			`{"question":"4x+3×0.7=6.5","subject":"数学","student_answer":""},` +
			`{"question":"2.7+4","subject":"数学","student_answer":""},` +
			`{"question":"2.7+4x=12.7","subject":"数学","student_answer":""},` +
			`{"question":"7.2×12.8","subject":"数学","student_answer":""},` +
			`{"question":"8+5","subject":"数学","student_answer":""}` +
			`]`, nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 4 {
		t.Fatalf("cropped blank fragments were not removed conservatively: %#v", qs)
	}
	for _, want := range []string{"4x+3×0.7=6.5", "2.7+4x=12.7", "7.2×12.8", "8+5"} {
		if !containsRecognizedQuestion(qs, want) {
			t.Errorf("complete/independent arithmetic %q was removed: %#v", want, qs)
		}
	}
}

func TestRecognize_CroppedArithmeticFilterNeverDropsStudentAnswerOrUnprovenPrefix(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return `[` +
			`{"question":"21×","subject":"数学","student_answer":"手写42"},` +
			`{"question":"4x+3","subject":"数学","student_answer":"手写过程"},` +
			`{"question":"4x+3×0.7=6.5","subject":"数学","student_answer":""},` +
			`{"question":"3+4","subject":"数学","student_answer":""},` +
			`{"question":"3+45=48","subject":"数学","student_answer":""}` +
			`]`, nil
	}
	qs, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"21×", "4x+3", "4x+3×0.7=6.5", "3+4", "3+45=48"} {
		if !containsRecognizedQuestion(qs, want) {
			t.Errorf("answered or unproven item %q was removed: %#v", want, qs)
		}
	}
}

func containsRecognizedQuestion(questions []usecase.RecognizedQuestion, want string) bool {
	for _, question := range questions {
		if question.Question == want {
			return true
		}
	}
	return false
}

// TestRecognizePrompt_AsksForStudentAnswerNoFabricate 提示词契约：显式要求逐题回收作答、
// 空白留空、绝不编造。防回归——prompt 退回旧口径会让视觉模型不回收作答信号。
func TestRecognizePrompt_AsksForStudentAnswerNoFabricate(t *testing.T) {
	for _, kw := range []string{
		"student_answer", "手写作答", "留空", "绝不",
		// BUG-20260714：视觉模型曾把印刷题 `4÷0.5=` 后的手写 `8` 合进 question，
		// 导致已完成作业被误判为空白卷。提示词必须明确分离印刷题干与手写墨迹并给例子。
		"题干只抄印刷体", `question 写 "4÷0.5="`, `student_answer 写 "8"`,
	} {
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
