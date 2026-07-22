package channel

// mathtext.go —— IM 出口的 LaTeX→Unicode 确定性兜底转换。
//
// 归属申报（哪层负责呈现适配）：channel.Message.Text 的既有契约就是「渠道适配层负责
// Markdown/纯文本投影降级」（见 channel.go）；IM（钉钉）不渲染 LaTeX，LaTeX→Unicode
// 属同一类呈现降级，故落在通道层。且 cron 投递是平台通用面也要复用本转换，放
// scenarios/k12 会倒置依赖方向（平台层不得 import K12 域）；本文件保持渠道中立、
// 零 K12 领域词（与 AP-1 同向）。
//
// 背景：solve 链提示词已硬禁 LaTeX、要求 Unicode 数学符号（engine/solve.go），但提示词
// 是软约束——识别侧真机取证过模型会违反（BUG-20260712-U）。桌面 HTTP 响应不经本转换
// （桌面前端可自行渲染，保持原文），只有 IM 投递出口过这一层。
//
// 硬约束：
//   - 转换幂等：f(f(x)) == f(x)；
//   - 不含 LaTeX 的文本零改动：``` 围栏代码块 / `行内代码` / URL 整段保护，
//     反斜杠命令按精确词表匹配（\Users、\note 这类非命令原样保留）；
//   - 未知反斜杠命令只在「已证实的数学定界符」（$…$、\(…\)、\[…\]）内部才剥反斜杠
//     保留词干（兜底降级），定界符外绝不动；
//   - $…$ 只有内部含 LaTeX 特征（\命令、^指数、_下标）才按公式剥定界符，
//     「价格 $5 和 $10」这类文本零改动。

import (
	"regexp"
	"strings"
)

// latexSymbols 是确定性符号映射词表（精确全词匹配；\left/\right 剥除、\quad 降为空格）。
var latexSymbols = map[string]string{
	"times": "×", "div": "÷", "cdot": "·", "pm": "±", "mp": "∓",
	"leq": "≤", "le": "≤", "geq": "≥", "ge": "≥",
	"neq": "≠", "ne": "≠", "approx": "≈", "equiv": "≡",
	"pi": "π", "infty": "∞", "degree": "°",
	"alpha": "α", "beta": "β", "gamma": "γ", "delta": "δ", "Delta": "Δ",
	"theta": "θ", "lambda": "λ", "mu": "μ", "sigma": "σ", "omega": "ω", "phi": "φ",
	"left": "", "right": "", "quad": " ", "qquad": " ",
}

var (
	// protectedSpanRe 保护段：``` 围栏代码块（允许未闭合到文末）、`行内代码`、URL——整段不转换。
	protectedSpanRe = regexp.MustCompile("(?s)```.*?(?:```|\\z)|`[^`\n]*`|https?://[^\\s]+")
	// latexCommandRe 反斜杠命令（全词：[a-zA-Z]+ 贪婪，\leq2 只取 leq、\fracture 取整词不误配 \frac）。
	latexCommandRe = regexp.MustCompile(`\\([a-zA-Z]+)`)
	// latexHintRe $…$ 内部的 LaTeX 特征：\命令 / ^指数 / _下标。
	latexHintRe = regexp.MustCompile(`\\[a-zA-Z]|\^[{0-9-]|_[{0-9]`)
	supBracedRe = regexp.MustCompile(`\^\{(-?[0-9]+)\}`)
	supBareRe   = regexp.MustCompile(`\^(-?[0-9]+)`)
	subBracedRe = regexp.MustCompile(`_\{([0-9]+)\}`)
	subBareRe   = regexp.MustCompile(`_([0-9]+)`)
	// LaTeX 细/负间距命令：\\, \\; \\: \\!。数字与单位间统一降级为单空格；
	// 与 adapter.NormalizeMathText 的既有 IM 投影规则保持一致。
	latexSpacingRe = regexp.MustCompile("\\s*\\\\[,;:!]\\s*")
	superscripts   = map[rune]rune{'0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶', '7': '⁷', '8': '⁸', '9': '⁹', '-': '⁻'}
	subscripts     = map[rune]rune{'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆', '7': '₇', '8': '₈', '9': '₉', '-': '₋'}
)

// LaTeXToUnicode 把常见 LaTeX 写法确定性映射为 Unicode 数学符号，返回转换结果与是否改动。
// 幂等；无 LaTeX 零改动；代码块/行内代码/URL 整段保护（契约见 mathtext_test.go）。
func LaTeXToUnicode(s string) (string, bool) {
	// 快速路径：没有任何 LaTeX 触发字符时零分配返回。
	if !strings.ContainsAny(s, "\\$^") && !strings.Contains(s, "_{") {
		return s, false
	}
	var b strings.Builder
	last := 0
	for _, m := range protectedSpanRe.FindAllStringIndex(s, -1) {
		b.WriteString(convertSegment(s[last:m[0]]))
		b.WriteString(s[m[0]:m[1]])
		last = m[1]
	}
	b.WriteString(convertSegment(s[last:]))
	out := b.String()
	return out, out != s
}

// convertSegment 处理一个非保护段：先剥数学定界符（内部按数学模式转换，未知命令可降级），
// 再对剩余文本做精确词表转换（定界符外未知命令绝不动）。
func convertSegment(s string) string {
	s = stripDelimited(s, `\(`, `\)`)
	s = stripDelimited(s, `\[`, `\]`)
	s = stripDollarMath(s)
	return convertMath(s, false)
}

// stripDelimited 剥 \(…\) / \[…\] 定界符（其存在即证实 LaTeX，内部按数学模式转换）。
func stripDelimited(s, open, close string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, open)
		if i < 0 {
			break
		}
		j := strings.Index(s[i+len(open):], close)
		if j < 0 {
			break
		}
		b.WriteString(s[:i])
		b.WriteString(strings.TrimSpace(convertMath(s[i+len(open):i+len(open)+j], true)))
		s = s[i+len(open)+j+len(close):]
	}
	b.WriteString(s)
	return b.String()
}

// stripDollarMath 剥 $…$ / $$…$$ 定界符：仅当内部含 LaTeX 特征才视为公式
// （货币「$5 和 $10」零改动）。单 $ 只在同一行内配对。
func stripDollarMath(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "$$") {
			if j := strings.Index(s[i+2:], "$$"); j >= 0 {
				inner := s[i+2 : i+2+j]
				if latexHintRe.MatchString(inner) {
					b.WriteString(strings.TrimSpace(convertMath(inner, true)))
				} else {
					b.WriteString(s[i : i+2+j+2])
				}
				i += 2 + j + 2
				continue
			}
			b.WriteString("$$")
			i += 2
			continue
		}
		rest := s[i+1:]
		limit := len(rest)
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			limit = nl
		}
		if j := strings.IndexByte(rest[:limit], '$'); j > 0 {
			inner := rest[:j]
			if latexHintRe.MatchString(inner) {
				b.WriteString(strings.TrimSpace(convertMath(inner, true)))
				i += 1 + j + 1
				continue
			}
		}
		b.WriteByte('$')
		i++
	}
	return b.String()
}

// convertMath 对一段文本做确定性 LaTeX→Unicode 转换。
// math=true 表示已证实处于数学定界符内：未知命令剥反斜杠保留词干、裸下标 _2 也转换；
// math=false（定界符外）只按精确词表转换，未知命令原样保留。
func convertMath(s string, math bool) string {
	s = latexSpacingRe.ReplaceAllString(s, " ")
	s = expandStructural(s, math)
	s = strings.ReplaceAll(s, `^{\circ}`, "°")
	s = strings.ReplaceAll(s, `^\circ`, "°")
	s = latexCommandRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1:]
		if rep, ok := latexSymbols[name]; ok {
			return rep
		}
		if math {
			return name // 兜底降级：剥反斜杠保留词干（仅数学定界符内）
		}
		return m
	})
	s = supBracedRe.ReplaceAllStringFunc(s, func(m string) string { return mapScript(m[2:len(m)-1], superscripts) })
	s = supBareRe.ReplaceAllStringFunc(s, func(m string) string { return mapScript(m[1:], superscripts) })
	s = subBracedRe.ReplaceAllStringFunc(s, func(m string) string { return mapScript(m[2:len(m)-1], subscripts) })
	if math {
		s = subBareRe.ReplaceAllStringFunc(s, func(m string) string { return mapScript(m[1:], subscripts) })
	}
	return s
}

func mapScript(digits string, table map[rune]rune) string {
	var b strings.Builder
	for _, r := range digits {
		b.WriteRune(table[r])
	}
	return b.String()
}

// structCmds 带花括号参数的结构性命令；bareDigit 允许 \frac12 / \sqrt2 的无括号单数字参数。
var structCmds = []struct {
	name      string
	args      int
	bareDigit bool
}{
	{`\dfrac`, 2, true}, {`\tfrac`, 2, true}, {`\frac`, 2, true},
	{`\sqrt`, 1, true}, {`\text`, 1, false}, {`\mathrm`, 1, false},
}

// expandStructural 展开 \frac{a}{b}→a/b、\sqrt{x}→√x、\text/\mathrm{…}→内容原样。
// 参数用平衡花括号扫描并递归转换——嵌套 \frac{\frac{1}{2}}{3} 自然降级为 (1/2)/3。
func expandStructural(s string, math bool) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if out, next, ok := expandStructuralAt(s, i, math); ok {
			b.WriteString(out)
			i = next
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func expandStructuralAt(s string, i int, math bool) (string, int, bool) {
	for _, c := range structCmds {
		if !strings.HasPrefix(s[i:], c.name) {
			continue
		}
		j := i + len(c.name)
		if j < len(s) && isASCIILetter(s[j]) {
			return "", 0, false // \fracture / \textbf 等更长命令：不是本命令，交由词表精确匹配
		}
		args := make([]string, 0, c.args)
		pos := j
		for k := 0; k < c.args; k++ {
			arg, next, ok := parseBraceArg(s, pos, c.bareDigit)
			if !ok {
				return "", 0, false
			}
			args = append(args, arg)
			pos = next
		}
		return renderStructural(c.name, args, math), pos, true
	}
	return "", 0, false
}

func parseBraceArg(s string, pos int, bareDigit bool) (string, int, bool) {
	for pos < len(s) && s[pos] == ' ' {
		pos++
	}
	if pos >= len(s) {
		return "", 0, false
	}
	if s[pos] == '{' {
		depth := 0
		for k := pos; k < len(s); k++ {
			switch s[k] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return s[pos+1 : k], k + 1, true
				}
			}
		}
		return "", 0, false
	}
	if bareDigit && s[pos] >= '0' && s[pos] <= '9' {
		return string(s[pos]), pos + 1, true
	}
	return "", 0, false
}

func renderStructural(name string, args []string, math bool) string {
	switch name {
	case `\frac`, `\dfrac`, `\tfrac`:
		return fracOperand(convertMath(args[0], math)) + "/" + fracOperand(convertMath(args[1], math))
	case `\sqrt`:
		inner := convertMath(args[0], math)
		if needsParens(inner) {
			return "√(" + inner + ")"
		}
		return "√" + inner
	default: // \text / \mathrm：内容原样
		return args[0]
	}
}

// fracOperand 多项操作数加括号保数学正确（\frac{a+b}{2}→(a+b)/2、嵌套分数→(1/2)/3）。
func fracOperand(s string) string {
	if needsParens(s) {
		return "(" + s + ")"
	}
	return s
}

func needsParens(s string) bool {
	return strings.ContainsAny(s, "+-/ ") || strings.ContainsAny(s, "−±×÷·")
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
