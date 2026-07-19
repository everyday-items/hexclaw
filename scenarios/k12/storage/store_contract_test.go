package k12storage_test

// 类型化存储不变量契约（§6.9 数据层替换、行为等价）：状态机转移校验、乐观锁、
// 幂等 dedupe、归属隔离、到期队列、导出/导入 round-trip——与 records.Store 时代
// 同一套语义，钉在 k12_* 类型化表上。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

func setup(t *testing.T) (*k12storage.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, a := range []string{"mingming", "lele"} {
		if _, err := db.Exec(`INSERT INTO agents(name) VALUES(?)`, a); err != nil {
			t.Fatalf("agent: %v", err)
		}
	}
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return k12storage.NewStore(db, reg.Records), db
}

func newMistake(t *testing.T, agent, session, question string) *records.AgentRecord {
	t.Helper()
	rec, err := k12.NewMistakeRecord(agent, session, k12.MistakeFields{
		Subject: "数学", Question: question, KnowledgePoint: "小数乘法",
		ErrorCause: "计算失误", EntrySource: k12.MistakeEntryPhoto,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// TestTyped_DedupeIdempotent 幂等 dedupe：同实例同题重复写入 → created=false 且回填既有 ID。
func TestTyped_DedupeIdempotent(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	r1 := newMistake(t, "mingming", "s1", "3.8×3=?")
	created, err := s.Put(ctx, r1)
	if err != nil || !created {
		t.Fatalf("首写应新建: created=%v err=%v", created, err)
	}
	r2 := newMistake(t, "mingming", "s1", "3.8 × 3 = ?") // OCR 微差，规范化后同题
	created, err = s.Put(ctx, r2)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("同题重复写入应去重命中")
	}
	if r2.RecordID != r1.RecordID {
		t.Fatalf("去重命中应回填既有 record_id：%s vs %s", r2.RecordID, r1.RecordID)
	}
	// 跨实例不去重（归属隔离）
	r3 := newMistake(t, "lele", "s1", "3.8×3=?")
	if created, err = s.Put(ctx, r3); err != nil || !created {
		t.Fatalf("跨实例同题应各自成立: created=%v err=%v", created, err)
	}
}

// TestTyped_StateMachine 状态机：合法阶梯放行、倒退/离开终态拒绝（ErrIllegalTransition）。
func TestTyped_StateMachine(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	r := newMistake(t, "mingming", "s1", "7+8=?")
	if _, err := s.Put(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, r.RecordID, k12.StatusExplained, nil, 0); err != nil {
		t.Fatalf("new→explained 应放行: %v", err)
	}
	if err := s.UpdateStatus(ctx, r.RecordID, k12.StatusNew, nil, 1); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("explained→new 倒退应拒绝, got %v", err)
	}
	if err := s.UpdateStatus(ctx, r.RecordID, k12.StatusArchived, nil, 1); err != nil {
		t.Fatalf("explained→archived 应放行: %v", err)
	}
	if err := s.UpdateStatus(ctx, r.RecordID, k12.StatusRetried, nil, 2); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("archived 终态出边应拒绝, got %v", err)
	}
	if err := s.UpdateStatus(ctx, r.RecordID, "不存在的状态", nil, 2); !errors.Is(err, records.ErrInvalidStatus) {
		t.Fatalf("非法状态应 ErrInvalidStatus, got %v", err)
	}
}

// TestTyped_OptimisticLock 乐观锁：过期 version CAS → ErrVersionConflict。
func TestTyped_OptimisticLock(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	r := newMistake(t, "mingming", "s1", "1+1=?")
	if _, err := s.Put(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, r.RecordID, k12.StatusExplained, nil, 0); err != nil {
		t.Fatal(err)
	}
	// 用过期 version=0 再推 → 冲突
	if err := s.UpdateStatus(ctx, r.RecordID, k12.StatusRetried, nil, 0); !errors.Is(err, records.ErrVersionConflict) {
		t.Fatalf("过期 version 应 ErrVersionConflict, got %v", err)
	}
	if err := s.UpdateStatus(ctx, r.RecordID, k12.StatusRetried, nil, 1); err != nil {
		t.Fatalf("正确 version 应成功: %v", err)
	}
	got, err := s.Get(ctx, r.RecordID)
	if err != nil || got.Version != 2 || got.Status != k12.StatusRetried {
		t.Fatalf("version 应推进到 2/retried, got %+v err=%v", got, err)
	}
}

// TestTyped_OwnershipIsolation 归属隔离：跨实例 Get 后 scoped 更新/删除按不存在处理；
// 未注册 agent 写入 → ErrScopeNotFound。
func TestTyped_OwnershipIsolation(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	r := newMistake(t, "mingming", "s1", "5×5=?")
	if _, err := s.Put(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatusScoped(ctx, "lele", r.RecordID, k12.StatusExplained, nil, 0); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("跨实例 scoped 更新应 ErrNotFound, got %v", err)
	}
	if err := s.Delete(ctx, "lele", r.RecordID); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("跨实例删除应 ErrNotFound, got %v", err)
	}
	if err := s.Delete(ctx, "mingming", r.RecordID); err != nil {
		t.Fatalf("本实例删除应成功: %v", err)
	}
	bad := newMistake(t, "查无此人", "s1", "6×6=?")
	if _, err := s.Put(ctx, bad); !errors.Is(err, records.ErrScopeNotFound) {
		t.Fatalf("未注册 agent 应 ErrScopeNotFound, got %v", err)
	}
}

// TestTyped_DueQueue 到期队列：due_at ≤ before 按到期升序返回；清 due 移出队列。
func TestTyped_DueQueue(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	due1, due2 := int64(100), int64(50)
	r1 := newMistake(t, "mingming", "s1", "题一")
	r1.DueAt = &due1
	r2 := newMistake(t, "mingming", "s1", "题二")
	r2.DueAt = &due2
	r3 := newMistake(t, "mingming", "s1", "题三") // 无 due
	for _, r := range []*records.AgentRecord{r1, r2, r3} {
		if _, err := s.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListDue(ctx, "mingming", k12.CollectionMistakes, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RecordID != r2.RecordID || got[1].RecordID != r1.RecordID {
		t.Fatalf("到期队列应按 due 升序 [题二,题一], got %d 条", len(got))
	}
	// 清 due → 移出队列
	if err := s.UpdateStatus(ctx, r2.RecordID, k12.StatusExplained, nil, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListDue(ctx, "mingming", k12.CollectionMistakes, 200)
	if len(got) != 1 || got[0].RecordID != r1.RecordID {
		t.Fatalf("清 due 后队列应只剩题一, got %d 条", len(got))
	}
}

// TestTyped_AggregateChildrenRoundTrip 练习集/作品聚合子表：写→读 Fields JSON 语义一致
// （items/versions 顺序、复批结论 result_correct 三态、feedback_skill 溯源戳）。
func TestTyped_AggregateChildrenRoundTrip(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	tru := true
	setRec, err := k12.NewPracticeSetRecord("mingming", "s1", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly, Title: "本周复习卷",
		ReturnAssets: []k12.PracticeReturnAsset{{
			ReturnID: "return-1", AssetID: "asset://mingming/return.png",
			ItemIDs: []string{"item-one"}, ReturnedAt: 1701,
		}},
		Items: []k12.PracticeItem{
			{ItemID: "item-one", Subject: "数学", AddedVia: k12.PracticeAddedViaWeekly, QuestionMarkdown: "3.8×3=?",
				ExpectedAnswerMarkdown: "11.4", VerificationStatus: k12.PracticeItemVerified,
				VerificationEvidence: "独立验算", PaperSeq: 1, Returned: true, ResultCorrect: &tru,
				PracticeProblemID: "pp-1", GenerationJobID: "pgen-1", VariantIndex: 2,
				RequestedDifficulty: "harder", ActualDifficulty: "harder"},
			{Subject: "语文", AddedVia: k12.PracticeAddedViaCustom, QuestionMarkdown: "默写：静夜思",
				ExpectedAnswerMarkdown: "床前明月光…", VerificationStatus: k12.PracticeItemPending},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, setRec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, setRec.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := k12.ParsePracticeSetFields(got.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Items) != 2 || f.Items[0].QuestionMarkdown != "3.8×3=?" || f.Items[1].Subject != "语文" {
		t.Fatalf("items 顺序/内容应 round-trip: %+v", f.Items)
	}
	if f.Items[0].ResultCorrect == nil || !*f.Items[0].ResultCorrect {
		t.Fatal("result_correct=true 应 round-trip")
	}
	if f.Items[1].ResultCorrect != nil {
		t.Fatal("无结论项 result_correct 应保持 nil")
	}
	if !f.Items[0].Returned || f.Items[0].PracticeProblemID != "pp-1" {
		t.Fatalf("returned/practice_problem_id 应 round-trip: %+v", f.Items[0])
	}
	if f.Items[0].GenerationJobID != "pgen-1" || f.Items[0].VariantIndex != 2 || f.Items[0].ActualDifficulty != "harder" {
		t.Fatalf("组卷来源/难度应 round-trip: %+v", f.Items[0])
	}
	if len(f.ReturnAssets) != 1 || f.ReturnAssets[0].ReturnID != "return-1" || f.ReturnAssets[0].ItemIDs[0] != "item-one" {
		t.Fatalf("return_assets 应 round-trip: %+v", f.ReturnAssets)
	}

	workRec, err := k12.NewCreativeWorkRecord("mingming", "s1", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "太空画", Task: "想象画",
		Versions: []k12.CreativeWorkVersion{
			{SourceAssetID: "asset://mingming/a.png", Feedback: "构图完整", FeedbackSource: "ai",
				FeedbackSkill: "art-feedback@1.0.0/disk", PracticeCardDoneAt: 1700},
			{ContentMarkdown: "修改稿"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, workRec); err != nil {
		t.Fatal(err)
	}
	wgot, err := s.Get(ctx, workRec.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := k12.ParseCreativeWorkFields(wgot.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(wf.Versions) != 2 || wf.Versions[0].FeedbackSkill != "art-feedback@1.0.0/disk" ||
		wf.Versions[0].PracticeCardDoneAt != 1700 || wf.Versions[1].Feedback != "" {
		t.Fatalf("作品版本/feedback 应 round-trip: %+v", wf.Versions)
	}
}

// TestTyped_ReturnAssetsAppendOnly DD-028：普通 fields 更新即使漏带旧批次也不得删除；
// 同 return_id 改 asset/item 映射必须让外层 CAS 事务整体回滚。
func TestTyped_ReturnAssetsAppendOnly(t *testing.T) {
	s, db := setup(t)
	ctx := context.Background()
	rec, err := k12.NewPracticeSetRecord("mingming", "s", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual, Title: "只追加回传卷",
		Items:        []k12.PracticeItem{{ItemID: "q1", QuestionMarkdown: "题", VerificationStatus: k12.PracticeItemPending}},
		ReturnAssets: []k12.PracticeReturnAsset{{ReturnID: "r1", AssetID: "asset://mingming/a.png", ItemIDs: []string{"q1"}, ReturnedAt: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, rec.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := k12.ParsePracticeSetFields(got.Fields)
	if !f.Items[0].Returned {
		t.Fatal("returned 必须可由 append-only return_assets 恢复，不能依赖可漂移的旧布尔投影")
	}
	// 模拟旧调用方漏带 return_assets：更新成功，但 append-only 子表不得被清空。
	f.ReturnAssets = nil
	f.Items[0].Returned = false
	raw, _ := json.Marshal(f)
	if err := s.UpdateStatusFields(ctx, rec.RecordID, got.Status, got.DueAt, string(raw), got.Version); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, rec.RecordID)
	f, _ = k12.ParsePracticeSetFields(got.Fields)
	if len(f.ReturnAssets) != 1 || f.ReturnAssets[0].ReturnID != "r1" {
		t.Fatalf("漏带字段不得删除旧批次: %+v", f.ReturnAssets)
	}
	if !f.Items[0].Returned {
		t.Fatal("旧调用方写回 false 也不得抹掉由 return_assets 证明的 returned 投影")
	}

	// 改写旧 return_id 必须失败，且题目更新也一并回滚。
	f.Title = "不应提交的标题"
	f.ReturnAssets[0].AssetID = "asset://mingming/other.png"
	raw, _ = json.Marshal(f)
	if err := s.UpdateStatusFields(ctx, rec.RecordID, got.Status, got.DueAt, string(raw), got.Version); err == nil {
		t.Fatal("同 return_id 改写载荷却成功")
	}
	got, _ = s.Get(ctx, rec.RecordID)
	f, _ = k12.ParsePracticeSetFields(got.Fields)
	if f.Title == "不应提交的标题" || f.ReturnAssets[0].AssetID != "asset://mingming/a.png" {
		t.Fatalf("冲突更新必须整事务回滚: %+v", f)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_practice_return_assets WHERE set_record_id=?`, rec.RecordID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("append-only 表应恰有一条: n=%d err=%v", n, err)
	}
}

// TestTyped_ExportImportRoundTrip 导出→替换导入 round-trip 逐字段一致（备份恢复底座）。
func TestTyped_ExportImportRoundTrip(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	due := int64(4242)
	m := newMistake(t, "mingming", "s1", "圆的周长公式?")
	m.DueAt = &due
	if _, err := s.Put(ctx, m); err != nil {
		t.Fatal(err)
	}
	setRec, _ := k12.NewPracticeSetRecord("mingming", "s2", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual, Title: "手工卷",
		Items: []k12.PracticeItem{{ItemID: "backup-q1", Subject: "数学", QuestionMarkdown: "9×9=?", ExpectedAnswerMarkdown: "81",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算", Returned: true}},
		ReturnAssets: []k12.PracticeReturnAsset{{ReturnID: "backup-return-1", AssetID: "asset://mingming/backup.png",
			ItemIDs: []string{"backup-q1"}, ReturnedAt: 4243}},
	})
	if _, err := s.Put(ctx, setRec); err != nil {
		t.Fatal(err)
	}

	exported, err := s.ExportAgent(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(exported) != 2 {
		t.Fatalf("应导出 2 条, got %d", len(exported))
	}

	// 替换导入到另一空库 → 再导出应逐字段一致
	s2, _ := setup(t)
	if err := s2.ReplaceAgentRecords(ctx, "mingming", exported); err != nil {
		t.Fatal(err)
	}
	roundTripped, err := s2.ExportAgent(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(roundTripped) != len(exported) {
		t.Fatalf("round-trip 条数不一致: %d vs %d", len(roundTripped), len(exported))
	}
	for i := range exported {
		a, b := exported[i], roundTripped[i]
		if a.RecordID != b.RecordID || a.Collection != b.Collection || a.Status != b.Status ||
			a.Fields != b.Fields || a.DedupeKey != b.DedupeKey || a.Version != b.Version ||
			a.CreatedAt != b.CreatedAt || a.UpdatedAt != b.UpdatedAt ||
			(a.DueAt == nil) != (b.DueAt == nil) || (a.DueAt != nil && *a.DueAt != *b.DueAt) ||
			a.SourceSession != b.SourceSession || a.Tags != b.Tags || a.SchemaVersion != b.SchemaVersion {
			t.Fatalf("记录 #%d round-trip 不一致:\n导出: %+v\n回读: %+v", i, a, b)
		}
	}
}
