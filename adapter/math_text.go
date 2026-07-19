// math_text.go —— 纯文本 IM 通道的 LaTeX 数学降级（BUG-20260712-P）。
//
// 背景（真机取证·微信机器人解题）：模型输出含 LaTeX 数学（\times、\div、\frac…），
// 桌面端 KaTeX 渲染无感，微信/企微/钉钉等纯文本通道原样漏给用户
// （「( 4.5 \times 2 = 9 )」）。修法=在**出站边界**做确定性符号降级——
// 不靠 system prompt 恳求模型改写法（不可靠且污染所有通道的表达能力）。
//
// 设计边界：只转换「以反斜杠开头的已知数学命令 + 成对数学定界符」，
// 普通文本、Windows 路径（C:\tmp）、$ENV 变量不受影响（见测试的零改动组）。
package adapter

import (
	"regexp"
	"strings"
)

// latexSymbolReplacer 已知 LaTeX 命令 → Unicode 数学符号。
// 顺序无关（Replacer 最长匹配优先），仅收录 K12/日常算术高频集；
// 未收录命令原样保留（宁可少转不误转）。
var latexSymbolReplacer = strings.NewReplacer(
	`\times`, "×",
	`\div`, "÷",
	`\cdot`, "·",
	`\pm`, "±",
	`\mp`, "∓",
	`\leq`, "≤",
	`\geq`, "≥",
	`\neq`, "≠",
	`\le`, "≤",
	`\ge`, "≥",
	`\ne`, "≠",
	`\approx`, "≈",
	`\infty`, "∞",
	`\pi`, "π",
	`\alpha`, "α",
	`\beta`, "β",
	`\sum`, "∑",
	`\int`, "∫",
	`\left`, "",
	`\right`, "",
	`\degree`, "°",
)

var (
	// \sqrt{16} / \sqrt 16 → √16
	reSqrt = regexp.MustCompile(`\\sqrt\s*\{([^{}]*)\}|\\sqrt\s+`)
	// \frac{a}{b} → a/b（仅一层花括号，嵌套罕见于 K12 场景，未命中原样保留）
	reFrac = regexp.MustCompile(`\\frac\s*\{([^{}]*)\}\s*\{([^{}]*)\}`)
	// \text{cm} / \mathrm{...} / \operatorname{...} 排版壳 → 只留内容（单位 cm/km² 等）
	reMathWrap = regexp.MustCompile(`\\(?:text|mathrm|mathbf|mathit|mathsf|mathtt|operatorname)\s*\{([^{}]*)\}`)
	// LaTeX 细/负间距命令：\, \; \: \! （数字与单位间常见，如 6 \, cm）→ 归一为单空格
	reThinSpace = regexp.MustCompile(`\s*\\[,;:!]\s*`)
	// 上标 x^2 / x^{-3}；下标 H_2 / H_{2}。单字符形仅认数字（不误伤标识符/路径），花括号形认全表。
	reSuper = regexp.MustCompile(`\^(?:\{([^{}]*)\}|([0-9]))`)
	reSub   = regexp.MustCompile(`_(?:\{([^{}]*)\}|([0-9]))`)
	// 行内/行间数学定界符：$...$ / \(...\) / \[...\]（成对才剥，单个 $ 不动）
	reDollar  = regexp.MustCompile(`\$([^$\n]+)\$`)
	reParen   = regexp.MustCompile(`\\\(\s*(.*?)\s*\\\)`)
	reBracket = regexp.MustCompile(`\\\[\s*(.*?)\s*\\\]`)
	// Common display environments degrade to readable lines; braces are layout,
	// not mathematical facts. Unknown environments remain untouched and fail
	// closed at the channel manifest boundary.
	reBeginEnd = regexp.MustCompile(`\\(?:begin|end)\s*\{(?:aligned|align\*?|gathered|cases)\}`)
)

// 上/下标 → Unicode（K12 常见：数字、正负号、括号、幂指 n）。
var (
	superRunes = map[rune]rune{'0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶', '7': '⁷', '8': '⁸', '9': '⁹', '+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽', ')': '⁾', 'n': 'ⁿ'}
	subRunes   = map[rune]rune{'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆', '7': '₇', '8': '₈', '9': '₉', '+': '₊', '-': '₋', '=': '₌', '(': '₍', ')': '₎'}
)

// mapScript 把匹配到的上/下标内容逐字符映射为 Unicode；任一字符不可映射则整体原样保留（宁可少转不误转）。
func mapScript(match string, sub []string, table map[rune]rune) string {
	content := sub[1] // 花括号形
	if content == "" {
		content = sub[2] // 单字符数字形
	}
	var b strings.Builder
	for _, r := range content {
		t, ok := table[r]
		if !ok {
			return match
		}
		b.WriteRune(t)
	}
	return b.String()
}

// NormalizeMathText 把常见 LaTeX 数学写法降级为 Unicode 纯文本（幂等）。
// 供纯文本 IM 通道（wechat/wecom/dingtalk/…）在出站前调用；桌面/Markdown 通道不需要。
func NormalizeMathText(s string) string {
	if !strings.ContainsAny(s, `\$^_`) {
		return s // 快路径：无反斜杠/$/上下标记号，零分配返回
	}
	out := s
	// 1. 结构命令先展开（\frac/\sqrt 内部可能还有符号命令）
	out = reFrac.ReplaceAllString(out, `$1/$2`)
	out = reSqrt.ReplaceAllStringFunc(out, func(m string) string {
		if sub := reSqrt.FindStringSubmatch(m); len(sub) > 1 && sub[1] != "" {
			return "√" + sub[1]
		}
		return "√"
	})
	// 1b. 排版壳剥离（\text{cm}→cm）+ 细间距归一（6 \, cm→6 cm）——须在上标前，否则 \text{cm}^3 断层。
	out = reMathWrap.ReplaceAllString(out, `$1`)
	out = reThinSpace.ReplaceAllString(out, " ")
	// 1c. 上/下标 → Unicode（cm³、x²、H₂O、10⁻³）。
	out = reSuper.ReplaceAllStringFunc(out, func(m string) string {
		return mapScript(m, reSuper.FindStringSubmatch(m), superRunes)
	})
	out = reSub.ReplaceAllStringFunc(out, func(m string) string {
		return mapScript(m, reSub.FindStringSubmatch(m), subRunes)
	})
	// 2. 符号命令替换
	out = latexSymbolReplacer.Replace(out)
	// 3. 剥成对数学定界符（内容保留）。放最后：内容已是 Unicode 符号。
	out = reParen.ReplaceAllString(out, `$1`)
	out = reBracket.ReplaceAllString(out, `$1`)
	out = reBeginEnd.ReplaceAllString(out, "")
	out = strings.ReplaceAll(out, `\\`, "\n")
	// $...$ 仅当内含数学痕迹（数字/运算符/已转换符号）才剥，避免误伤 `$HOME 变量` 这类文本
	out = reDollar.ReplaceAllStringFunc(out, func(m string) string {
		inner := m[1 : len(m)-1]
		if strings.ContainsAny(inner, "0123456789⁰¹²³⁴⁵⁶⁷⁸⁹₀₁₂₃₄₅₆₇₈₉×÷±≤≥≠≈√π=+-*/^") {
			return inner
		}
		return m
	})
	return out
}
