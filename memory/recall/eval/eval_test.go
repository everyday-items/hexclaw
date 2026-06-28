package eval

import (
	"strings"
	"testing"
)

// 召回评测护城河（方案 §G1）：全部用例必须绿 —— 这是「召得准、灌得净」的可证明依据。
// 任何三维打分权重/minScore/dedup 改动若伤准确性，此处即 RED。
func TestRecallEvalSuite(t *testing.T) {
	rep := RunSuite(Scenarios())
	if rep.PassRate() < 1.0 {
		t.Fatalf("召回评测未全绿 (%d/%d)：\n%s",
			rep.Passed(), rep.Total(), strings.Join(rep.Failures(), "\n"))
	}
	t.Logf("召回评测全绿：%d/%d，覆盖类别 %v",
		rep.Passed(), rep.Total(), SortedClasses(Scenarios()))
}

// 覆盖面护栏：至少覆盖 LongMemEval 6 类 + LoCoMo 2 类（不串场/误召），共 ≥8 类。
func TestEvalCoverageBreadth(t *testing.T) {
	classes := SortedClasses(Scenarios())
	if len(classes) < 8 {
		t.Fatalf("评测类别覆盖不足，仅 %d 类：%v", len(classes), classes)
	}
	wantPrefixes := []string{"longmemeval/", "locomo/"}
	for _, p := range wantPrefixes {
		found := false
		for _, c := range classes {
			if strings.HasPrefix(c, p) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("缺少 %s 类用例", p)
		}
	}
}
