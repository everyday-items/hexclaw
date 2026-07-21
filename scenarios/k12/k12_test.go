package k12

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// setup 建库 + 装配真实 K12 场景包，返回 records.Store 与六缝 registry。
func setup(t *testing.T) (*records.Store, *scenario.Registry) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatalf("insert agent: %v", err)
	}

	reg := scenario.NewRegistry()
	if err := reg.Assemble(Pack(NewCurriculumStub())); err != nil {
		t.Fatalf("装配 K12 场景包应成功: %v", err)
	}
	return records.NewStore(db, reg.Records), reg
}

// TestK12Pack_MistakeRoundTrip 端到端：真错题本 schema 走通用 records 存储。
func TestK12Pack_MistakeRoundTrip(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()

	// 写一道错题
	r, err := NewMistakeRecord("mingming", "sess-1", MistakeFields{
		Question: "3.8 × 3 = ?", KnowledgePoint: "小数乘法", ErrorCause: "计算失误", WrongProcess: "3.8×3 误算为 10.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Put(ctx, r)
	if err != nil || !created {
		t.Fatalf("首次入库应 created=true: created=%v err=%v", created, err)
	}
	if r.Status != StatusNew {
		t.Errorf("默认状态应 new, got %q", r.Status)
	}

	// 同题同会话（OCR 微差：空格）→ 去重（PRD：同题去重键）
	r2, _ := NewMistakeRecord("mingming", "sess-1", MistakeFields{Question: "3.8×3 = ?", KnowledgePoint: "小数乘法", ErrorCause: "计算失误"})
	created, _ = store.Put(ctx, r2)
	if created {
		t.Error("同题同会话应去重 created=false")
	}

	// 不同题 → 新记录
	r3, _ := NewMistakeRecord("mingming", "sess-1", MistakeFields{Question: "1/2 + 1/3 = ?", KnowledgePoint: "分数加减", ErrorCause: "概念不清"})
	created, _ = store.Put(ctx, r3)
	if !created {
		t.Error("不同题应新建 created=true")
	}

	// 错题本共 2 条
	list, _ := store.ListByScope(ctx, "mingming", CollectionMistakes, "")
	if len(list) != 2 {
		t.Fatalf("错题本应 2 条, got %d", len(list))
	}

	// 状态机推进：new → explained
	if err := store.UpdateStatus(ctx, r.RecordID, StatusExplained, nil, 0); err != nil {
		t.Fatalf("推进到 explained 应成功: %v", err)
	}
	// 非法状态被状态机拒绝
	if err := store.UpdateStatus(ctx, r.RecordID, "bogus", nil, 1); err == nil {
		t.Error("非法状态应被错题本状态机拒绝")
	}

	// 缺题干 → 字段校验失败
	bad, _ := NewMistakeRecord("mingming", "sess-9", MistakeFields{Question: "  "})
	if _, err := store.Put(ctx, bad); err == nil {
		t.Error("缺题干应校验失败")
	}
}

// TestK12Pack_GradeBoundary 年级边界正查/倒查（超纲检测基座）走通用约束缝。
func TestK12Pack_GradeBoundary(t *testing.T) {
	_, reg := setup(t)
	ctx := context.Background()
	cp, ok := reg.Constraints.Get("math")
	if !ok {
		t.Fatal("数学约束应已注册")
	}
	// 正查：五年级上已学知识点白名单
	allowed, _ := cp.Allowed(ctx, "五年级上")
	if !containsStr(allowed, "简易方程") {
		t.Errorf("五年级上应含简易方程, got %v", allowed)
	}
	// 倒查：解方程组首学初一下 → 五年级用它即超纲
	if g, ok := cp.FirstGrade(ctx, "解方程组"); !ok || g != "初一下" {
		t.Errorf("解方程组应首学初一下, got %q ok=%v", g, ok)
	}
}

// TestK12Pack_ModeFeature mode 特性词经注册表命中（engine 不认识"错题"）。
func TestK12Pack_ModeFeature(t *testing.T) {
	_, reg := setup(t)
	if m := reg.Modes.Match("看我家错题本这道题"); m != "mem-augmented" {
		t.Errorf("『错题本』应命中 mem-augmented, got %q", m)
	}
	if m := reg.Modes.Match("安排下周复习"); m != "plan-execute" {
		t.Errorf("『复习』应命中 plan-execute, got %q", m)
	}
	if !reg.Modes.MatchesMode("plan-execute", "该备考了") {
		t.Errorf("MatchesMode(plan-execute,备考) 应 true")
	}
	if b, ok := reg.Buttons.Get("expect_question_confirm"); !ok || b.Prompt != "是这道题吗？" {
		t.Errorf("识题确认按钮应已注册, got %+v ok=%v", b, ok)
	}
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// 回归锁（BUG-20260708）：message_badges 值须落在前端契约枚举 MessageBadgeKind 内（verify/record-chip），
// 防后端声明与 contracts/view-descriptor.ts 漂移（此前用 "verify-badge" 不在枚举内）。
func TestPack_MessageBadgesMatchContract(t *testing.T) {
	valid := map[string]bool{"verify": true, "record-chip": true}
	p := Pack(NewCurriculumStub())
	ve, ok := p.ViewExtensions["tutor"]
	if !ok {
		t.Fatal("缺 tutor 视图扩展")
	}
	for _, b := range ve.MessageBadges {
		if !valid[b] {
			t.Errorf("message_badge %q 不在前端契约枚举 {verify,record-chip} 内", b)
		}
	}
}

func TestPack_ComposerMatchesAuthoritativePrototype(t *testing.T) {
	ve, ok := Pack(NewCurriculumStub()).ViewExtensions["tutor"]
	if !ok {
		t.Fatal("缺 tutor 视图扩展")
	}
	if got, want := ve.ComposerPlaceholder, "发消息、粘贴带分数/公式的题目，或 ⌘V 粘贴作业照片"; got != want {
		t.Fatalf("composer placeholder = %q, want %q", got, want)
	}
	wantChips := []string{"📚 自动识别学科", "💡 渐进提示", "📷 识题校验"}
	if !reflect.DeepEqual(ve.ComposerChips, wantChips) {
		t.Fatalf("composer chips = %#v, want %#v", ve.ComposerChips, wantChips)
	}
}
