package engine

import "testing"

// Bug 2026-07-18（真实环境回归矩阵 B1 拍照批改发现）：
// 照片识别产出的题干自带题号前缀（如「1. 26×3=」）。确定性算式快路径
// normalizeTrivialArithmetic 去空白后把「1. 26*3」误拼成小数「1.26*3」，
// 得出 3.78 并以 numeric_exec/verified-strong 判正确作答为错——
// 全卷 4 题全部被错误判错、4 条伪错题入库（§5.4 零容忍：确定性证据必须真确定）。
//
// 契约：
//  1. 题号列表前缀（数字 + [.、．)）] + 空白）必须剥离后再求值；
//  2. 真小数题（无空白分隔，如「1.26×3=」）不受影响，仍按小数计算。
func TestBug20260718_ItemNumberPrefixNotDecimal(t *testing.T) {
	cases := []struct {
		problem string
		answer  string
	}{
		{"1. 26×3=", "78"},
		{"2. 144÷12=", "12"},
		{"3. 57+38=", "95"},
		{"4. 200-76=", "124"},
		{"12. 7+8=", "15"},
		{"3、 57+38=", "95"},
		{"3) 57+38=", "95"},
	}
	for _, tt := range cases {
		_, got, ok := solveTrivialArithmetic(tt.problem)
		if !ok || got != tt.answer {
			t.Errorf("solveTrivialArithmetic(%q) = %q,%v want %q,true（题号前缀被误当小数）", tt.problem, got, ok, tt.answer)
		}
	}
}

// 真小数题不受剥离影响：无空白分隔的「1.26×3」是真正的小数乘法。
func TestBug20260718_RealDecimalStillWorks(t *testing.T) {
	_, got, ok := solveTrivialArithmetic("1.26×3=")
	if !ok || got != "3.78" {
		t.Fatalf("solveTrivialArithmetic(1.26×3=) = %q,%v want 3.78,true（真小数不得被当题号剥掉）", got, ok)
	}
}
