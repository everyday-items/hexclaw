package k12

import "strings"

// 孩子档案存 agents.metadata（map[string]string）。K12 键 namespace 化（AP-1：平台不 typed K12 字段）。
const (
	MetaKeyChildName                     = "k12.child_name"
	MetaKeyGradeTerm                     = "k12.grade_term"
	MetaKeyTextbook                      = "k12.textbook_edition"
	MetaKeyTextbookMath                  = "k12.textbook_edition.math"
	MetaKeyTextbookChinese               = "k12.textbook_edition.chinese"
	MetaKeyTextbookEnglish               = "k12.textbook_edition.english"
	MetaKeyTextbookScience               = "k12.textbook_edition.science"
	MetaKeyTextbookInformationTechnology = "k12.textbook_edition.information_technology"
	MetaKeyTextbookArt                   = "k12.textbook_edition.art"
)

// SubjectTextbooks is the canonical six-subject textbook map.
type SubjectTextbooks struct {
	Math                  string `json:"math"`
	Chinese               string `json:"chinese"`
	English               string `json:"english"`
	Science               string `json:"science"`
	InformationTechnology string `json:"information_technology"`
	Art                   string `json:"art"`
}

// NormalizeSubjectTextbooks trims all values and reports whether the canonical
// six-subject exact set is complete.
func NormalizeSubjectTextbooks(in SubjectTextbooks) (SubjectTextbooks, bool) {
	out := SubjectTextbooks{
		Math:                  strings.TrimSpace(in.Math),
		Chinese:               strings.TrimSpace(in.Chinese),
		English:               strings.TrimSpace(in.English),
		Science:               strings.TrimSpace(in.Science),
		InformationTechnology: strings.TrimSpace(in.InformationTechnology),
		Art:                   strings.TrimSpace(in.Art),
	}
	return out, out.Math != "" && out.Chinese != "" && out.English != "" &&
		out.Science != "" && out.InformationTechnology != "" && out.Art != ""
}

// ChildProfile 孩子辅导基准档案（PRD §5.2.2）。
type ChildProfile struct {
	ChildName        string           `json:"child_name"`
	GradeTerm        string           `json:"grade_term"`
	SubjectTextbooks SubjectTextbooks `json:"subject_textbooks"`
	TextbookEdition  string           `json:"textbook_edition"`
}

// ProfileFromMeta 从实例 metadata 读出孩子档案。
func ProfileFromMeta(meta map[string]string) ChildProfile {
	textbooks := SubjectTextbooks{
		Math:                  strings.TrimSpace(meta[MetaKeyTextbookMath]),
		Chinese:               strings.TrimSpace(meta[MetaKeyTextbookChinese]),
		English:               strings.TrimSpace(meta[MetaKeyTextbookEnglish]),
		Science:               strings.TrimSpace(meta[MetaKeyTextbookScience]),
		InformationTechnology: strings.TrimSpace(meta[MetaKeyTextbookInformationTechnology]),
		Art:                   strings.TrimSpace(meta[MetaKeyTextbookArt]),
	}
	if textbooks.Math == "" {
		textbooks.Math = strings.TrimSpace(meta[MetaKeyTextbook])
	}
	return ChildProfile{
		ChildName:        meta[MetaKeyChildName],
		GradeTerm:        meta[MetaKeyGradeTerm],
		SubjectTextbooks: textbooks,
		TextbookEdition:  textbooks.Math,
	}
}

// ApplyProfileToMeta 把档案字段写回 metadata 的**克隆**（只改 K12 键，保留其他 metadata）。
// 传入的 meta 不被修改（避免别名污染 router 内部 map）。空字段不覆盖。
func ApplyProfileToMeta(meta map[string]string, p ChildProfile) map[string]string {
	clone := make(map[string]string, len(meta)+9)
	for k, v := range meta {
		clone[k] = v
	}
	if value := strings.TrimSpace(p.ChildName); value != "" {
		clone[MetaKeyChildName] = value
	}
	if value := strings.TrimSpace(p.GradeTerm); value != "" {
		clone[MetaKeyGradeTerm] = value
	}
	textbooks, _ := NormalizeSubjectTextbooks(p.SubjectTextbooks)
	if textbooks.Math == "" {
		textbooks.Math = strings.TrimSpace(p.TextbookEdition)
	}
	for key, value := range map[string]string{
		MetaKeyTextbookMath:                  textbooks.Math,
		MetaKeyTextbookChinese:               textbooks.Chinese,
		MetaKeyTextbookEnglish:               textbooks.English,
		MetaKeyTextbookScience:               textbooks.Science,
		MetaKeyTextbookInformationTechnology: textbooks.InformationTechnology,
		MetaKeyTextbookArt:                   textbooks.Art,
	} {
		if value != "" {
			clone[key] = value
		}
	}
	math := strings.TrimSpace(clone[MetaKeyTextbookMath])
	if math == "" {
		math = strings.TrimSpace(clone[MetaKeyTextbook])
	}
	if math != "" {
		clone[MetaKeyTextbookMath] = math
		clone[MetaKeyTextbook] = math
	}
	return clone
}

// ReplaceProfileInMeta exact-replaces the K12 profile namespace while preserving
// unrelated agent metadata. A nil profile means the archived state had no K12
// profile and therefore clears all canonical and compatibility profile keys.
func ReplaceProfileInMeta(meta map[string]string, p *ChildProfile) map[string]string {
	clone := make(map[string]string, len(meta)+9)
	for k, v := range meta {
		switch k {
		case MetaKeyChildName, MetaKeyGradeTerm, MetaKeyTextbook,
			MetaKeyTextbookMath, MetaKeyTextbookChinese, MetaKeyTextbookEnglish,
			MetaKeyTextbookScience, MetaKeyTextbookInformationTechnology,
			MetaKeyTextbookArt:
			continue
		default:
			clone[k] = v
		}
	}
	if p == nil {
		return clone
	}
	return ApplyProfileToMeta(clone, *p)
}

// ValidGradeTerm 校验年级学期是否 18 档之一。
func ValidGradeTerm(g string) bool { return GradeRank(g) >= 0 }
