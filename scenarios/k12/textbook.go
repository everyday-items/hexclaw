package k12

// TextbookSubjects 分科教材绑定的学科口径（架构设计 §1.3 学科表 / §4.3：TextbookBinding
// 按 Learner × Subject 建立，不使用单一全局教材字段）。取值为当前六学科中文名；
// 空学科表示分科上线前的不分科旧语义（前向兼容），由调用方单独处理。
var TextbookSubjects = []string{"数学", "语文", "英语", "科学", "信息科技", "美术"}

// ValidTextbookSubject 报告 s 是否为合法的分科教材学科（不含空值语义）。
func ValidTextbookSubject(s string) bool {
	for _, v := range TextbookSubjects {
		if s == v {
			return true
		}
	}
	return false
}
