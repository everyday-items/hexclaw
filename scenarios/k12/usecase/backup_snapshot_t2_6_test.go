package usecase_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type memProfileStore struct{ m map[string]k12.ChildProfile }

func (s *memProfileStore) GetProfile(_ context.Context, a string) (k12.ChildProfile, error) {
	p, ok := s.m[a]
	if !ok {
		return k12.ChildProfile{}, records.ErrNotFound
	}
	return p, nil
}
func (s *memProfileStore) SaveProfile(_ context.Context, a string, p k12.ChildProfile) error {
	s.m[a] = p
	return nil
}

func newBackupDeps(t *testing.T) (usecase.Deps, *memProfileStore) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('xiaoming')`)
	cur := curriculum.New()
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(cur)); err != nil {
		t.Fatal(err)
	}
	ps := &memProfileStore{m: map[string]k12.ChildProfile{}}
	return usecase.Deps{
		Solver: toggleSolver{}, Grader: &toggleGrader{}, Records: records.NewStore(db, reg.Records),
		Profiles: ps, Constraint: cur, Now: func() int64 { return 1000 },
	}, ps
}

// T2.6a（PRD §3.12.4-1）：全量导出应含孩子档案（不止 records）。
func TestT2_6_BackupIncludesProfile(t *testing.T) {
	ctx := context.Background()
	d, ps := newBackupDeps(t)
	ps.m["xiaoming"] = k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上", TextbookEdition: "人教版"}

	bak, err := d.Backup(ctx, "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	if bak.Profile == nil || bak.Profile.GradeTerm != "五年级上" {
		t.Fatalf("导出应含档案（含年级）, got %+v", bak.Profile)
	}

	// 清档案 → Restore 应还原。
	ps.m = map[string]k12.ChildProfile{}
	if _, err := d.Restore(ctx, bak); err != nil {
		t.Fatal(err)
	}
	if ps.m["xiaoming"].GradeTerm != "五年级上" {
		t.Errorf("Restore 应还原档案, got %+v", ps.m["xiaoming"])
	}
}

// T2.6b（PRD §3.12.9）：恢复前自动做一次快照，便于回退。
func TestT2_6_RestoreWithSnapshotCapturesPreState(t *testing.T) {
	ctx := context.Background()
	d, _ := newBackupDeps(t)
	// 造既有记录（恢复前状态）。
	pre := &records.AgentRecord{
		RecordID: "old1", AgentName: "xiaoming", Collection: k12.CollectionMistakes,
		Status: k12.StatusNew, Fields: `{"question":"旧题","knowledge_point":"小数乘法"}`, DedupeKey: "d-old",
	}
	if err := d.Records.ImportRecords(ctx, []*records.AgentRecord{pre}); err != nil {
		t.Fatal(err)
	}
	// 用一个空档案覆盖恢复。
	incoming, _ := d.Backup(ctx, "xiaoming") // 当前快照作为 incoming（含 old1）
	_, snapshot, err := d.RestoreWithSnapshot(ctx, incoming)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot == nil || len(snapshot.Records) != 1 || snapshot.Records[0].RecordID != "old1" {
		t.Errorf("操作前快照应含恢复前的 old1 记录, got %+v", snapshot)
	}
}
