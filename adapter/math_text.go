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
	`\degree`, "°",
)

var (
	// \sqrt{16} / \sqrt 16 → √16
	reSqrt = regexp.MustCompile(`\\sqrt\s*\{([^{}]*)\}|\\sqrt\s+`)
	// \frac{a}{b} → a/b（仅一层花括号，嵌套罕见于 K12 场景，未命中原样保留）
	reFrac = regexp.MustCompile(`\\frac\s*\{([^{}]*)\}\s*\{([^{}]*)\}`)
	// 行内/行间数学定界符：$...$ / \(...\) / \[...\]（成对才剥，单个 $ 不动）
	reDollar  = regexp.MustCompile(`\$([^$\n]+)\$`)
	reParen   = regexp.MustCompile(`\\\(\s*(.*?)\s*\\\)`)
	reBracket = regexp.MustCompile(`\\\[\s*(.*?)\s*\\\]`)
)

// NormalizeMathText 把常见 LaTeX 数学写法降级为 Unicode 纯文本（幂等）。
// 供纯文本 IM 通道（wechat/wecom/dingtalk/…）在出站前调用；桌面/Markdown 通道不需要。
func NormalizeMathText(s string) string {
	if !strings.ContainsAny(s, `\$`) {
		return s // 快路径：无反斜杠无 $，零分配返回
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
	// 2. 符号命令替换
	out = latexSymbolReplacer.Replace(out)
	// 3. 剥成对数学定界符（内容保留）。放最后：内容已是 Unicode 符号。
	out = reParen.ReplaceAllString(out, `$1`)
	out = reBracket.ReplaceAllString(out, `$1`)
	// $...$ 仅当内含数学痕迹（数字/运算符/已转换符号）才剥，避免误伤 `$HOME 变量` 这类文本
	out = reDollar.ReplaceAllStringFunc(out, func(m string) string {
		inner := m[1 : len(m)-1]
		if strings.ContainsAny(inner, "0123456789×÷±≤≥≠≈√π=+-*/^") {
			return inner
		}
		return m
	})
	return out
}
