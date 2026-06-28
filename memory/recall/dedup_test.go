package recall

import (
	"testing"
	"time"
)

func TestDedupUpsert_InsertNovel(t *testing.T) {
	now := time.Now()
	existing := []Entry{{ID: "1", Type: TypeFact, Content: "用户用 Go 写后端"}}
	d := DedupUpsert(Entry{Type: TypeFact, Content: "用户喜欢吃辣"}, existing, now, DedupOptions{})
	if d.Action != ActionInsert {
		t.Fatalf("无关新事实应 insert，得 %s (%s)", d.Action, d.Reason)
	}
}

func TestDedupUpsert_DiscardExactDuplicate(t *testing.T) {
	now := time.Now()
	existing := []Entry{{ID: "1", Type: TypeFact, Content: "用户喜欢深色主题"}}
	d := DedupUpsert(Entry{Type: TypeFact, Content: "用户喜欢深色主题"}, existing, now, DedupOptions{})
	if d.Action != ActionDiscard {
		t.Fatalf("完全重复应 discard，得 %s", d.Action)
	}
}

func TestDedupUpsert_UpdateNewSuperset(t *testing.T) {
	now := time.Now()
	existing := []Entry{{ID: "1", Type: TypeFact, Content: "用户用 VSCode"}}
	// 新条包含旧条且更全 → 就地更新（保留旧 ID），修「只增不整合」。
	d := DedupUpsert(Entry{Type: TypeFact, Content: "用户用 VSCode 搭配 Vim 插件"}, existing, now, DedupOptions{})
	if d.Action != ActionUpdate || d.TargetID != "1" {
		t.Fatalf("新条是旧条超集应 update 旧条，得 %s target=%s", d.Action, d.TargetID)
	}
}

func TestDedupUpsert_UpdateSemanticOverlap(t *testing.T) {
	now := time.Now()
	existing := []Entry{{ID: "1", Type: TypeFact, Content: "用户 喜欢 简洁 的 代码 风格"}}
	d := DedupUpsert(Entry{Type: TypeFact, Content: "用户 喜欢 简洁 的 代码 命名"}, existing, now, DedupOptions{JaccardThreshold: 0.6})
	if d.Action != ActionUpdate || d.TargetID != "1" {
		t.Fatalf("高词集重叠应 update，得 %s target=%s (%s)", d.Action, d.TargetID, d.Reason)
	}
}

// G3 矛盾消解：同主语不同取值 → 时序取代（非覆盖；旧条留史）。
func TestDedupUpsert_SupersedeSameSubject(t *testing.T) {
	now := time.Now()
	existing := []Entry{{ID: "old", Type: TypeFact, Subject: "居住地", Content: "用户住北京"}}
	d := DedupUpsert(Entry{Type: TypeFact, Subject: "居住地", Content: "用户住上海"}, existing, now, DedupOptions{})
	if d.Action != ActionSupersede || d.TargetID != "old" {
		t.Fatalf("同主语新值应 supersede 旧条，得 %s target=%s", d.Action, d.TargetID)
	}
}

func TestDedupUpsert_SameSubjectIdenticalDiscard(t *testing.T) {
	now := time.Now()
	existing := []Entry{{ID: "old", Type: TypeFact, Subject: "居住地", Content: "用户住北京"}}
	d := DedupUpsert(Entry{Type: TypeFact, Subject: "居住地", Content: "用户住北京"}, existing, now, DedupOptions{})
	if d.Action != ActionDiscard {
		t.Fatalf("同主语同值应 discard，得 %s", d.Action)
	}
}

// 租户/角色隔离：跨 user 或跨 role 不视为重复 → insert（不串场）。
func TestDedupUpsert_TenantRoleIsolation(t *testing.T) {
	now := time.Now()
	existing := []Entry{{ID: "1", UserID: "u1", Role: "coder", Subject: "居住地", Content: "用户住北京"}}
	// 不同 user，同主语 → 不应取代 u1 的记忆。
	d := DedupUpsert(Entry{UserID: "u2", Role: "coder", Subject: "居住地", Content: "用户住上海"}, existing, now, DedupOptions{})
	if d.Action != ActionInsert {
		t.Fatalf("跨租户不应取代，应 insert，得 %s", d.Action)
	}
	// 同 user 不同 role → 也不串。
	d2 := DedupUpsert(Entry{UserID: "u1", Role: "writer", Subject: "居住地", Content: "用户住上海"}, existing, now, DedupOptions{})
	if d2.Action != ActionInsert {
		t.Fatalf("跨角色不应取代，应 insert，得 %s", d2.Action)
	}
}

func TestApplySupersede(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	old := Entry{ID: "old", Content: "用户住北京"}
	fresh := Entry{ID: "new", Content: "用户住上海"}
	ApplySupersede(&old, &fresh, now)
	if old.ValidTo == nil || !old.ValidTo.Equal(now) {
		t.Fatalf("旧条应被标失效于 now，得 %v", old.ValidTo)
	}
	if fresh.Supersedes != "old" || !fresh.ValidFrom.Equal(now) {
		t.Fatalf("新条应指回旧条并自 now 生效，得 supersedes=%s from=%v", fresh.Supersedes, fresh.ValidFrom)
	}
	if !IsCurrentlyValid(fresh, now) || IsCurrentlyValid(old, now) {
		t.Fatal("取代后：新条当前有效、旧条失效")
	}
}

func TestDedupUpsert_EmptyContentDiscard(t *testing.T) {
	d := DedupUpsert(Entry{Content: "   "}, nil, time.Now(), DedupOptions{})
	if d.Action != ActionDiscard {
		t.Fatalf("空内容应 discard，得 %s", d.Action)
	}
}
