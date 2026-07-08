package scenario

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
)

// mockConstraint 一个领域中性的约束 provider（模拟档位白名单的最小形态）。
type mockConstraint struct {
	allowed map[string][]string // grade -> keys
	first   map[string]string   // key -> grade
}

func (m *mockConstraint) Allowed(_ context.Context, grade string) ([]string, error) {
	return m.allowed[grade], nil
}
func (m *mockConstraint) FirstGrade(_ context.Context, key string) (string, bool) {
	g, ok := m.first[key]
	return g, ok
}

// mockPack 一个纯 mock 的场景包，覆盖全部六缝——用来证明「加一个场景包不改内核」。
func mockPack() *Pack {
	return &Pack{
		Name: "mock",
		RecordSchemas: []*records.RecordSchema{{
			Collection:    "mock-notes",
			Version:       1,
			InitialStatus: "new",
			Statuses:      []string{"new", "done"},
			DedupeKey:     func(r *records.AgentRecord) string { return r.SourceSession },
		}},
		Constraints: map[string]ConstraintProvider{
			"mock-domain": &mockConstraint{
				allowed: map[string][]string{"g1": {"k1", "k2"}},
				first:   map[string]string{"k9": "g9"},
			},
		},
		ViewExtensions: map[string]ViewExtension{
			"mock-view": {HeaderTabs: []string{"a", "b"}, ComposerPlaceholder: "hi", SchemaVersion: 1},
		},
		ModeFeatures: []ModeFeature{{Mode: "mock-mode", Keywords: []string{"kw-alpha", "kw-beta"}}},
		Buttons:      []ButtonSpec{{TriggerKey: "expect_mock", Prompt: "?", Labels: []string{"是", "否"}, Actions: []string{"y", "n"}}},
		EvalSuites:   []string{"eval/mock"},
	}
}

// TestAssemble_MockPack_AllSeams 是 AP-1 的可证伪验收：
// 一个 mock 场景包，不改 engine/records/router/chat shell，Assemble 后六缝各挂上一项。
func TestAssemble_MockPack_AllSeams(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Assemble(mockPack()); err != nil {
		t.Fatalf("装配 mock 场景包应成功: %v", err)
	}

	// 1) 记录集
	if _, err := reg.Records.Get("mock-notes"); err != nil {
		t.Errorf("记录集未挂上: %v", err)
	}
	// 2) 约束（正查 + 倒查）
	cp, ok := reg.Constraints.Get("mock-domain")
	if !ok {
		t.Fatal("约束未挂上")
	}
	if got, _ := cp.Allowed(context.Background(), "g1"); len(got) != 2 {
		t.Errorf("正查 g1 应得 2 个 key, got %v", got)
	}
	if g, ok := cp.FirstGrade(context.Background(), "k9"); !ok || g != "g9" {
		t.Errorf("倒查 k9 应得 g9, got %q ok=%v", g, ok)
	}
	// 3) 视图槽
	if ve, ok := reg.Views.Get("mock-view"); !ok || len(ve.HeaderTabs) != 2 {
		t.Errorf("视图槽未挂上或字段丢失: %+v", ve)
	}
	// 4) mode 特性（关键词命中）
	if m := reg.Modes.Match("这里包含 kw-alpha 触发词"); m != "mock-mode" {
		t.Errorf("mode 关键词 kw-alpha 应命中 mock-mode, got %q", m)
	}
	if m := reg.Modes.Match("今天天气不错"); m != "" {
		t.Errorf("无关键词不应命中任何 mode, got %q", m)
	}
	// 5) 按钮
	if b, ok := reg.Buttons.Get("expect_mock"); !ok || len(b.Labels) != 2 {
		t.Errorf("按钮未挂上或字段丢失: %+v", b)
	}
	// 6) eval 套件
	if dirs := reg.Evals.Dirs(); len(dirs) != 1 || dirs[0] != "eval/mock" {
		t.Errorf("eval 目录未登记: %v", dirs)
	}
}

// TestAssemble_EmptyPlatformIsClean 反向门（AP-1）：不 Assemble 任何 pack，
// 平台六缝全空 = 干净通用形态（对应「删 scenarios/k12 后平台仍干净」）。
func TestAssemble_EmptyPlatformIsClean(t *testing.T) {
	reg := NewRegistry()
	if cols := reg.Records.Collections(); len(cols) != 0 {
		t.Errorf("空平台不应有任何记录集, got %v", cols)
	}
	if _, ok := reg.Constraints.Get("mock-domain"); ok {
		t.Error("空平台不应有约束")
	}
	if m := reg.Modes.Match("kw-alpha kw-beta 任意词"); m != "" {
		t.Errorf("空平台不应命中任何 mode（无 pack 注册特性词）, got %q", m)
	}
	if len(reg.Evals.Dirs()) != 0 {
		t.Error("空平台不应有 eval 目录")
	}
}

// TestAssemble_MissingName 缺 Name 的 pack 应被拒（装配器唯一的结构性校验）。
func TestAssemble_MissingName(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Assemble(&Pack{}); err == nil {
		t.Fatal("缺 Name 应报错")
	}
}

// TestAssemble_DuplicateCollectionFails 重复记录集在装配期即失败（防两个 pack 撞名）。
func TestAssemble_DuplicateCollectionFails(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Assemble(mockPack()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Assemble(mockPack()); err == nil {
		t.Fatal("重复装配同名记录集应失败")
	}
}
