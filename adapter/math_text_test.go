package adapter

import "testing"

// BUG-20260712-P（真机取证·微信机器人）：解题回复把 LaTeX 命令原样漏给纯文本 IM——
// 用户看到「( 4.5 \times 2 = 9 )」「( 4.5 \div 0.01 = 450 )」。桌面端有 KaTeX 渲染无感，
// 微信/企微/钉钉是纯文本，必须在**出站边界**做确定性降级（不靠 prompt 恳求模型改写法）。
func TestBug20260712_NormalizeMathText(t *testing.T) {
	cases := []struct{ in, want string }{
		// 真机原样取证（微信截图）
		{`( 4.5 \times 2 = 9 )`, `( 4.5 × 2 = 9 )`},
		{`( 4.5 \div 0.01 = 450 )`, `( 4.5 ÷ 0.01 = 450 )`},
		// 常用命令族
		{`a \cdot b \pm c`, `a · b ± c`},
		{`x \leq 3, y \geq 2, z \neq 1, w \approx 0.5`, `x ≤ 3, y ≥ 2, z ≠ 1, w ≈ 0.5`},
		{`\le \ge \ne`, `≤ ≥ ≠`},
		{`\sqrt{16} = 4`, `√16 = 4`},
		{`\frac{3}{4} + \frac{1}{4} = 1`, `3/4 + 1/4 = 1`},
		{`\pi r^2`, `π r²`},
		// 数学定界符剥除（内容保留）
		{`$2.8 \times 3.85$`, `2.8 × 3.85`},
		{`\(a+b\) 与 \[c-d\]`, `a+b 与 c-d`},
		// 上下标 → Unicode（x²、H₂O、10⁻³）
		{`x^2 + y^{2} = r^2`, `x² + y² = r²`},
		{`H_2O 与 CO_{2}`, `H₂O 与 CO₂`},
		{`水的化学式是 $H_2O$`, `水的化学式是 H₂O`},
		{`10^{-3} 和 2^{n}`, `10⁻³ 和 2ⁿ`},
		// \text{}/\mathrm{} 单位剥壳 + 细空格 + 上标（真机桌面「再练一道」原样取证）
		{`\( V = l \times w \times h \)`, `V = l × w × h`},
		{`\[ V = 6 \, \text{cm} \times 6 \, \text{cm} \]`, `V = 6 cm × 6 cm`},
		{`216 \text{cm}^3`, `216 cm³`},
		// 非数学文本零改动（含路径/代码里的反斜杠不误伤）
		{`C:\tmp\dir 和 $HOME 变量`, `C:\tmp\dir 和 $HOME 变量`},
		{`普通中文，不含任何公式。`, `普通中文，不含任何公式。`},
	}
	for _, c := range cases {
		if got := NormalizeMathText(c.in); got != c.want {
			t.Fatalf("NormalizeMathText(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

// BUG-20260712-Q（真机取证·钉钉）：只发作业图片不带文字 → 模型收到「裸图无指令」的
// 用户消息，自由发挥成自我介绍；用户被迫补一句「解题」才得到解答。
// 契约：图片-only 消息必须携带默认意图指令（文字 part），让模型直接处理图片内容。
func TestBug20260712_ImageOnlyMessageGetsDefaultInstruction(t *testing.T) {
	imgs := []Attachment{{Type: "image", Mime: "image/jpeg", Data: "Zm9v"}}

	m := BuildMultimodalUserMessage("", imgs)
	if len(m.MultiContent) < 2 {
		t.Fatalf("图片-only 消息应含 默认指令文字 part + 图片 part，got %d parts", len(m.MultiContent))
	}
	first := m.MultiContent[0]
	if first.Text == "" {
		t.Fatalf("首个 part 应为非空默认指令文字，got %+v", first)
	}

	// 用户带了文字 → 原样使用，绝不叠加默认指令
	m2 := BuildMultimodalUserMessage("帮我批改", imgs)
	if m2.MultiContent[0].Text != "帮我批改" {
		t.Fatalf("用户文字必须原样保留，got %q", m2.MultiContent[0].Text)
	}
	// 纯文字无图 → 不受影响
	m3 := BuildMultimodalUserMessage("你好", nil)
	if len(m3.MultiContent) != 1 || m3.MultiContent[0].Text != "你好" {
		t.Fatalf("纯文字消息零改动，got %+v", m3.MultiContent)
	}
}
