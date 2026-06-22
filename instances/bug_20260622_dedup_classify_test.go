package instances

import (
	"errors"
	"testing"
)

// TestBug20260622_DedupOnlyOnUniqueViolation 回归锁定 [BUG-DEDUP-CONSTRAINT-SUBSTR]：
// recordEvent 此前用裸 strings.Contains(err, "constraint") 判定「重复事件」，
// 把 CHECK / FOREIGN KEY / NOT NULL 等约束错误也误判为重复 → seen=true → 真实平台消息
// 被静默丢弃。修复后仅 UNIQUE/主键冲突算重复，其它约束错误必须上抛。
func TestBug20260622_DedupOnlyOnUniqueViolation(t *testing.T) {
	// 非唯一键约束错误：必须 false（上抛，不可当重复丢弃）
	notDup := []error{
		errors.New("CHECK constraint failed: status"),
		errors.New("FOREIGN KEY constraint failed"),
		errors.New("NOT NULL constraint failed: platform_events.instance_name"),
		errors.New("some random db error"),
		nil,
	}
	for _, e := range notDup {
		if isDuplicateKeyErr(e) {
			t.Errorf("非唯一键错误被误判为重复 → 会静默丢消息: %v", e)
		}
	}

	// 真正的唯一键/主键冲突：必须 true（去重命中）
	dup := []error{
		errors.New("UNIQUE constraint failed: platform_events.instance_name, platform_events.event_id"),
		errors.New("Error 1062: Duplicate entry 'inst-x' for key 'PRIMARY'"),
		errors.New("pq: duplicate key value violates unique constraint \"platform_events_pkey\""),
	}
	for _, e := range dup {
		if !isDuplicateKeyErr(e) {
			t.Errorf("唯一键冲突应判为重复: %v", e)
		}
	}
}
