package usecase

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestAccumulation(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	// 复习型（错词）→ 待复习；积累型（好词好句）→ 已积累
	_, created, err := d.AddAccumulation(ctx, "mingming", "s1", k12.AccumFields{Subject: "英语", EntryType: "错词", Content: "necessary"})
	if err != nil || !created {
		t.Fatalf("错词应入库: %v %v", created, err)
	}
	d.AddAccumulation(ctx, "mingming", "s1", k12.AccumFields{Subject: "语文", EntryType: "好词好句", Content: "落霞与孤鹜齐飞"})

	// 学科过滤
	en, _ := d.ListAccumulation(ctx, "mingming", "英语")
	if len(en) != 1 || en[0].Record.Status != k12.AccumStatusReviewing {
		t.Fatalf("英语积累应 1 条·待复习, got %+v", en)
	}
	zh, _ := d.ListAccumulation(ctx, "mingming", "语文")
	if len(zh) != 1 || zh[0].Record.Status != k12.AccumStatusKept {
		t.Fatalf("语文好词好句应 1 条·已积累, got %+v", zh)
	}
	// 非法学科拒绝（数学不进积累本）
	if _, _, err := d.AddAccumulation(ctx, "mingming", "s1", k12.AccumFields{Subject: "数学", EntryType: "错词", Content: "x"}); err == nil {
		t.Error("数学不应进积累本")
	}
}

// TestBackupRestore_RoundTripFieldConsistency M4-1 DoD：导出→清库→导入，逐字段一致 + checksum。
func TestBackupRestore_RoundTripFieldConsistency(t *testing.T) {
	d, store1 := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	// 造跨记录集数据（错题 + 积累）
	seedMistake(t, d, "m1", "小数乘法", "计算失误", 500)
	d.AddAccumulation(ctx, "mingming", "s1", k12.AccumFields{Subject: "英语", EntryType: "错词", Content: "necessary"})

	// 备份
	bak, err := d.Backup(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if bak.Checksum == "" || len(bak.Records) != 2 {
		t.Fatalf("备份应含 2 条 + checksum, got %+v", bak)
	}

	// 篡改 checksum → 恢复应拒绝
	tampered := *bak
	tampered.Checksum = "deadbeef"
	if _, err := d.Restore(ctx, &tampered); err == nil {
		t.Error("checksum 不符应拒绝恢复")
	}

	// 新库（模拟清库/换机）→ 恢复
	store2 := freshStore(t)
	d2 := Deps{Records: store2}
	n, err := d2.Restore(ctx, bak)
	if err != nil || n != 2 {
		t.Fatalf("恢复应导入 2 条: n=%d err=%v", n, err)
	}

	// 逐字段一致：两库 ExportAgent 结果完全相同
	before, _ := store1.ExportAgent(ctx, "mingming")
	after, _ := store2.ExportAgent(ctx, "mingming")
	if len(before) != len(after) {
		t.Fatalf("恢复后条数不一致: %d vs %d", len(before), len(after))
	}
	for i := range before {
		a, b := before[i], after[i]
		if a.RecordID != b.RecordID || a.Collection != b.Collection || a.Status != b.Status ||
			a.Fields != b.Fields || a.DedupeKey != b.DedupeKey || a.Version != b.Version ||
			a.CreatedAt != b.CreatedAt || !eqDue(a.DueAt, b.DueAt) {
			t.Errorf("第 %d 条 round-trip 字段不一致:\n  before=%+v\n  after =%+v", i, a, b)
		}
	}
}

func TestExportMistakesMarkdown(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	seedMistake(t, d, "m1", "小数乘法", "计算失误", 500)
	md, err := d.ExportMistakesMarkdown(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "错题本导出") || !strings.Contains(md, "小数乘法") {
		t.Errorf("导出 Markdown 内容不符: %q", md)
	}
}

func eqDue(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// freshStore 建一个独立的空库 store（模拟换机恢复目标），注册 K12 schema。
func freshStore(t *testing.T) *k12storage.Store {
	t.Helper()
	db := openMigratedTestDB(t)
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	reg := scenario.NewRegistry()
	reg.Assemble(k12.Pack(k12.NewCurriculumStub()))
	return k12storage.NewStore(db, reg.Records)
}
