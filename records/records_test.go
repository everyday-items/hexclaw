package records

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// mockSchema 一个领域中性的记录集 schema（模拟场景包注册进来的东西）。
// 去重键 = 来源会话；状态机 new → done。records 包本身不认识它，靠注册拿到。
func mockSchema() *RecordSchema {
	return &RecordSchema{
		Collection:    "notes",
		Version:       1,
		InitialStatus: "new",
		Statuses:      []string{"new", "done"},
		DedupeKey:     func(r *AgentRecord) string { return r.SourceSession },
	}
}

// newTestStore 建内存库、跑迁移、插两个实例、注册 mockSchema。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// agent_records.agent_name FK → agents(name)；插两个实例（多孩隔离对象）
	for _, name := range []string{"mingming", "honghong"} {
		if _, err := db.Exec(`INSERT INTO agents(name) VALUES(?)`, name); err != nil {
			t.Fatalf("insert agent %s: %v", name, err)
		}
	}
	reg := NewRecordSchemaRegistry()
	if err := reg.Register(mockSchema()); err != nil {
		t.Fatalf("register schema: %v", err)
	}
	return NewStore(db, reg)
}

func TestRegistry_Validation(t *testing.T) {
	reg := NewRecordSchemaRegistry()
	// InitialStatus 不在 Statuses → 拒绝
	err := reg.Register(&RecordSchema{Collection: "x", Statuses: []string{"a"}, InitialStatus: "z", DedupeKey: func(*AgentRecord) string { return "" }})
	if err == nil {
		t.Fatal("InitialStatus 不在 Statuses 应报错")
	}
	// 正常注册
	if err := reg.Register(mockSchema()); err != nil {
		t.Fatalf("正常注册应成功: %v", err)
	}
	// 重复注册 → 拒绝
	if err := reg.Register(mockSchema()); err == nil {
		t.Fatal("重复注册应报错")
	}
	// 未注册 collection → ErrUnknownCollection
	if _, err := reg.Get("nope"); !errors.Is(err, ErrUnknownCollection) {
		t.Fatalf("未注册应返回 ErrUnknownCollection, got %v", err)
	}
}

func TestPut_IdempotentDedup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	r1 := &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "sess-1", Fields: `{"q":"3.8x3"}`}
	created, err := s.Put(ctx, r1)
	if err != nil || !created {
		t.Fatalf("首次写入应 created=true, got created=%v err=%v", created, err)
	}
	// 同实例+记录集+同来源会话（dedupe_key 相同）→ 幂等跳过
	r2 := &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "sess-1", Fields: `{"q":"3.8x3 again"}`}
	created, err = s.Put(ctx, r2)
	if err != nil {
		t.Fatalf("重复写入不应报错: %v", err)
	}
	if created {
		t.Fatal("同 dedupe_key 重复写入应 created=false（幂等去重）")
	}
	got, err := s.ListByScope(ctx, "mingming", "notes", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("去重后应只有 1 条, got %d", len(got))
	}
	if got[0].Status != "new" {
		t.Errorf("默认状态应为 InitialStatus=new, got %q", got[0].Status)
	}
	if got[0].SchemaVersion != 1 {
		t.Errorf("schema_version 应为 1, got %d", got[0].SchemaVersion)
	}
}

func TestListByScope_AgentIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, &AgentRecord{AgentName: "honghong", Collection: "notes", SourceSession: "h1"}); err != nil {
		t.Fatal(err)
	}
	// 小明只能看到小明的记录（零串库）
	ming, _ := s.ListByScope(ctx, "mingming", "notes", "")
	if len(ming) != 1 || ming[0].SourceSession != "m1" {
		t.Fatalf("小明作用域应只含 m1, got %+v", ming)
	}
	hong, _ := s.ListByScope(ctx, "honghong", "notes", "")
	if len(hong) != 1 || hong[0].SourceSession != "h1" {
		t.Fatalf("小红作用域应只含 h1, got %+v", hong)
	}
}

func TestListDue_ReviewQueue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	due100 := int64(100)
	due300 := int64(300)
	// 两条有到期(100,300)，一条无到期
	mustPut(t, s, &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "a", DueAt: &due300})
	mustPut(t, s, &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "b", DueAt: &due100})
	mustPut(t, s, &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "c"}) // 无 due

	// before=200：应只出 due<=200 的（b，due=100），且不含无 due 的 c
	q, err := s.ListDue(ctx, "mingming", "notes", 200)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	if len(q) != 1 || q[0].SourceSession != "b" {
		t.Fatalf("before=200 应只出 b(due=100), got %+v", q)
	}
	// before=500：出 b(100),a(300) 且按 due 升序
	q, _ = s.ListDue(ctx, "mingming", "notes", 500)
	if len(q) != 2 || q[0].SourceSession != "b" || q[1].SourceSession != "a" {
		t.Fatalf("before=500 应按到期升序 [b,a], got %+v", q)
	}
}

func TestUpdateStatus_OptimisticLock(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r := &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "x"}
	mustPut(t, s, r)

	// 非法状态 → 拒绝
	if err := s.UpdateStatus(ctx, r.RecordID, "bogus", nil, 0); !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("非法状态应 ErrInvalidStatus, got %v", err)
	}
	// 正确 version=0 → 推进到 done
	if err := s.UpdateStatus(ctx, r.RecordID, "done", nil, 0); err != nil {
		t.Fatalf("合法推进应成功: %v", err)
	}
	// 再用旧 version=0 → 冲突（已被推进到 version=1）
	if err := s.UpdateStatus(ctx, r.RecordID, "new", nil, 0); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("陈旧 version 应 ErrVersionConflict, got %v", err)
	}
	got, _ := s.Get(ctx, r.RecordID)
	if got.Status != "done" || got.Version != 1 {
		t.Fatalf("状态应 done/version1, got %s/%d", got.Status, got.Version)
	}
}

func TestPut_UnknownCollection(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Put(context.Background(), &AgentRecord{AgentName: "mingming", Collection: "ghost", SourceSession: "z"})
	if !errors.Is(err, ErrUnknownCollection) {
		t.Fatalf("未注册记录集应 ErrUnknownCollection, got %v", err)
	}
}

func mustPut(t *testing.T, s *Store, r *AgentRecord) {
	t.Helper()
	if _, err := s.Put(context.Background(), r); err != nil {
		t.Fatalf("put %s: %v", r.SourceSession, err)
	}
}
