package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
)

// mockManifest 构造一个覆盖六缝、资源名带 prefix 的 Manifest v2（领域中性）。
// 用不同 prefix 生成互不撞名的两个场景，验证多场景共存（§6.5「双 mock 场景同时安装」）。
func mockManifest(id, mount, prefix string) *Manifest {
	return &Manifest{
		ID:                 id,
		Version:            "1.0.0",
		ContractVersion:    2,
		MinContractVersion: 2,
		MountPath:          mount,
		Stages:             map[string]Availability{"stage-a": Available, "stage-b": Unavailable},
		Subjects:           map[string]Availability{"subj-a": Available},
		Resources: &Pack{
			Name: id,
			RecordSchemas: []*records.RecordSchema{{
				Collection:    prefix + "-notes",
				Version:       1,
				InitialStatus: "new",
				Statuses:      []string{"new", "done"},
				DedupeKey:     func(r *records.AgentRecord) string { return r.SourceSession },
			}},
			Constraints: map[string]ConstraintProvider{
				prefix + "-domain": &mockConstraint{
					allowed: map[string][]string{"g1": {"k1"}},
					first:   map[string]string{"k9": "g9"},
				},
			},
			ViewExtensions: map[string]ViewExtension{
				prefix + "-view": {HeaderTabs: []string{"a"}, SchemaVersion: 1},
			},
			ModeFeatures: []ModeFeature{{Mode: "shared-mode", Keywords: []string{prefix + "-kw"}}},
			Buttons:      []ButtonSpec{{TriggerKey: prefix + "-btn", Prompt: "?", Labels: []string{"y"}, Actions: []string{"a"}}},
			EvalSuites:   []string{"eval/" + prefix},
		},
	}
}

// TestInstall_ReceiptNamespacedResources 安装返回 InstallationReceipt（§6.3 步骤 5），
// 资源键统一为 ScenarioID@Version/ResourceID（§6.2）。
func TestInstall_ReceiptNamespacedResources(t *testing.T) {
	reg := NewRegistry()
	rc, err := reg.Install(context.Background(), mockManifest("alpha", "/api/alpha", "a"))
	if err != nil {
		t.Fatalf("安装应成功: %v", err)
	}
	if rc.ScenarioID != "alpha" || rc.Version != "1.0.0" || rc.MountPath != "/api/alpha" {
		t.Errorf("收据头字段错: %+v", rc)
	}
	if len(rc.Resources) != 6 {
		t.Fatalf("六缝各一项应记 6 条资源, got %d: %+v", len(rc.Resources), rc.Resources)
	}
	for _, ref := range rc.Resources {
		if !strings.HasPrefix(ref.Key, "alpha@1.0.0/") {
			t.Errorf("资源键须为 ScenarioID@Version/ResourceID, got %q", ref.Key)
		}
	}
	if got := reg.Installed(); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("Installed() 应含 alpha, got %v", got)
	}
}

// TestInstall_TwoScenariosCoexistAndUninstallIsolated 两个场景共存互不越权（AP-1 升级门；
// §6.3 卸载只按 Receipt 精确清理本场景资源，不影响其他专项智能体）。
func TestInstall_TwoScenariosCoexistAndUninstallIsolated(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()
	if _, err := reg.Install(ctx, mockManifest("alpha", "/api/alpha", "a")); err != nil {
		t.Fatalf("安装 alpha: %v", err)
	}
	if _, err := reg.Install(ctx, mockManifest("beta", "/api/beta", "b")); err != nil {
		t.Fatalf("安装 beta: %v", err)
	}
	// 共存：两套记录集/视图/按钮/约束并立
	for _, c := range []string{"a-notes", "b-notes"} {
		if _, err := reg.Records.Get(c); err != nil {
			t.Errorf("共存时记录集 %q 应在: %v", c, err)
		}
	}
	// 同名 mode 不同关键词并立
	if m := reg.Modes.Match("a-kw"); m != "shared-mode" {
		t.Errorf("alpha 关键词应命中, got %q", m)
	}
	if m := reg.Modes.Match("b-kw"); m != "shared-mode" {
		t.Errorf("beta 关键词应命中, got %q", m)
	}
	// 收据命名空间互斥：alpha 收据不含 beta 资源
	instA, _ := reg.Installation("alpha")
	for _, ref := range instA.Receipt.Resources {
		if strings.HasPrefix(ref.Name, "b-") {
			t.Errorf("alpha 收据越权记录 beta 资源: %+v", ref)
		}
	}

	// 卸载 alpha：alpha 六缝资源全部移除，beta 完全不受影响
	if err := reg.Uninstall(ctx, "alpha"); err != nil {
		t.Fatalf("卸载 alpha: %v", err)
	}
	if _, err := reg.Records.Get("a-notes"); err == nil {
		t.Error("卸载后 alpha 记录集应移除")
	}
	if _, ok := reg.Views.Get("a-view"); ok {
		t.Error("卸载后 alpha 视图槽应移除")
	}
	if _, ok := reg.Constraints.Get("a-domain"); ok {
		t.Error("卸载后 alpha 约束应移除")
	}
	if _, ok := reg.Buttons.Get("a-btn"); ok {
		t.Error("卸载后 alpha 按钮应移除")
	}
	if m := reg.Modes.Match("a-kw"); m != "" {
		t.Errorf("卸载后 alpha mode 关键词不应命中, got %q", m)
	}
	for _, d := range reg.Evals.Dirs() {
		if d == "eval/a" {
			t.Error("卸载后 alpha eval 目录应移除")
		}
	}
	// beta 全部健在
	if _, err := reg.Records.Get("b-notes"); err != nil {
		t.Errorf("卸载 alpha 不应影响 beta 记录集: %v", err)
	}
	if _, ok := reg.Views.Get("b-view"); !ok {
		t.Error("卸载 alpha 不应影响 beta 视图槽")
	}
	if m := reg.Modes.Match("b-kw"); m != "shared-mode" {
		t.Errorf("卸载 alpha 不应影响 beta mode, got %q", m)
	}
	if got := reg.Installed(); len(got) != 1 || got[0] != "beta" {
		t.Errorf("卸载后应只剩 beta, got %v", got)
	}
}

// TestInstall_AtomicZeroEffect 安装失败时零资源生效（§6.3）：撞已装场景的记录集名 →
// 整体失败，且该场景的其余资源（视图/按钮/eval）一个都不落地。
func TestInstall_AtomicZeroEffect(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()
	if _, err := reg.Install(ctx, mockManifest("alpha", "/api/alpha", "a")); err != nil {
		t.Fatal(err)
	}
	bad := mockManifest("gamma", "/api/gamma", "g")
	bad.Resources.RecordSchemas[0].Collection = "a-notes" // 撞名
	if _, err := reg.Install(ctx, bad); err == nil {
		t.Fatal("撞名安装应失败")
	}
	if _, ok := reg.Views.Get("g-view"); ok {
		t.Error("失败安装不应留下视图槽（零资源生效）")
	}
	if _, ok := reg.Buttons.Get("g-btn"); ok {
		t.Error("失败安装不应留下按钮")
	}
	if m := reg.Modes.Match("g-kw"); m != "" {
		t.Error("失败安装不应留下 mode 特性")
	}
	for _, d := range reg.Evals.Dirs() {
		if d == "eval/g" {
			t.Error("失败安装不应留下 eval 目录")
		}
	}
	if _, ok := reg.Installation("gamma"); ok {
		t.Error("失败安装不应进安装台账")
	}
}

// TestInstall_MountAndIDConflicts 路由前缀与场景 ID 的多场景冲突语义。
func TestInstall_MountAndIDConflicts(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()
	if _, err := reg.Install(ctx, mockManifest("alpha", "/api/alpha", "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Install(ctx, mockManifest("alpha", "/api/alpha2", "a2")); err == nil {
		t.Error("同 ID 重复安装应失败（先卸载再装新版本）")
	}
	if _, err := reg.Install(ctx, mockManifest("delta", "/api/alpha", "d")); err == nil {
		t.Error("挂载前缀冲突应失败（路由命名空间隔离）")
	}
}

// TestInstall_RequiresAndConflicts 依赖与冲突校验（§6.3 步骤 2）。
func TestInstall_RequiresAndConflicts(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()
	needs := mockManifest("beta", "/api/beta", "b")
	needs.Requires = []string{"alpha"}
	if _, err := reg.Install(ctx, needs); err == nil {
		t.Error("依赖未安装应失败")
	}
	if _, err := reg.Install(ctx, mockManifest("alpha", "/api/alpha", "a")); err != nil {
		t.Fatal(err)
	}
	hates := mockManifest("gamma", "/api/gamma", "g")
	hates.Conflicts = []string{"alpha"}
	if _, err := reg.Install(ctx, hates); err == nil {
		t.Error("冲突场景已安装应失败")
	}
}

// TestManifest_Validate 结构性校验：ID/版本/契约版本/可用性枚举/挂载前缀/Pack 名一致。
func TestManifest_Validate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"缺 ID", func(m *Manifest) { m.ID = "" }},
		{"ID 含资源键保留字符", func(m *Manifest) { m.ID = "a@b" }},
		{"缺版本", func(m *Manifest) { m.Version = "" }},
		{"契约版本 <1", func(m *Manifest) { m.ContractVersion = 0 }},
		{"最低兼容 > 当前", func(m *Manifest) { m.MinContractVersion = 99 }},
		{"挂载前缀不以 / 开头", func(m *Manifest) { m.MountPath = "api/x" }},
		{"非法可用性枚举", func(m *Manifest) { m.Stages["stage-a"] = "maybe" }},
		{"Pack 名与场景 ID 漂移", func(m *Manifest) { m.Resources.Name = "other" }},
	}
	for _, tc := range cases {
		m := mockManifest("alpha", "/api/alpha", "a")
		tc.mutate(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: 应校验失败", tc.name)
		}
	}
	if err := mockManifest("alpha", "/api/alpha", "a").Validate(); err != nil {
		t.Errorf("合法 Manifest 不应报错: %v", err)
	}
}

// TestManifest_DerivedDeclarations 数据对象/动作从 Pack 派生（§6.2 禁止手写副本），
// DeclarationJSON 不携带不可序列化的 Resources。
func TestManifest_DerivedDeclarations(t *testing.T) {
	m := mockManifest("alpha", "/api/alpha", "a")
	if got := m.DataObjects(); len(got) != 1 || got[0] != "a-notes" {
		t.Errorf("DataObjects 应从记录集派生, got %v", got)
	}
	if got := m.Actions(); len(got) != 1 || got[0] != "a" {
		t.Errorf("Actions 应从按钮派生, got %v", got)
	}
	raw, err := m.DeclarationJSON()
	if err != nil {
		t.Fatalf("DeclarationJSON: %v", err)
	}
	var head map[string]any
	if err := json.Unmarshal(raw, &head); err != nil {
		t.Fatalf("声明 JSON 应可解析: %v", err)
	}
	if head["id"] != "alpha" {
		t.Errorf("声明 JSON 缺 id: %s", raw)
	}
	if _, ok := head["resources"]; ok {
		t.Error("声明 JSON 不应内嵌 Resources（含函数字段，收据才是资源清单）")
	}
}

// fakeRecorder 记录持久化调用；可注入失败模拟收据落库失败。
type fakeRecorder struct {
	installs, uninstalls []string
	failInstall          bool
}

func (f *fakeRecorder) RecordInstall(_ context.Context, m *Manifest, _ *Receipt) error {
	if f.failInstall {
		return fmt.Errorf("boom")
	}
	f.installs = append(f.installs, m.ID)
	return nil
}
func (f *fakeRecorder) RecordUninstall(_ context.Context, id string) error {
	f.uninstalls = append(f.uninstalls, id)
	return nil
}

// TestInstall_RecorderPersistence 安装/卸载写收据台账；落库失败 = 安装失败且零资源生效。
func TestInstall_RecorderPersistence(t *testing.T) {
	ctx := context.Background()
	rec := &fakeRecorder{}
	reg := NewRegistry()
	reg.Recorder = rec
	if _, err := reg.Install(ctx, mockManifest("alpha", "/api/alpha", "a")); err != nil {
		t.Fatal(err)
	}
	if err := reg.Uninstall(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if len(rec.installs) != 1 || rec.installs[0] != "alpha" || len(rec.uninstalls) != 1 {
		t.Errorf("持久化调用错: %+v", rec)
	}

	rec2 := &fakeRecorder{failInstall: true}
	reg2 := NewRegistry()
	reg2.Recorder = rec2
	if _, err := reg2.Install(ctx, mockManifest("alpha", "/api/alpha", "a")); err == nil {
		t.Fatal("收据落库失败应使安装整体失败")
	}
	if _, err := reg2.Records.Get("a-notes"); err == nil {
		t.Error("落库失败后应回滚已注册资源（零资源生效）")
	}
	if _, ok := reg2.Installation("alpha"); ok {
		t.Error("落库失败不应进安装台账")
	}
}

// TestUninstall_Unknown 卸载未安装场景应显式报错。
func TestUninstall_Unknown(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Uninstall(context.Background(), "ghost"); err == nil {
		t.Fatal("卸载未安装场景应报错")
	}
}
