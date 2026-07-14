package engineadapter

import (
	"regexp"
	"strconv"
	"strings"
)

var retryStepLineRE = regexp.MustCompile(`^第[一二三四五六七八九十百0-9]+(?:步|次|种|点|阶段)[^：:\n]{0,12}[：:]`)

// normalizeRetryMarkdown 是「再练一道」模型输出的容错边界。模型即使偶尔忽略提示词、返回
// “问题：/解答：/答案：”普通文本，也会被收口成前端可稳定渲染的 GitHub Markdown。
func normalizeRetryMarkdown(content string) string {
	content = stripOuterMarkdownFence(strings.TrimSpace(content))
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+6)
	section := ""
	step := 0
	appendLine := func(line string) {
		if strings.TrimSpace(line) == "" {
			if len(out) == 0 || out[len(out)-1] == "" {
				return
			}
			out = append(out, "")
			return
		}
		out = append(out, line)
	}

	for _, line := range lines {
		if heading, tail, ok := retrySectionLine(line); ok {
			appendLine("")
			appendLine("## " + heading)
			appendLine("")
			section = heading
			step = 0
			if tail != "" {
				appendLine(tail)
			}
			continue
		}

		trimmed := strings.TrimSpace(line)
		if section == "解答" && retryStepLineRE.MatchString(trimmed) {
			step++
			appendLine(strings.Repeat(" ", leadingSpaces(line)) + strconv.Itoa(step) + ". " + trimmed)
			continue
		}
		appendLine(line)
	}

	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// normalizeSolveMarkdown 是空白作业 /solve 的呈现边界。真实 solver 常给“计划/第1步/答案：”
// 普通文本；补上稳定的 GitHub Markdown 章节，并突出最终答案，桌面与钉钉共用同一正文。
func normalizeSolveMarkdown(content string) string {
	content = normalizeRetryMarkdown(content)
	if content == "" {
		return ""
	}
	if !strings.Contains(content, "## 解答") {
		content = "## 解答\n\n" + content
	}

	lines := strings.Split(content, "\n")
	inAnswer := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "## 答案" {
			inAnswer = true
			continue
		}
		if !inAnswer || trimmed == "" {
			continue
		}
		// 验算引用不是答案；模型漏答案正文时保持原样。
		if strings.HasPrefix(trimmed, ">") || strings.HasPrefix(trimmed, "#") {
			break
		}
		if !strings.HasPrefix(trimmed, "**") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = indent + "**" + trimmed + "**"
		}
		break
	}
	return strings.Join(lines, "\n")
}

// retrySectionLine 识别模型常见的纯文本、粗体和 Markdown 标题三种段名写法。
func retrySectionLine(line string) (heading, tail string, ok bool) {
	s := strings.TrimSpace(line)
	for strings.HasPrefix(s, "#") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "#"))
	}
	if strings.HasPrefix(s, "**") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "**"))
	}

	type sectionName struct{ raw, canonical string }
	sections := []sectionName{
		{"最终答案", "答案"},
		{"解题过程", "解答"},
		{"问题", "问题"},
		{"题目", "问题"},
		{"解答", "解答"},
		{"解析", "解答"},
		{"答案", "答案"},
	}
	for _, candidate := range sections {
		if !strings.HasPrefix(s, candidate.raw) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(s, candidate.raw))
		switch {
		case rest == "":
			return candidate.canonical, "", true
		case strings.HasPrefix(rest, "："):
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "："))
		case strings.HasPrefix(rest, ":"):
			rest = strings.TrimSpace(strings.TrimPrefix(rest, ":"))
		case strings.HasPrefix(rest, "**"):
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "**"))
		default:
			continue
		}
		if strings.HasPrefix(rest, "**") {
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "**"))
		}
		return candidate.canonical, rest, true
	}
	return "", "", false
}

func stripOuterMarkdownFence(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[len(lines)-1]) != "```" {
		return content
	}
	first := strings.ToLower(strings.TrimSpace(lines[0]))
	if first != "```" && first != "```md" && first != "```markdown" && first != "```gfm" {
		return content
	}
	return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
}

func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
}
