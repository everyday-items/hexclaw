package k12

import (
	"io/fs"
	"strings"
	"testing"
)

// BUG-20260712 #9 IM 识题「内联回显」契约回归锁。
//
// 背景：桌面拍照识题有交互门（RecognizeGuardPanel 展示 OCR 结果让家长确认后才批改），
// 但 IM（钉钉/微信）此前无回显护栏——「真图解题门」为单轮效率硬化成「整页全解 + 最终答案直出」，
// OCR 读错会静默带错整页答案，家长无从察觉。用户定夺方案=「内联回显」：不阻塞等确认，
// 在解答同一条消息开头先列「我读到的题目」抬头再给整页解答，家长一眼可核对纠正。
//
// 本锁校验 homework-checker skill 的 OCR 回显护栏落成「内联回显」契约，且不回退成
// 「先不解题、等家长确认后才开始」的阻塞式两轮门（IM 场景往返代价高，已弃）。
func TestHomeworkChecker_InlineEchoContract(t *testing.T) {
	raw, err := fs.ReadFile(BundledSkillsFS(), "skills/homework-checker.md")
	if err != nil {
		t.Fatalf("读取 homework-checker.md 失败: %v", err)
	}
	body := string(raw)

	// 必含：内联回显契约要素（回显抬头 + 同条消息 + 不确定字符护栏）。
	mustContain := map[string]string{
		"内联回显":   "缺内联回显方案声明",
		"我读到的题目": "缺回显抬头（先列识别到的题目让家长核对）",
		"同一条消息":  "缺「同一条消息内回显+解答」的单轮契约",
		"[?]":    "缺手写不确定字符 [?] 标注护栏",
		"待核对":    "缺 [?] 题在确认前只标待核对、不下批改结论的护栏",
	}
	for sub, why := range mustContain {
		if !strings.Contains(body, sub) {
			t.Errorf("回显护栏契约缺失「%s」：%s", sub, why)
		}
	}

	// 不得回退：阻塞式两轮门的措辞（识别完先不解题、等家长确认读得对后才进入）已弃。
	mustNotContain := map[string]string{
		"先不解题": "不得回退到「识别完先不解题」的阻塞式两轮门（IM 往返代价高）",
		"读得对":  "不得保留「家长确认『读得对』后才进入」的阻塞门措辞",
	}
	for sub, why := range mustNotContain {
		if strings.Contains(body, sub) {
			t.Errorf("回显护栏残留阻塞门措辞「%s」：%s", sub, why)
		}
	}
}
