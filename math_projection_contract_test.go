package hexclaw_test

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/channel"
)

func TestIMMathProjectionEndpointsStayEquivalent(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		want        string
		allowRawTeX bool
	}{
		{
			name:  "mixed dfrac",
			input: `第一天修了 $2\dfrac{3}{4}$ 千米。`,
			want:  "第一天修了 2 3/4 千米。",
		},
		{
			name:  "mixed tfrac",
			input: `第二天多修了 $2\tfrac{3}{4}$ 千米。`,
			want:  "第二天多修了 2 3/4 千米。",
		},
		{
			name:  "plain dollar math",
			input: "验算 $2+2=4$。",
			want:  "验算 2+2=4。",
		},
		{
			name:  "numeric inline math",
			input: "小数是 $0.5$。",
			want:  "小数是 0.5。",
		},
		{
			name:  "numeric display math",
			input: "结果：$$42$$",
			want:  "结果：42",
		},
		{
			name:        "currency range is not a formula",
			input:       "价格区间 $5-$10",
			want:        "价格区间 $5-$10",
			allowRawTeX: true,
		},
		{
			name:        "separate currency prefixes are not paired",
			input:       "原价 $5 + 税，现价 $4",
			want:        "原价 $5 + 税，现价 $4",
			allowRawTeX: true,
		},
		{
			name:        "currency prefix before inline math stays literal",
			input:       "价格 $5，验算 $2+2=4$。",
			want:        "价格 $5，验算 2+2=4。",
			allowRawTeX: true,
		},
		{
			name:        "two currency prefixes before inline math stay literal",
			input:       "原价 $5，现价 $4，验算 $3+4=7$。",
			want:        "原价 $5，现价 $4，验算 3+4=7。",
			allowRawTeX: true,
		},
		{
			name:  "aligned display environment",
			input: "推导：\\[\\begin{aligned}2x + 1 &= 5 \\\\ 2x &= 4\\end{aligned}\\]",
			want:  "推导：2x + 1 = 5\n2x = 4",
		},
		{
			name:  "cases display environment",
			input: "分段：\\[\\begin{cases}x+1 & x>0 \\\\ 0 & x=0\\end{cases}\\]",
			want:  "分段：x+1 x>0\n0 x=0",
		},
		{
			name:  "nested display environments",
			input: "嵌套：\\[\\begin{aligned}f(x)&=\\begin{cases}x^2 & x>0 \\\\ 0 & x=0\\end{cases} \\\\ y&=1\\end{aligned}\\]",
			want:  "嵌套：f(x)=x² x>0\n0 x=0\ny=1",
		},
		{
			name:        "inline code",
			input:       "命令示例：`$2\\dfrac{3}{4}$`",
			want:        "命令示例：`$2\\dfrac{3}{4}$`",
			allowRawTeX: true,
		},
		{
			name:  "double backtick code",
			input: "代码：``x^2 与 `inner` ``",
			want:  "代码：``x^2 与 `inner` ``",
		},
		{
			name:        "fenced code",
			input:       "```tex\n$2\\tfrac{3}{4}$\n```",
			want:        "```tex\n$2\\tfrac{3}{4}$\n```",
			allowRawTeX: true,
		},
		{
			name:        "tilde fenced code",
			input:       "~~~tex\n$2\\frac{3}{4}$\n~~~",
			want:        "~~~tex\n$2\\frac{3}{4}$\n~~~",
			allowRawTeX: true,
		},
		{
			name:  "uppercase HTTPS URL",
			input: "见 HTTPS://example.com/x^2?a_1=b",
			want:  "见 HTTPS://example.com/x^2?a_1=b",
		},
		{
			name:  "uppercase identifiers stay literal",
			input: "API_2、HTTP_2、IMG_2.PNG",
			want:  "API_2、HTTP_2、IMG_2.PNG",
		},
		{
			name:  "element-shaped identifiers stay literal",
			input: "PCI_2、OS_2",
			want:  "PCI_2、OS_2",
		},
		{
			name:  "escaped ampersand stays literal",
			input: `品牌 $\text{AT\&T}$`,
			want:  "品牌 AT&T",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fromAdapter := adapter.NormalizeMathText(tc.input)
			fromChannel, _ := channel.LaTeXToUnicode(tc.input)
			if fromAdapter != fromChannel {
				t.Fatalf("IM math projection drifted:\nadapter=%q\nchannel=%q", fromAdapter, fromChannel)
			}
			if fromAdapter != tc.want {
				t.Fatalf("projection=%q, want %q", fromAdapter, tc.want)
			}
			if tc.allowRawTeX {
				return
			}
			for _, raw := range []string{`\frac`, `\dfrac`, `\tfrac`, "$"} {
				if strings.Contains(fromAdapter, raw) {
					t.Fatalf("raw TeX %q leaked into projection %q", raw, fromAdapter)
				}
			}
		})
	}
}
