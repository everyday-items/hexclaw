package records

import (
	"context"
	"errors"
	"testing"
)

// TestBug_DedupReturnsExistingRecordID 钉死 bug：Put 去重命中(created=false)时，
// 必须把 r.RecordID 回填为**已存在记录**的 ID，而非一个从未入库的新 ID。
//
// 修复前：第二次 Put 返回的 RecordID 是新生成的 NanoID（未入库），Get 它会 ErrNotFound，
// 前端拿这个 id 去 mark-mastered/详情全部失败。
func TestBug_DedupReturnsExistingRecordID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	r1 := &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "sess-1"}
	created, err := s.Put(ctx, r1)
	if err != nil || !created {
		t.Fatalf("首次应 created=true: %v %v", created, err)
	}
	firstID := r1.RecordID

	// 同 dedupe 再写 → 去重命中
	r2 := &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "sess-1"}
	created, err = s.Put(ctx, r2)
	if err != nil {
		t.Fatalf("重复写不应报错: %v", err)
	}
	if created {
		t.Fatal("应去重 created=false")
	}

	// 关键断言：返回的 RecordID 必须 = 已存在记录的 ID，且可 Get 到
	if r2.RecordID != firstID {
		t.Errorf("去重后 RecordID 应回填为已存在记录 %q，got %q（指向不存在的行）", firstID, r2.RecordID)
	}
	if _, err := s.Get(ctx, r2.RecordID); errors.Is(err, ErrNotFound) {
		t.Errorf("去重返回的 RecordID %q 应能 Get 到实际记录", r2.RecordID)
	}
}
