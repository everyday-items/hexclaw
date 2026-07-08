package records

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// T1.1（hex-test 审计）：RecordSchema 声明 Statuses（节点集）但无转移边，UpdateStatus 只校验
// 目标 status ∈ 全集 → 任意态→任意态放行（含倒退 mastered→retried、离开终态 archived→*）。
// 应按 schema 声明的 Transitions 偏序校验：前进/同态/归档允许、倒退/离开终态禁止；未声明
// Transitions 的 schema（如积累本）保持不校验（向后兼容）。

// ladderSchema 带转移偏序的测试 schema（模拟错题本状态机）。
func ladderSchema() *RecordSchema {
	return &RecordSchema{
		Collection:    "ladder",
		Version:       1,
		InitialStatus: "new",
		Statuses:      []string{"new", "explained", "retried", "mastered", "archived"},
		Transitions: map[string][]string{
			"new":       {"explained", "retried", "mastered", "archived"},
			"explained": {"retried", "mastered", "archived"},
			"retried":   {"mastered", "archived"},
			"mastered":  {"archived"},
			"archived":  {},
		},
		DedupeKey: func(r *AgentRecord) string { return r.SourceSession },
	}
}

func newLadderStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	reg := NewRecordSchemaRegistry()
	if err := reg.Register(ladderSchema()); err != nil {
		t.Fatal(err)
	}
	return NewStore(db, reg)
}

func seedLadder(t *testing.T, s *Store, status string) *AgentRecord {
	t.Helper()
	r := &AgentRecord{RecordID: "r-" + status, AgentName: "mingming", Collection: "ladder", Status: status, SourceSession: status}
	if _, err := s.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestT1_1_StatusTransition_ForwardAndSameAllowed(t *testing.T) {
	s := newLadderStore(t)
	ctx := context.Background()
	// 前进 new→retried。
	r := seedLadder(t, s, "new")
	if err := s.UpdateStatus(ctx, r.RecordID, "retried", nil, r.Version); err != nil {
		t.Errorf("前进 new→retried 应允许, got %v", err)
	}
	// 同态 retried→retried（幂等重做）。
	cur, _ := s.Get(ctx, r.RecordID)
	if err := s.UpdateStatus(ctx, r.RecordID, "retried", nil, cur.Version); err != nil {
		t.Errorf("同态 retried→retried 应允许, got %v", err)
	}
	// 手动「他会了」new→mastered（跨阶梯手动 override 合法）。
	r2 := seedLadder(t, s, "new")
	// 用不同 record 造 new 态；上面 seedLadder 复用了 record_id，这里换来源。
	r2.RecordID, r2.SourceSession = "r-new2", "new2"
	if _, err := s.Put(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, r2.RecordID, "mastered", nil, r2.Version); err != nil {
		t.Errorf("手动 new→mastered 应允许, got %v", err)
	}
}

func TestT1_1_StatusTransition_BackwardAndTerminalRejected(t *testing.T) {
	s := newLadderStore(t)
	ctx := context.Background()
	// 倒退 mastered→retried 应拒。
	m := seedLadder(t, s, "mastered")
	if err := s.UpdateStatus(ctx, m.RecordID, "retried", nil, m.Version); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("倒退 mastered→retried 应 ErrIllegalTransition, got %v", err)
	}
	// 离开终态 archived→new 应拒。
	a := seedLadder(t, s, "archived")
	if err := s.UpdateStatus(ctx, a.RecordID, "new", nil, a.Version); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("离开终态 archived→new 应 ErrIllegalTransition, got %v", err)
	}
	// 倒退 explained→new 应拒。
	e := seedLadder(t, s, "explained")
	if err := s.UpdateStatus(ctx, e.RecordID, "new", nil, e.Version); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("倒退 explained→new 应 ErrIllegalTransition, got %v", err)
	}
}

// 未声明 Transitions 的 schema 保持不校验（向后兼容 mockSchema 等）。
func TestT1_1_StatusTransition_NilTransitionsBackwardCompat(t *testing.T) {
	s := newTestStore(t) // mockSchema：Statuses[new,done]，无 Transitions
	ctx := context.Background()
	r := &AgentRecord{RecordID: "bc1", AgentName: "mingming", Collection: "notes", Status: "done", SourceSession: "s"}
	if _, err := s.Put(ctx, r); err != nil {
		t.Fatal(err)
	}
	// done→new 若强制偏序会被拒；无 Transitions 声明时应放行（不破坏存量记录集）。
	if err := s.UpdateStatus(ctx, r.RecordID, "new", nil, r.Version); err != nil {
		t.Errorf("无 Transitions 声明的记录集应不校验转移, got %v", err)
	}
}
