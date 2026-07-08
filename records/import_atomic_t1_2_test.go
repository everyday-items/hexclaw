package records

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// T1.2（hex-test 审计）：备份 Restore 逐条 ImportRecord，中途失败前面已导入的留库 = 部分导入
// （PRD §3.12.8「文件损坏明确报错不部分导入」的精神应贯彻到写库中途故障）。ImportRecords 须
// 单事务原子：任一条失败整批回滚，绝不留下半个档案。

func newFKStore(t *testing.T) *Store {
	t.Helper()
	// 显式开 foreign_keys，使 ghost agent_name 触发 FK 失败（模拟中途写库故障）。
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
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

func TestT1_2_ImportRecordsAtomic_RollbackOnMidBatchFailure(t *testing.T) {
	s := newFKStore(t)
	ctx := context.Background()
	good := &AgentRecord{RecordID: "g1", AgentName: "mingming", Collection: "ladder", Status: "new", SourceSession: "s"}
	bad := &AgentRecord{RecordID: "b1", AgentName: "ghost", Collection: "ladder", Status: "new"} // FK 违约

	err := s.ImportRecords(ctx, []*AgentRecord{good, bad})
	if err == nil {
		t.Fatal("含坏记录的批量导入应报错")
	}
	// 原子性：good 不得留库（整批回滚）。
	if _, e := s.Get(ctx, "g1"); !errors.Is(e, ErrNotFound) {
		t.Errorf("中途失败应整批回滚，good 记录不应存在, got err=%v", e)
	}
}

func TestT1_2_ImportRecordsAtomic_AllOrNothingSuccess(t *testing.T) {
	s := newFKStore(t)
	ctx := context.Background()
	recs := []*AgentRecord{
		{RecordID: "a1", AgentName: "mingming", Collection: "ladder", Status: "new", DedupeKey: "k1", SourceSession: "s1"},
		{RecordID: "a2", AgentName: "mingming", Collection: "ladder", Status: "retried", DedupeKey: "k2", SourceSession: "s2"},
	}
	if err := s.ImportRecords(ctx, recs); err != nil {
		t.Fatalf("全合法批量导入应成功: %v", err)
	}
	for _, id := range []string{"a1", "a2"} {
		if _, e := s.Get(ctx, id); e != nil {
			t.Errorf("记录 %s 应已导入, got %v", id, e)
		}
	}
}
