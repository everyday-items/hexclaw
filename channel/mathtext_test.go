package channel

// LaTeX→Unicode 确定性兜底转换的契约测试（IM 呈现适配归通道层，见 mathtext.go 归属申报）。
// 硬约束：常见公式全映射；嵌套 \frac 至少一层；混合文本只动公式；幂等；
// 无 LaTeX 零改动；代码块/行内代码/URL 不误伤。

import "testing"

func TestLaTeXToUnicode_CommonMappings(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"times", `3 \times 4`, "3 × 4"},
		{"div", `12 \div 3`, "12 ÷ 3"},
		{"cdot", `a \cdot b`, "a · b"},
		{"pm", `\pm 2`, "± 2"},
		{"leq", `x \leq 5`, "x ≤ 5"},
		{"le", `x \le 5`, "x ≤ 5"},
		{"geq", `x \geq 5`, "x ≥ 5"},
		{"ge", `x \ge 5`, "x ≥ 5"},
		{"neq", `a \neq b`, "a ≠ b"},
		{"ne", `a \ne b`, "a ≠ b"},
		{"approx", `\approx 3.14`, "≈ 3.14"},
		{"pi", `\pi`, "π"},
		{"infty", `\infty`, "∞"},
		{"degree", `45\degree`, "45°"},
		{"circ-sup", `45^\circ`, "45°"},
		{"circ-sup-braced", `45^{\circ}`, "45°"},
		{"sqrt", `\sqrt{16}`, "√16"},
		{"sqrt-expr", `\sqrt{a+b}`, "√(a+b)"},
		{"sqrt-bare", `\sqrt2`, "√2"},
		{"frac", `\frac{1}{2}`, "1/2"},
		{"frac-bare", `\frac12`, "1/2"},
		{"frac-expr", `\frac{a+b}{2}`, "(a+b)/2"},
		{"mixed-number", `$2\frac{3}{4}$`, "2 3/4"},
		{"sup-braced", `x^{2}`, "x²"},
		{"sup-bare", `x^2`, "x²"},
		{"sup-multi", `2^10`, "2¹⁰"},
		{"sup-neg", `10^{-3}`, "10⁻³"},
		{"sub-braced", `H_{2}O`, "H₂O"},
		{"sub-bare-in-math", `$a_2 + a_3$`, "a₂ + a₃"},
		{"chemical-sub-bare", `Na_2CO_3`, "Na₂CO₃"},
		{"text", `\text{厘米}`, "厘米"},
		{"mathrm", `\mathrm{kg}`, "kg"},
		{"spacing-unit", `$12 \, \mathrm{cm}$`, "12 cm"},
		{"dollar-inline", `$\pi r^2$`, "π r²"},
		{"paren-delim", `\(a \times b\)`, "a × b"},
		{"bracket-delim", `\[x^2 + y^2 = z^2\]`, "x² + y² = z²"},
		{"display-dollar", `$$\frac{1}{2}$$`, "1/2"},
		{"left-right", `\left(\frac{1}{2}\right)^2`, "(1/2)²"},
		{"unknown-cmd-in-math-strips-backslash", `$x \to \infty$`, "x to ∞"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := LaTeXToUnicode(tc.in)
			if got != tc.want {
				t.Fatalf("LaTeXToUnicode(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !changed {
				t.Fatalf("LaTeXToUnicode(%q) 应报告 changed=true", tc.in)
			}
		})
	}
}

func TestLaTeXToUnicode_NestedFrac(t *testing.T) {
	cases := []struct{ in, want string }{
		{`\frac{\frac{1}{2}}{3}`, "(1/2)/3"},
		{`\frac{1}{\frac{3}{4}}`, "1/(3/4)"},
	}
	for _, tc := range cases {
		if got, _ := LaTeXToUnicode(tc.in); got != tc.want {
			t.Fatalf("嵌套 \\frac 至少降级一层: LaTeXToUnicode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLaTeXToUnicode_MixedTextOnlyTouchesFormula(t *testing.T) {
	in := "第 1 题：$\\frac{3}{4} \\times 8 = 6$，做对了；注意 5^2 = 25。"
	want := "第 1 题：3/4 × 8 = 6，做对了；注意 5² = 25。"
	got, changed := LaTeXToUnicode(in)
	if got != want {
		t.Fatalf("混合文本只动公式部分:\n got  %q\n want %q", got, want)
	}
	if !changed {
		t.Fatal("应报告 changed=true")
	}
}

func TestREGBUGK12C02StandaloneVariable003_StandaloneMathIdentifier(t *testing.T) {
	in := "设 $x$ 为边长，面积记为 $S$；价格仍写作 $5。"
	want := "设 x 为边长，面积记为 S；价格仍写作 $5。"
	got, changed := LaTeXToUnicode(in)
	if got != want || !changed {
		t.Fatalf("standalone math identifier projection: got=%q changed=%v want=%q", got, changed, want)
	}
}

func TestLaTeXToUnicode_Idempotent(t *testing.T) {
	inputs := []string{
		`3 \times 4`, `\frac{\frac{1}{2}}{3}`, `$\pi r^2$`, `\sqrt{a+b}`,
		"第 1 题：$\\frac{3}{4} \\times 8 = 6$，注意 5^2 = 25。",
		`$x \to \infty$`, `45^{\circ}`, `H_{2}O`, "普通中文，无任何公式。",
	}
	for _, in := range inputs {
		once, _ := LaTeXToUnicode(in)
		twice, changed := LaTeXToUnicode(once)
		if twice != once {
			t.Fatalf("转换必须幂等: f(%q)=%q, f(f)=%q", in, once, twice)
		}
		if changed {
			t.Fatalf("第二次转换必须报告 changed=false: %q", once)
		}
	}
}

func TestLaTeXToUnicode_NoLaTeXZeroChange(t *testing.T) {
	inputs := []string{
		"今天的作业完成得很棒，继续保持！",
		"路径 C:\\Users\\新建文件夹\\note.txt 已保存",
		"价格是 $5，总共 $10 元",
		"markdown _强调_ 和 file_2.txt 不该变",
		"5 + 3 = 8，100 ÷ 4 = 25，x² 已是 Unicode",
	}
	for _, in := range inputs {
		got, changed := LaTeXToUnicode(in)
		if got != in || changed {
			t.Fatalf("无 LaTeX 文本必须零改动: in=%q got=%q changed=%v", in, got, changed)
		}
	}
}

func TestLaTeXToUnicode_CodeAndURLUntouched(t *testing.T) {
	inputs := []string{
		"运行 `sum = $a^2$` 试试",
		"```\n\\frac{1}{2} 代码块里保持原样 x^2\n```",
		"见 https://example.com/x^2?a_1=b 这个链接",
	}
	for _, in := range inputs {
		got, changed := LaTeXToUnicode(in)
		if got != in || changed {
			t.Fatalf("代码块/行内代码/URL 不得误伤: in=%q got=%q changed=%v", in, got, changed)
		}
	}
}

func TestLaTeXToUnicode_DoubleEscapedMathKeepsMarkdownStructure(t *testing.T) {
	in := "## 解题步骤\n\n1. **列式**：$\\\\frac{3}{4} \\\\times 8 = 6$\n2. **验算**：$6 \\\\div 8 = \\\\frac{3}{4}$"
	want := "## 解题步骤\n\n1. **列式**：3/4 × 8 = 6\n2. **验算**：6 ÷ 8 = 3/4"
	got, changed := LaTeXToUnicode(in)
	if got != want {
		t.Fatalf("双重转义数学只应降级公式并保留 Markdown:\n got  %q\n want %q", got, want)
	}
	if !changed {
		t.Fatal("双重转义数学应报告 changed=true")
	}
}

func TestLaTeXToUnicode_DoubleBackslashBeforeUnknownWordRemainsRowBreak(t *testing.T) {
	in := "$a \\\\x + b = c$"
	want := "a\nx + b = c"
	got, changed := LaTeXToUnicode(in)
	if got != want {
		t.Fatalf("未知字母前的双反斜杠必须保持 TeX 换行语义:\n got  %q\n want %q", got, want)
	}
	if !changed {
		t.Fatal("数学换行归一化应报告 changed=true")
	}
}
