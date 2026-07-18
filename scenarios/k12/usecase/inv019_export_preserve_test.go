package usecase

// K12-INV-019「Markdown 导出保留 canonical 数学公式」（架构设计-v0.5.0 §7）——导出侧钉住：
// 入库口径裁决（2026-07-18）是「存储即规范形」（adapter 边界 Normalize，见
// engineadapter/solve_adapter.go），因此导出侧对已规范化内容**原样保留、不做二次转换**
//（现状 ExportMistakesMarkdown 不走 NormalizeMathText / LaTeXToUnicode——现状正确）。
//
// 断言方式：库内字段（含 Unicode 数学 × ÷ ½ cm³ 与合法 `$` 货币文本——任何一轮
// NormalizeMathText/LaTeXToUnicode 都会把 $…$ 当数学定界符剥掉）必须逐字节出现在导出
// Markdown 中。若未来有人给导出加了「二次转换」，$ 哨兵即红。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestINV019_ExportMarkdownPreservesCanonicalBytes(t *testing.T) {
	d, store := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	ctx := context.Background()

	const (
		question     = "花了 $5 买笔，找回 $2，共 3.8 × 3 = ? 元"
		errorCause   = "把 ½ 当成了 0.2，体积单位 cm³ 也写错"
		wrongProcess = "第二步 3.8 × 3 误算成 10.4，又用 6 ÷ 0.5 验算"
	)
	rec, err := k12.NewMistakeRecord("mingming", "s-inv019", k12.MistakeFields{
		Subject:         "数学",
		Question:        question,
		KnowledgePoint:  "小数乘法",
		ErrorCause:      errorCause,
		WrongProcess:    wrongProcess,
		CanonicalAnswer: "11.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}

	md, err := d.ExportMistakesMarkdown(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"题面（含 $ 货币哨兵）": question,
		"错因（含 ½ cm³）":  errorCause,
		"错误过程（含 × ÷）":  wrongProcess,
	} {
		if !strings.Contains(md, want) {
			t.Errorf("K12-INV-019 违规：导出 Markdown 未逐字节保留库内%s。\n  want 子串: %q\n  导出全文: %q", name, want, md)
		}
	}
}
