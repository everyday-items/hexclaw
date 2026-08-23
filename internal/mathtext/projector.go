package mathtext

// projector.go —— IM 出口共享的 Markdown/LaTeX→可读文本确定性投影。
//
// adapter 与 channel 都只通过本包投影，避免平台普通回复和 K12/cron 通道产生两套
// 数学语义。包仅依赖标准库，不反向依赖任一出口。
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
//   - $…$ 只有满足数学内容与闭合边界时才剥定界符；纯数字公式可读，
//     「价格 $5-$10」「原价 $5，现价 $4」这类货币前缀零改动。

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
	"sum": "∑", "int": "∫",
	"left": "", "right": "", "quad": " ", "qquad": " ",
}

var (
	// latexCommandRe 反斜杠命令（全词：[a-zA-Z]+ 贪婪，\leq2 只取 leq、\fracture 取整词不误配 \frac）。
	latexCommandRe = regexp.MustCompile(`\\([a-zA-Z]+)`)
	// 模型或历史 JSON 正文可能把一个 TeX 命令保存成两个反斜杠。只在已证实的
	// 数学定界符内部把「双反斜杠 + 命令/间距符」还原为一个反斜杠；真正的
	// TeX 换行 `\\` 后不是命令 token，仍由布局归一化处理。
	doubleEscapedMathTokenRe = regexp.MustCompile(`\\\\([a-zA-Z]+|[&,;:!])`)
	// latexHintRe $…$ 内部的 LaTeX 特征：\命令 / ^指数 / _下标。
	latexHintRe = regexp.MustCompile(`\\[a-zA-Z]|\^[{0-9-]|_[{0-9]`)
	// 已经是纯文本运算式或完整纯数字的 $…$ 同样属于数学。是否为货币前缀由
	// closing-dollar 边界规则判断，不能靠拒绝所有数字公式规避。
	readableMathHintRe = regexp.MustCompile(`[=+*/^×÷±≤≥≠≈√π-]`)
	numericMathRe      = regexp.MustCompile(`^\s*[+-]?(?:\d+(?:\.\d*)?|\.\d+)\s*$`)
	// 单个 ASCII 字母位于成对美元定界符内时只能表达数学变量；货币仍由数字与闭合边界规则处理。
	identifierMathRe = regexp.MustCompile(`^\s*[A-Za-z]\s*$`)
	supBracedRe      = regexp.MustCompile(`\^\{([-0-9n+=()]+)\}`)
	supBareRe        = regexp.MustCompile(`\^(-?[0-9]+)`)
	subBracedRe      = regexp.MustCompile(`_\{([-0-9+=()]+)\}`)
	subBareRe        = regexp.MustCompile(`_([0-9]+)`)
	// 定界符外的 `_` 在词法上无法区分化学式与普通标识符。这里只找出候选，
	// 再用已冻结的裸化学式白名单 fail-closed，避免把 PCI_2、OS_2 一类标识符改坏。
	chemicalFormulaRe = regexp.MustCompile(`\b(?:[A-Z][a-z]?(?:_[0-9]+)?)+\b`)
	// LaTeX 细/负间距命令：\\, \\; \\: \\!。数字与单位间统一降级为单空格；
	// 与 adapter.NormalizeMathText 的既有 IM 投影规则保持一致。
	latexSpacingRe = regexp.MustCompile("\\s*\\\\[,;:!]\\s*")
	// 常见 display 环境属于排版壳：通道文本保留每行数学语义，去掉 begin/end、
	// 对齐标记和 TeX 行尾。token + 栈配对支持 aligned 内嵌 cases，避免正则从外层
	// begin 错配到内层 end 后泄漏排版命令。
	mathEnvironmentTokenRe = regexp.MustCompile(`\\(begin|end)\s*\{(aligned|align\*?|gathered|cases)\}`)
	latexRowBreakRe        = regexp.MustCompile(`\\\\(?:\[[^]\n]*\])?`)
	superscripts           = map[rune]rune{'0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶', '7': '⁷', '8': '⁸', '9': '⁹', '+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽', ')': '⁾', 'n': 'ⁿ'}
	subscripts             = map[rune]rune{'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆', '7': '₇', '8': '₈', '9': '₉', '+': '₊', '-': '₋', '=': '₌', '(': '₍', ')': '₎'}
)

type byteSpan struct {
	start int
	end   int
}

// WithoutProtectedMarkdown 返回排除围栏代码、行内代码与 URL 后的可见语义文本。
// 分隔处写入换行，避免删除保护区后把两侧 `$` 或反斜杠命令错误拼成一个公式。
// 渲染证据校验复用本函数，确保“投影器保护什么，泄漏检测器就忽略什么”。
func WithoutProtectedMarkdown(s string) string {
	spans := findProtectedSpans(s)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, span := range spans {
		b.WriteString(s[last:span.start])
		b.WriteByte('\n')
		last = span.end
	}
	b.WriteString(s[last:])
	return b.String()
}

// ProjectReadable 把常见 LaTeX 写法确定性映射为可读数学文本，返回结果与是否改动。
// 幂等；无 LaTeX 零改动；代码块、行内代码和 URL 整段保护。
func ProjectReadable(s string) (string, bool) {
	// 快速路径：没有任何 LaTeX 触发字符时零分配返回。
	if !strings.ContainsAny(s, "\\$^_") {
		return s, false
	}
	var b strings.Builder
	last := 0
	for _, span := range findProtectedSpans(s) {
		b.WriteString(convertSegment(s[last:span.start]))
		b.WriteString(s[span.start:span.end])
		last = span.end
	}
	b.WriteString(convertSegment(s[last:]))
	out := b.String()
	return out, out != s
}

// findProtectedSpans recognizes Markdown code delimiters by their actual run
// length and fenced-code line position, plus case-insensitive HTTP(S) URLs.
// This keeps “code with ` inside“ and ~~~ fences atomic instead of trying to
// approximate CommonMark with one regex.
func findProtectedSpans(s string) []byteSpan {
	var spans []byteSpan
	for i := 0; i < len(s); {
		if end, ok := httpURLAt(s, i); ok {
			spans = append(spans, byteSpan{start: i, end: end})
			i = end
			continue
		}
		if s[i] != '`' && s[i] != '~' {
			i++
			continue
		}

		marker := s[i]
		runEnd := i + 1
		for runEnd < len(s) && s[runEnd] == marker {
			runEnd++
		}
		runLength := runEnd - i
		if runLength >= 3 && isFenceLineStart(s, i) {
			end := findFenceEnd(s, runEnd, marker, runLength)
			if end < 0 {
				end = len(s)
			}
			spans = append(spans, byteSpan{start: i, end: end})
			i = end
			continue
		}
		if marker == '`' {
			if end := findMatchingBacktickRun(s, runEnd, runLength); end >= 0 {
				spans = append(spans, byteSpan{start: i, end: end})
				i = end
				continue
			}
		}
		i = runEnd
	}
	return spans
}

func httpURLAt(s string, start int) (int, bool) {
	const http = "http://"
	const https = "https://"
	length := 0
	switch {
	case len(s)-start >= len(https) && strings.EqualFold(s[start:start+len(https)], https):
		length = len(https)
	case len(s)-start >= len(http) && strings.EqualFold(s[start:start+len(http)], http):
		length = len(http)
	default:
		return 0, false
	}
	end := start + length
	for end < len(s) && s[end] != ' ' && s[end] != '\t' && s[end] != '\r' && s[end] != '\n' {
		end++
	}
	return end, true
}

func isFenceLineStart(s string, markerStart int) bool {
	lineStart := strings.LastIndexByte(s[:markerStart], '\n') + 1
	if markerStart-lineStart > 3 {
		return false
	}
	for i := lineStart; i < markerStart; i++ {
		if s[i] != ' ' {
			return false
		}
	}
	return true
}

func findFenceEnd(s string, openerEnd int, marker byte, minimumRun int) int {
	nextLine := strings.IndexByte(s[openerEnd:], '\n')
	if nextLine < 0 {
		return -1
	}
	lineStart := openerEnd + nextLine + 1
	for lineStart <= len(s) {
		cursor := lineStart
		for cursor < len(s) && cursor-lineStart < 3 && s[cursor] == ' ' {
			cursor++
		}
		runEnd := cursor
		for runEnd < len(s) && s[runEnd] == marker {
			runEnd++
		}
		if runEnd-cursor >= minimumRun {
			lineEnd := runEnd
			for lineEnd < len(s) && s[lineEnd] != '\n' {
				if s[lineEnd] != ' ' && s[lineEnd] != '\t' && s[lineEnd] != '\r' {
					break
				}
				lineEnd++
			}
			if lineEnd == len(s) || s[lineEnd] == '\n' {
				return lineEnd
			}
		}
		next := strings.IndexByte(s[lineStart:], '\n')
		if next < 0 {
			break
		}
		lineStart += next + 1
	}
	return -1
}

func findMatchingBacktickRun(s string, from, runLength int) int {
	for cursor := from; cursor < len(s); {
		found := strings.IndexByte(s[cursor:], '`')
		if found < 0 {
			return -1
		}
		start := cursor + found
		end := start + 1
		for end < len(s) && s[end] == '`' {
			end++
		}
		if end-start == runLength {
			return end
		}
		cursor = end
	}
	return -1
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

// stripDollarMath 剥 $…$ / $$…$$ 定界符：仅当内部含 LaTeX 特征或明确运算符才视为公式
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
		if isEscapedAt(s, i) {
			b.WriteByte('$')
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "$$") {
			if j := strings.Index(s[i+2:], "$$"); j >= 0 {
				inner := s[i+2 : i+2+j]
				if isDollarMath(inner) {
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
		if close := findInlineDollarClose(s, i+1); close > i+1 {
			inner := s[i+1 : close]
			if isDollarMath(inner) {
				b.WriteString(strings.TrimSpace(convertMath(inner, true)))
				i = close + 1
				continue
			}
		}
		b.WriteByte('$')
		i++
	}
	return b.String()
}

func findInlineDollarClose(s string, from int) int {
	lineEnd := len(s)
	if newline := strings.IndexByte(s[from:], '\n'); newline >= 0 {
		lineEnd = from + newline
	}
	for cursor := from; cursor < lineEnd; {
		offset := strings.IndexByte(s[cursor:lineEnd], '$')
		if offset < 0 {
			return -1
		}
		close := cursor + offset
		if isEscapedAt(s, close) {
			cursor = close + 1
			continue
		}
		// A closing delimiter cannot end after whitespace, and a dollar
		// immediately followed by a digit is another currency prefix.
		if close > from && !isASCIIWhitespace(s[close-1]) &&
			(close+1 >= len(s) || s[close+1] < '0' || s[close+1] > '9') {
			return close
		}
		return -1
	}
	return -1
}

func isEscapedAt(s string, index int) bool {
	backslashes := 0
	for index > 0 && s[index-1] == '\\' {
		backslashes++
		index--
	}
	return backslashes%2 == 1
}

func isASCIIWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func isDollarMath(inner string) bool {
	return latexHintRe.MatchString(inner) ||
		readableMathHintRe.MatchString(inner) ||
		numericMathRe.MatchString(inner) ||
		identifierMathRe.MatchString(inner)
}

// convertMath 对一段文本做确定性 LaTeX→Unicode 转换。
// math=true 表示已证实处于数学定界符内：未知命令剥反斜杠保留词干、裸下标 _2 也转换；
// math=false（定界符外）只按精确词表转换，未知命令原样保留。
func convertMath(s string, math bool) string {
	if math {
		s = doubleEscapedMathTokenRe.ReplaceAllStringFunc(s, func(token string) string {
			if !isKnownDoubleEscapedMathToken(token[2:]) {
				return token
			}
			return token[1:]
		})
	}
	if math && (latexRowBreakRe.MatchString(s) || strings.Contains(s, "&")) {
		s = normalizeMathLayout(s)
	}
	s = expandMathEnvironments(s)
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
			if name == "begin" || name == "end" {
				return m
			}
			return name // 兜底降级：剥反斜杠保留词干（仅数学定界符内）
		}
		return m
	})
	s = supBracedRe.ReplaceAllStringFunc(s, func(m string) string { return mapScript(m[2:len(m)-1], superscripts) })
	s = supBareRe.ReplaceAllStringFunc(s, func(m string) string { return mapScript(m[1:], superscripts) })
	s = subBracedRe.ReplaceAllStringFunc(s, func(m string) string { return mapScript(m[2:len(m)-1], subscripts) })
	if math {
		s = subBareRe.ReplaceAllStringFunc(s, func(m string) string { return mapScript(m[1:], subscripts) })
	} else {
		s = chemicalFormulaRe.ReplaceAllStringFunc(s, func(formula string) string {
			if !isSupportedBareChemicalFormula(formula) {
				return formula
			}
			return subBareRe.ReplaceAllStringFunc(formula, func(m string) string {
				return mapScript(m[1:], subscripts)
			})
		})
	}
	if math {
		s = strings.ReplaceAll(s, `\&`, "&")
	}
	return s
}

func isKnownDoubleEscapedMathToken(name string) bool {
	if len(name) == 1 && strings.ContainsRune("&,;:!", rune(name[0])) {
		return true
	}
	if _, ok := latexSymbols[name]; ok {
		return true
	}
	if name == "begin" || name == "end" || name == "circ" {
		return true
	}
	for _, command := range structCmds {
		if strings.TrimPrefix(command.name, `\`) == name {
			return true
		}
	}
	return false
}

func isSupportedBareChemicalFormula(formula string) bool {
	switch formula {
	case "H_2O", "CO_2", "Na_2CO_3":
		return true
	default:
		return false
	}
}

func stripMathAlignmentMarkers(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] == '\\' && i+1 < len(line) && line[i+1] == '&' {
			b.WriteString(`\&`)
			i += 2
			continue
		}
		if line[i] == '&' {
			if i+1 < len(line) && line[i+1] == '=' {
				b.WriteByte('=')
				i += 2
				continue
			}
			b.WriteByte(' ')
			i++
			continue
		}
		b.WriteByte(line[i])
		i++
	}
	return b.String()
}

type mathEnvironmentToken struct {
	start int
	end   int
	kind  string
	name  string
}

func nextMathEnvironmentToken(s string, from int) (mathEnvironmentToken, bool) {
	match := mathEnvironmentTokenRe.FindStringSubmatchIndex(s[from:])
	if match == nil {
		return mathEnvironmentToken{}, false
	}
	return mathEnvironmentToken{
		start: from + match[0],
		end:   from + match[1],
		kind:  s[from+match[2] : from+match[3]],
		name:  s[from+match[4] : from+match[5]],
	}, true
}

func matchingMathEnvironmentEnd(
	s string,
	begin mathEnvironmentToken,
) (mathEnvironmentToken, bool) {
	stack := []string{begin.name}
	cursor := begin.end
	for {
		token, ok := nextMathEnvironmentToken(s, cursor)
		if !ok {
			return mathEnvironmentToken{}, false
		}
		cursor = token.end
		if token.kind == "begin" {
			stack = append(stack, token.name)
			continue
		}
		if len(stack) == 0 || stack[len(stack)-1] != token.name {
			return mathEnvironmentToken{}, false
		}
		stack = stack[:len(stack)-1]
		if len(stack) == 0 {
			return token, true
		}
	}
}

// expandMathEnvironments converts only the body of balanced known display
// environments. Prose around a bare environment keeps math=false safety.
func expandMathEnvironments(s string) string {
	var b strings.Builder
	cursor := 0
	for cursor < len(s) {
		begin, ok := nextMathEnvironmentToken(s, cursor)
		if !ok {
			break
		}
		if begin.kind != "begin" {
			b.WriteString(s[cursor:begin.end])
			cursor = begin.end
			continue
		}
		end, ok := matchingMathEnvironmentEnd(s, begin)
		if !ok {
			break
		}
		b.WriteString(s[cursor:begin.start])
		body := s[begin.end:end.start]
		b.WriteString(convertMath(normalizeMathLayout(body), true))
		cursor = end.end
	}
	b.WriteString(s[cursor:])
	return b.String()
}

func normalizeMathLayout(s string) string {
	s = latexRowBreakRe.ReplaceAllString(s, "\n")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		line = stripMathAlignmentMarkers(line)
		lines[i] = strings.Join(strings.Fields(line), " ")
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
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
	{`\sqrt`, 1, true},
	{`\text`, 1, false}, {`\mathrm`, 1, false}, {`\mathbf`, 1, false},
	{`\mathit`, 1, false}, {`\mathsf`, 1, false}, {`\mathtt`, 1, false},
	{`\operatorname`, 1, false},
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
			if i > 0 && s[i-1] >= '0' && s[i-1] <= '9' &&
				(strings.HasPrefix(s[i:], `\frac`) || strings.HasPrefix(s[i:], `\dfrac`) || strings.HasPrefix(s[i:], `\tfrac`)) {
				b.WriteByte(' ')
			}
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
