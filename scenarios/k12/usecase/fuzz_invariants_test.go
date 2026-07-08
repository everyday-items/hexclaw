package usecase

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 方法1 不变量/property：纯函数对任意输入不 panic + 关键不变量成立。
func FuzzLooksLikeGhostwrite(f *testing.F) {
	f.Add("")
	f.Add("提纲：先想想")
	f.Add(strings.Repeat("字", 300))
	f.Fuzz(func(t *testing.T, s string) {
		out := LooksLikeGhostwrite(s) // 不变量：不 panic
		// 含引导词 → 恒不判代写。
		if strings.Contains(s, "提纲") && out {
			t.Errorf("含「提纲」不应判代写: %q", s)
		}
		// 空串 → 恒 false。
		if strings.TrimSpace(s) == "" && out {
			t.Errorf("空串不应判代写")
		}
	})
}

func FuzzNextGradeTerm(f *testing.F) {
	f.Add("五年级上")
	f.Add("")
	f.Add("初三下")
	f.Fuzz(func(t *testing.T, g string) {
		next, ok := k12.NextGradeTerm(g) // 不变量：不 panic
		if ok {
			// 不变量：下一学期档位恰好 = 当前 +1。
			if k12.GradeRank(next) != k12.GradeRank(g)+1 {
				t.Errorf("NextGradeTerm(%q)=%q 档位不连续", g, next)
			}
		}
	})
}
