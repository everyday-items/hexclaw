package usecase

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// RED 合同：到期复习项必须携带学科与知识点（原型 app.html .kpill、架构图 10）。
// 当前 WeeklyPracticeItem 无该字段 → 编译失败即 RED；修复后 GREEN。
func TestBUG20260816006WeeklyDueItemCarriesSubjectAndKnowledgePoint(t *testing.T) {
	var item k12.WeeklyPracticeItem
	if item.Subject == "" && item.KnowledgePoint == "" {
		// 字段存在即可编译；此处仅证明字段可访问（填充逻辑由 usecase 测试覆盖）。
		t.Logf("fields addressable: subject=%q knowledge_point=%q", item.Subject, item.KnowledgePoint)
	}
}
