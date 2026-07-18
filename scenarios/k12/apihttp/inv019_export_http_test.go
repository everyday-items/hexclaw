package apihttp_test

// K12-INV-019 HTTP 面钉住：GET /export（format=md/缺省）返回的 Markdown 对库内已规范化
// 内容原样保留、不做二次转换（入库口径裁决「存储即规范形」，engineadapter/solve_adapter.go）。
// `$` 货币哨兵：任何一轮 NormalizeMathText / channel.LaTeXToUnicode 都会把 $…$ 当数学
// 定界符剥掉——若导出链路被误加转换，哨兵即红。

import (
	"strings"
	"testing"
)

func TestINV019_ExportHTTPMarkdownPreservesCanonicalContent(t *testing.T) {
	h := newFaithfulServer(t)
	rec, out := do(t, h, "POST", "/record-mistake",
		`{"agent":"mingming","subject":"数学","grade":"五年级上",`+
			`"problem":"花了 $5 买笔，找回 $2，共 3.8 × 3 = ? 元",`+
			`"student_answer":"10.4",`+
			`"error_cause":"把 ½ 当成了 0.2，体积单位 cm³ 也写错",`+
			`"knowledge_points":["小数乘法"]}`)
	if rec.Code != 200 {
		t.Fatalf("/record-mistake 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if created, _ := out["record_created"].(bool); !created {
		t.Fatalf("错题应入库, got %v", out)
	}

	rec, exp := do(t, h, "GET", "/export?agent=mingming&format=md", "")
	if rec.Code != 200 || exp["format"] != "markdown" {
		t.Fatalf("/export format=md 应 200+markdown, got %d %v", rec.Code, exp["format"])
	}
	content, _ := exp["content"].(string)
	for _, want := range []string{
		"花了 $5 买笔，找回 $2，共 3.8 × 3 = ? 元", // $ 哨兵 + Unicode ×
		"把 ½ 当成了 0.2，体积单位 cm³ 也写错",       // Unicode ½ cm³
	} {
		if !strings.Contains(content, want) {
			t.Errorf("K12-INV-019 违规：/export markdown 未原样保留库内规范形内容。\n  want 子串: %q\n  content: %q", want, content)
		}
	}
}
