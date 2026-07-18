package k12

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
)

// TestManifest_Header Manifest v2 头部声明（架构设计 §6.2：id/版本/契约版本/兼容最低版本/挂载点）。
func TestManifest_Header(t *testing.T) {
	m := Manifest(NewCurriculumStub())
	if m.ID != "k12" || m.Version == "" {
		t.Fatalf("场景 id/版本缺失: %+v", m)
	}
	if m.ContractVersion < 2 {
		t.Errorf("Manifest v2 契约版本应 >=2, got %d", m.ContractVersion)
	}
	if m.MinContractVersion < 1 || m.MinContractVersion > m.ContractVersion {
		t.Errorf("兼容最低版本非法: min=%d cur=%d", m.MinContractVersion, m.ContractVersion)
	}
	if m.MountPath != "/api/k12" {
		t.Errorf("挂载点应为 /api/k12（多场景路由隔离前缀）, got %q", m.MountPath)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("K12 Manifest 应通过结构校验: %v", err)
	}
	if m.Resources == nil || m.Resources.Name != "k12" {
		t.Error("Resources 应为 K12 六缝 Pack 且 Name 与场景 ID 一致")
	}
}

// TestManifest_FreezeScope 冻结范围（架构设计 顶部冻结清单 / §6.2 / K12-INV-014）：
// 初中、高中必须 unavailable；Manifest 不出现物理、化学、音乐。
func TestManifest_FreezeScope(t *testing.T) {
	m := Manifest(NewCurriculumStub())
	if m.Stages["小学"] != scenario.Available {
		t.Errorf("小学应 available, got %q", m.Stages["小学"])
	}
	for _, stage := range []string{"初中", "高中"} {
		if m.Stages[stage] != scenario.Unavailable {
			t.Errorf("%s 必须 unavailable（冻结范围）, got %q", stage, m.Stages[stage])
		}
	}
	for _, banned := range []string{"物理", "化学", "音乐"} {
		if _, ok := m.Subjects[banned]; ok {
			t.Errorf("冻结学科 %s 不得进入当前 Manifest", banned)
		}
	}
	for _, want := range []string{"数学", "语文", "英语", "科学", "信息科技", "美术"} {
		if m.Subjects[want] != scenario.Available {
			t.Errorf("当前小学学科 %s 应 available（§1.3 学科表）, got %q", want, m.Subjects[want])
		}
	}
	// 历史仓库 Skill 也不得携带冻结学科（冻结清单第 3 条）
	for _, s := range m.Skills {
		low := strings.ToLower(s.Name)
		if strings.Contains(low, "physics") || strings.Contains(low, "chemistry") || strings.Contains(low, "music") {
			t.Errorf("冻结学科 Skill 不得进 Manifest: %s", s.Name)
		}
	}
	raw, err := m.DeclarationJSON()
	if err != nil {
		t.Fatal(err)
	}
	decl := string(raw)
	for _, banned := range []string{"物理", "化学", "音乐"} {
		if strings.Contains(decl, banned) {
			t.Errorf("Manifest 声明 JSON 不得出现冻结学科 %s", banned)
		}
	}
}

// TestManifest_NoHardcodedModels K12-INV-013：Manifest 不硬编码模型名。
func TestManifest_NoHardcodedModels(t *testing.T) {
	raw, err := Manifest(NewCurriculumStub()).DeclarationJSON()
	if err != nil {
		t.Fatal(err)
	}
	decl := strings.ToLower(string(raw))
	for _, model := range []string{"glm", "gpt", "claude", "qwen", "deepseek", "gemini"} {
		if strings.Contains(decl, model) {
			t.Errorf("Manifest 不得硬编码模型名 %q（K12-INV-013）", model)
		}
	}
}

// TestManifest_DerivedCatalog Skill catalog 从内嵌 skills 派生、数据对象从 Pack 记录集派生
// （§6.2 禁止手写学科/Skill/动作副本）。
func TestManifest_DerivedCatalog(t *testing.T) {
	m := Manifest(NewCurriculumStub())
	if len(m.Skills) == 0 {
		t.Fatal("Skill catalog 不应为空（内嵌 skills 派生）")
	}
	names := make(map[string]bool, len(m.Skills))
	for _, s := range m.Skills {
		names[s.Name] = true
	}
	for _, want := range []string{"math-tutor", "writing-feedback", "k12-pedagogy"} {
		if !names[want] {
			t.Errorf("Skill catalog 应含内嵌 skill %s, got %v", want, m.Skills)
		}
	}
	objs := m.DataObjects()
	hasMistakes := false
	for _, o := range objs {
		if o == CollectionMistakes {
			hasMistakes = true
		}
	}
	if !hasMistakes {
		t.Errorf("数据对象应从 Pack 记录集派生（含错题本）, got %v", objs)
	}
	// 工具面：平台侧注册的 k12 工具能力声明
	tools := strings.Join(m.Tools, ",")
	if !strings.Contains(tools, "k12_grade") || !strings.Contains(tools, "k12_review") {
		t.Errorf("Tool capability 应声明 k12_grade/k12_review, got %v", m.Tools)
	}
}

// TestManifest_ScheduledWorkflows 定时工作流描述符能力声明（§6.11：安装只注册任务描述符能力，
// 任务实例按 Learner 建档 provision）。
func TestManifest_ScheduledWorkflows(t *testing.T) {
	m := Manifest(NewCurriculumStub())
	want := map[string]bool{"weekly-sheet": true, "return-reminder": true, "semester-spring": true, "semester-fall": true}
	if len(m.ScheduledWorkflows) != len(want) {
		t.Fatalf("工作流描述符应为 4 个, got %v", m.ScheduledWorkflows)
	}
	for _, k := range m.ScheduledWorkflows {
		if !want[k] {
			t.Errorf("未知工作流描述符 %q", k)
		}
	}
}

// TestManifest_CoexistsWithMockScenario 双场景契约（§6.5「双 mock 场景同时安装」代替关键词指标）：
// mock 第二场景与 K12 同 Registry 共存，记录集/视图/路由互不可见；卸载 mock 不伤 K12。
func TestManifest_CoexistsWithMockScenario(t *testing.T) {
	reg := scenario.NewRegistry()
	ctx := t.Context()
	k12Receipt, err := reg.Install(ctx, Manifest(NewCurriculumStub()))
	if err != nil {
		t.Fatalf("安装 K12: %v", err)
	}
	mock := &scenario.Manifest{
		ID: "mock2", Version: "0.1.0", ContractVersion: 2, MinContractVersion: 2,
		MountPath: "/api/mock2",
		Resources: &scenario.Pack{
			Name: "mock2",
			ViewExtensions: map[string]scenario.ViewExtension{
				"mock2-view": {HeaderTabs: []string{"x"}, SchemaVersion: 1},
			},
			ModeFeatures: []scenario.ModeFeature{{Mode: "mock2-mode", Keywords: []string{"mock2-kw"}}},
			EvalSuites:   []string{"eval/mock2"},
		},
	}
	if _, err := reg.Install(ctx, mock); err != nil {
		t.Fatalf("安装 mock2: %v", err)
	}
	// K12 收据不含 mock2 资源；mock2 视图槽与 K12 tutor 槽并立
	for _, ref := range k12Receipt.Resources {
		if strings.Contains(ref.Name, "mock2") {
			t.Errorf("K12 收据越权含 mock2 资源: %+v", ref)
		}
	}
	if _, ok := reg.Views.Get("tutor"); !ok {
		t.Error("K12 tutor 视图槽应在")
	}
	if _, ok := reg.Views.Get("mock2-view"); !ok {
		t.Error("mock2 视图槽应在")
	}
	if err := reg.Uninstall(ctx, "mock2"); err != nil {
		t.Fatalf("卸载 mock2: %v", err)
	}
	if _, ok := reg.Views.Get("mock2-view"); ok {
		t.Error("卸载后 mock2 视图槽应移除")
	}
	if _, err := reg.Records.Get(CollectionMistakes); err != nil {
		t.Errorf("卸载 mock2 不应影响 K12 错题本: %v", err)
	}
	if m := reg.Modes.Match("复习一下"); m == "" {
		t.Error("卸载 mock2 不应影响 K12 mode 特性")
	}
}
