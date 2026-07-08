package k12

// 年级学期 18 档有序枚举（PRD §5.2.2）。顺序即学习先后，用于超纲判定。
var gradeOrder = []string{
	"一年级上", "一年级下", "二年级上", "二年级下", "三年级上", "三年级下",
	"四年级上", "四年级下", "五年级上", "五年级下", "六年级上", "六年级下",
	"初一上", "初一下", "初二上", "初二下", "初三上", "初三下",
}

var gradeRank = func() map[string]int {
	m := make(map[string]int, len(gradeOrder))
	for i, g := range gradeOrder {
		m[g] = i
	}
	return m
}()

// GradeRank 返回年级档位的序号；未知年级返回 -1。
func GradeRank(grade string) int {
	if r, ok := gradeRank[grade]; ok {
		return r
	}
	return -1
}

// IsBeyond 判断 firstGrade（某知识点首学档位）是否**晚于**生效年级 grade，
// 即"用了还没学的知识" = 超纲。任一档位未知 → 返回 false（不误判超纲，交由 fail-visible 处理）。
func IsBeyond(grade, firstGrade string) bool {
	g, f := GradeRank(grade), GradeRank(firstGrade)
	if g < 0 || f < 0 {
		return false
	}
	return f > g
}

// NextGradeTerm 返回 current 学期的下一档（用于 3.1/9.1 学期确认提醒，PRD §3.6.4-5）。
// current 未知或已是最末档（初三下）→ ok=false（无下一学期，不推进）。
// 注意：只算出"建议值"，档案是否更新由家长确认，绝不自动推进（PRD 硬规则）。
func NextGradeTerm(current string) (string, bool) {
	r := GradeRank(current)
	if r < 0 || r+1 >= len(gradeOrder) {
		return "", false
	}
	return gradeOrder[r+1], true
}
