package k12

// 孩子档案存 agents.metadata（map[string]string）。K12 键 namespace 化（AP-1：平台不 typed K12 字段）。
const (
	MetaKeyChildName = "k12.child_name"
	MetaKeyGradeTerm = "k12.grade_term"
	MetaKeyTextbook  = "k12.textbook_edition"
)

// ChildProfile 孩子辅导基准档案（PRD §5.2.2）。
type ChildProfile struct {
	ChildName       string `json:"child_name"`
	GradeTerm       string `json:"grade_term"`
	TextbookEdition string `json:"textbook_edition"`
}

// ProfileFromMeta 从实例 metadata 读出孩子档案。
func ProfileFromMeta(meta map[string]string) ChildProfile {
	return ChildProfile{
		ChildName:       meta[MetaKeyChildName],
		GradeTerm:       meta[MetaKeyGradeTerm],
		TextbookEdition: meta[MetaKeyTextbook],
	}
}

// ApplyProfileToMeta 把档案字段写回 metadata 的**克隆**（只改 K12 键，保留其他 metadata）。
// 传入的 meta 不被修改（避免别名污染 router 内部 map）。空字段不覆盖。
func ApplyProfileToMeta(meta map[string]string, p ChildProfile) map[string]string {
	clone := make(map[string]string, len(meta)+3)
	for k, v := range meta {
		clone[k] = v
	}
	if p.ChildName != "" {
		clone[MetaKeyChildName] = p.ChildName
	}
	if p.GradeTerm != "" {
		clone[MetaKeyGradeTerm] = p.GradeTerm
	}
	if p.TextbookEdition != "" {
		clone[MetaKeyTextbook] = p.TextbookEdition
	}
	return clone
}

// ValidGradeTerm 校验年级学期是否 18 档之一。
func ValidGradeTerm(g string) bool { return GradeRank(g) >= 0 }
