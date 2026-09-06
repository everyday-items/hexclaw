package usecase

import "strings"

// SplitRetryPresentation 「再练一道」题答分离（2026-07-18 P2 清偿，守答案遮罩红线）。
//
// 变式题正文经 engineadapter.normalizeRetryMarkdown 收口成 ## 问题 / ## 解答 / ## 答案
// 章节（模型输出“问题：/解答：/答案：”等普通文本也会被归一化），本函数按章节拆：
//   - question       = ## 问题 章节正文（先显给孩子，不含任何解答）；
//   - answer         = 自 ## 解答（或 ## 答案）起的全部内容（默认遮罩，家长点按才揭示）；
//   - expectedAnswer = ## 答案 章节正文（去粗体标记；装篮 expected_answer_markdown 用）。
//
// 拆分路线申报：解析归一化产物而非改提示词——提示词侧已由 normalizeRetryMarkdown 容错收口，
// 解析层零 LLM 依赖、可测可复现。任一边界不成立（无问题章节 / 无解答内容）时诚实回退：
// question 为空、answer 为整段原文，前端整段遮罩（最小闭环，不猜测题答边界）。
func SplitRetryPresentation(solution string) (question, answer, expectedAnswer string) {
	lines := strings.Split(solution, "\n")
	qStart, splitAt := -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case "## 问题":
			if qStart < 0 {
				qStart = i
			}
		case "## 解答", "## 答案":
			if splitAt < 0 {
				splitAt = i
			}
		}
	}
	expectedAnswer = extractAnswerSection(lines)
	// 题答边界成立 = 有问题章节、其后有解答/答案章节、两侧正文都非空。
	if qStart < 0 || splitAt <= qStart {
		return "", solution, expectedAnswer
	}
	q := strings.TrimSpace(strings.Join(lines[qStart+1:splitAt], "\n"))
	a := strings.TrimSpace(strings.Join(lines[splitAt:], "\n"))
	if q == "" || a == "" {
		return "", solution, expectedAnswer
	}
	return q, a, expectedAnswer
}

// extractAnswerSection 取答案章节，剥离末尾确切验算尾注并成对去除完整外层粗体。
func extractAnswerSection(lines []string) string {
	start := -1
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if start < 0 {
			if t == "## 答案" {
				start = i + 1
			}
			continue
		}
		if strings.HasPrefix(t, "## ") {
			lines = lines[:i]
			break
		}
	}
	if start < 0 {
		return ""
	}
	body := strings.TrimSpace(strings.Join(lines[start:], "\n"))
	body = strings.TrimSpace(strings.TrimSuffix(body, "\n> ✅ 最终答案已由独立校验员用代码重算核验一致（高置信）。"))
	if strings.HasPrefix(body, "**") && strings.HasSuffix(body, "**") && strings.Count(body, "**") == 2 {
		body = body[2 : len(body)-2]
	}
	return strings.TrimSpace(body)
}
