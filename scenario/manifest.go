// manifest.go 定义 ScenarioManifest v2 与多场景安装/卸载语义（架构设计 §6.2/§6.3/§6.5）。
//
// Manifest = 唯一版本化声明头（id/版本/契约版本/能力/挂载点/依赖冲突/工作流描述符）
// + 六缝资源载荷（Pack，安装/卸载资源）。演进策略：**向后兼容**——Pack 与 Assemble
// 原样保留（六缝契约不破），Install 是 v2 的多场景入口：
//
//	解析 Manifest → 校验（版本/依赖/冲突/重复键/挂载前缀）→ 生成安装计划（不改运行时）
//	→ 一次提交命名空间化资源 → 返回 InstallationReceipt（§6.3 五步；失败零资源生效）。
//
// 卸载只按 Receipt 精确清理本场景资源，不做全局 reset，不影响其他场景；
// 业务数据（如 k12_* 表）保留——文档未规定卸载删数据，按保留处理（见迁移 V10 注释）。
package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Availability 能力可用性枚举（§6.2 阶段能力 available/degraded/unavailable）。
type Availability string

const (
	Available   Availability = "available"
	Degraded    Availability = "degraded"
	Unavailable Availability = "unavailable"
)

func (a Availability) valid() bool {
	switch a {
	case Available, Degraded, Unavailable:
		return true
	}
	return false
}

// SkillDecl Skill catalog 一项（§6.2：名称/版本/依赖；完整验收以场景的 Skill 清单文档为准）。
type SkillDecl struct {
	Name     string   `json:"name"`
	Version  string   `json:"version,omitempty"`
	Requires []string `json:"requires,omitempty"`
}

// Manifest ScenarioManifest v2（§6.2）：唯一版本化场景声明。
//
// 声明头可序列化（DeclarationJSON）；Resources 是六缝资源载荷（含函数/接口，不序列化，
// 实际创建的资源以 Receipt 为准）。数据对象与动作从 Resources 派生（DataObjects/Actions），
// 禁止再维护手写副本。
type Manifest struct {
	ID                 string                  `json:"id"`                            // scenario id
	Version            string                  `json:"version"`                       // 场景版本
	ContractVersion    int                     `json:"contract_version"`              // Manifest 契约版本（v2 = 2）
	MinContractVersion int                     `json:"min_contract_version"`          // 兼容最低契约版本
	MountPath          string                  `json:"mount_path,omitempty"`          // HTTP 挂载前缀（多场景路由隔离）
	Stages             map[string]Availability `json:"stages,omitempty"`              // 阶段能力
	Subjects           map[string]Availability `json:"subjects,omitempty"`            // 学科能力
	Skills             []SkillDecl             `json:"skills,omitempty"`              // Skill catalog
	Tools              []string                `json:"tools,omitempty"`               // Tool capability（平台工具面注册名）
	Validators         []string                `json:"validators,omitempty"`          // 已达质量门的确定性验证器
	ChannelProjections []string                `json:"channel_projections,omitempty"` // 渠道投影能力
	Priority           int                     `json:"priority,omitempty"`            // 显式 priority（§6.2 资源排序）
	Requires           []string                `json:"requires,omitempty"`            // 依赖的场景 id
	Conflicts          []string                `json:"conflicts,omitempty"`           // 冲突的场景 id
	Migrations         []string                `json:"migrations,omitempty"`          // 数据迁移标识（声明）
	ScheduledWorkflows []string                `json:"scheduled_workflows,omitempty"` // 工作流描述符（实例建档时 provision）
	Resources          *Pack                   `json:"-"`                             // 六缝安装/卸载资源载荷
}

// Validate 结构性校验（§6.3 步骤 2 的领域中性部分；产品冻结范围由场景自身契约测试钉死）。
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("scenario: manifest 为 nil")
	}
	if m.ID == "" {
		return fmt.Errorf("scenario: manifest 缺少 id")
	}
	if strings.ContainsAny(m.ID, "@/") {
		return fmt.Errorf("scenario: manifest id %q 含资源键保留字符 @ /", m.ID)
	}
	if m.Version == "" {
		return fmt.Errorf("scenario[%s]: manifest 缺少 version", m.ID)
	}
	if m.ContractVersion < 1 {
		return fmt.Errorf("scenario[%s]: contract_version 须 >=1, got %d", m.ID, m.ContractVersion)
	}
	if m.MinContractVersion < 1 || m.MinContractVersion > m.ContractVersion {
		return fmt.Errorf("scenario[%s]: min_contract_version 非法（min=%d cur=%d）", m.ID, m.MinContractVersion, m.ContractVersion)
	}
	if m.MountPath != "" && !strings.HasPrefix(m.MountPath, "/") {
		return fmt.Errorf("scenario[%s]: 挂载前缀须以 / 开头, got %q", m.ID, m.MountPath)
	}
	for stage, av := range m.Stages {
		if !av.valid() {
			return fmt.Errorf("scenario[%s]: 阶段 %q 可用性非法 %q（available/degraded/unavailable）", m.ID, stage, av)
		}
	}
	for subj, av := range m.Subjects {
		if !av.valid() {
			return fmt.Errorf("scenario[%s]: 学科 %q 可用性非法 %q", m.ID, subj, av)
		}
	}
	if m.Resources != nil && m.Resources.Name != m.ID {
		return fmt.Errorf("scenario[%s]: Pack.Name %q 与场景 id 漂移", m.ID, m.Resources.Name)
	}
	return nil
}

// DataObjects 数据对象声明——从 Pack 记录集派生（§6.2 禁止手写副本）。
func (m *Manifest) DataObjects() []string {
	if m.Resources == nil {
		return nil
	}
	out := make([]string, 0, len(m.Resources.RecordSchemas))
	for _, s := range m.Resources.RecordSchemas {
		out = append(out, s.Collection)
	}
	return out
}

// Actions 动作声明——从 Pack 按钮 action 派生（§6.2 禁止手写副本）。
func (m *Manifest) Actions() []string {
	if m.Resources == nil {
		return nil
	}
	var out []string
	for _, b := range m.Resources.Buttons {
		out = append(out, b.Actions...)
	}
	return out
}

// ResourceKey 统一资源键 ScenarioID@Version/ResourceID（§6.2）。
func (m *Manifest) ResourceKey(kind ResourceKind, name string) string {
	return fmt.Sprintf("%s@%s/%s:%s", m.ID, m.Version, kind, name)
}

// DeclarationJSON 声明头 JSON（安装台账 manifest_json；Resources 不序列化，
// 实际创建资源见 Receipt）。
func (m *Manifest) DeclarationJSON() ([]byte, error) {
	return json.Marshal(m)
}

// ResourceKind 收据里的资源类别（六缝各一）。
type ResourceKind string

const (
	KindRecordSchema ResourceKind = "record-schema"
	KindConstraint   ResourceKind = "constraint"
	KindView         ResourceKind = "view"
	KindModeFeature  ResourceKind = "mode-feature"
	KindButton       ResourceKind = "button"
	KindEvalSuite    ResourceKind = "eval-suite"
)

// ResourceRef 收据资源项：类别 + 缝内名 + 命名空间化资源键。
// mode 特性无天然主键，附带 Keywords 使卸载可精确移除本场景那一条。
type ResourceRef struct {
	Kind     ResourceKind `json:"kind"`
	Name     string       `json:"name"`
	Key      string       `json:"key"`
	Keywords []string     `json:"keywords,omitempty"`
}

// Receipt InstallationReceipt（§6.3 步骤 5）：记录实际创建资源；卸载按它精确清理。
type Receipt struct {
	ScenarioID  string        `json:"scenario_id"`
	Version     string        `json:"version"`
	MountPath   string        `json:"mount_path,omitempty"`
	Resources   []ResourceRef `json:"resources"`
	InstalledAt int64         `json:"installed_at"`
}

// InstallationRecorder 安装台账持久化端口（scenario_installations 表；实现见
// storage/scenarioinstall）。落库失败视为安装失败（原子性含收据）。
type InstallationRecorder interface {
	RecordInstall(ctx context.Context, m *Manifest, r *Receipt) error
	RecordUninstall(ctx context.Context, scenarioID string) error
}

// Installation 一次已生效安装：声明 + 收据。
type Installation struct {
	Manifest *Manifest
	Receipt  *Receipt
}

// plannedResource 安装计划一项：收据引用 + 提交/回滚闭包（同一遍历产出，防两处漂移）。
type plannedResource struct {
	ref      ResourceRef
	register func() error
	remove   func()
}

// Install 按 Manifest v2 原子安装一个场景（§6.3 五步）。
//
// 计划期只做冲突校验不改运行时；提交期任一步失败回滚已注册资源 + 不写台账 = 零资源生效。
// 提交顺序稳定（§6.5：记录集按声明序，map 缝按键排序）。
func (r *Registry) Install(ctx context.Context, m *Manifest) (*Receipt, error) {
	// 1. 解析/结构校验
	if err := m.Validate(); err != nil {
		return nil, err
	}
	r.instMu.Lock()
	defer r.instMu.Unlock()
	// 2. 版本/依赖/冲突/挂载前缀校验
	if _, ok := r.installed[m.ID]; ok {
		return nil, fmt.Errorf("scenario[%s]: 已安装（升级请先卸载再装新版本）", m.ID)
	}
	for _, req := range m.Requires {
		if _, ok := r.installed[req]; !ok {
			return nil, fmt.Errorf("scenario[%s]: 依赖场景 %q 未安装", m.ID, req)
		}
	}
	for _, c := range m.Conflicts {
		if _, ok := r.installed[c]; ok {
			return nil, fmt.Errorf("scenario[%s]: 与已安装场景 %q 冲突", m.ID, c)
		}
	}
	if m.MountPath != "" {
		for id, inst := range r.installed {
			if inst.Manifest.MountPath == m.MountPath {
				return nil, fmt.Errorf("scenario[%s]: 挂载前缀 %q 已被场景 %q 占用", m.ID, m.MountPath, id)
			}
		}
	}
	// 3. 生成安装计划（不修改运行时）
	plan, err := r.planResources(m)
	if err != nil {
		return nil, fmt.Errorf("scenario[%s]: 安装计划失败（零资源生效）: %w", m.ID, err)
	}
	// 4. 一次提交；失败回滚已提交项
	var committed []plannedResource
	rollback := func() {
		for i := len(committed) - 1; i >= 0; i-- {
			committed[i].remove()
		}
	}
	for _, p := range plan {
		if err := p.register(); err != nil {
			rollback()
			return nil, fmt.Errorf("scenario[%s]: 提交 %s %q 失败（已回滚，零资源生效）: %w", m.ID, p.ref.Kind, p.ref.Name, err)
		}
		committed = append(committed, p)
	}
	// 5. 收据 + 台账（落库失败同样回滚）
	refs := make([]ResourceRef, 0, len(plan))
	for _, p := range plan {
		refs = append(refs, p.ref)
	}
	receipt := &Receipt{
		ScenarioID:  m.ID,
		Version:     m.Version,
		MountPath:   m.MountPath,
		Resources:   refs,
		InstalledAt: time.Now().Unix(),
	}
	if r.Recorder != nil {
		if err := r.Recorder.RecordInstall(ctx, m, receipt); err != nil {
			rollback()
			return nil, fmt.Errorf("scenario[%s]: 安装收据落库失败（已回滚，零资源生效）: %w", m.ID, err)
		}
	}
	r.installed[m.ID] = &Installation{Manifest: m, Receipt: receipt}
	return receipt, nil
}

// planResources 生成稳定排序的安装计划，并做零副作用冲突预检（重复键 §6.3 步骤 2）。
func (r *Registry) planResources(m *Manifest) ([]plannedResource, error) {
	var plan []plannedResource
	if m.Resources == nil {
		return plan, nil
	}
	p := m.Resources
	// 记录集：声明序
	seen := map[string]bool{}
	for _, s := range p.RecordSchemas {
		s := s
		if seen[s.Collection] {
			return nil, fmt.Errorf("记录集 %q 场景内重复声明", s.Collection)
		}
		seen[s.Collection] = true
		if _, err := r.Records.Get(s.Collection); err == nil {
			return nil, fmt.Errorf("记录集 %q 已被占用", s.Collection)
		}
		plan = append(plan, plannedResource{
			ref:      ResourceRef{Kind: KindRecordSchema, Name: s.Collection, Key: m.ResourceKey(KindRecordSchema, s.Collection)},
			register: func() error { return r.Records.Register(s) },
			remove:   func() { r.Records.Deregister(s.Collection) },
		})
	}
	// 约束：域名排序
	for _, domain := range sortedKeys(p.Constraints) {
		domain, cp := domain, p.Constraints[domain]
		if _, ok := r.Constraints.Get(domain); ok {
			return nil, fmt.Errorf("约束域 %q 已被占用", domain)
		}
		plan = append(plan, plannedResource{
			ref:      ResourceRef{Kind: KindConstraint, Name: domain, Key: m.ResourceKey(KindConstraint, domain)},
			register: func() error { return r.Constraints.Register(domain, cp) },
			remove:   func() { r.Constraints.Remove(domain) },
		})
	}
	// 视图槽：槽名排序
	for _, slot := range sortedKeys(p.ViewExtensions) {
		slot, ve := slot, p.ViewExtensions[slot]
		if _, ok := r.Views.Get(slot); ok {
			return nil, fmt.Errorf("视图槽 %q 已被占用", slot)
		}
		plan = append(plan, plannedResource{
			ref:      ResourceRef{Kind: KindView, Name: slot, Key: m.ResourceKey(KindView, slot)},
			register: func() error { return r.Views.Register(slot, ve) },
			remove:   func() { r.Views.Remove(slot) },
		})
	}
	// mode 特性：声明序（同 mode 可多场景并立，卸载按 Mode+Keywords 精确移除）
	for _, mf := range p.ModeFeatures {
		mf := mf
		plan = append(plan, plannedResource{
			ref:      ResourceRef{Kind: KindModeFeature, Name: mf.Mode, Key: m.ResourceKey(KindModeFeature, mf.Mode), Keywords: mf.Keywords},
			register: func() error { return r.Modes.Register(mf) },
			remove:   func() { r.Modes.Remove(mf) },
		})
	}
	// 按钮：声明序
	for _, b := range p.Buttons {
		b := b
		if _, ok := r.Buttons.Get(b.TriggerKey); ok {
			return nil, fmt.Errorf("按钮 %q 已被占用", b.TriggerKey)
		}
		plan = append(plan, plannedResource{
			ref:      ResourceRef{Kind: KindButton, Name: b.TriggerKey, Key: m.ResourceKey(KindButton, b.TriggerKey)},
			register: func() error { return r.Buttons.Register(b) },
			remove:   func() { r.Buttons.Remove(b.TriggerKey) },
		})
	}
	// eval 套件：声明序
	for _, dir := range p.EvalSuites {
		dir := dir
		plan = append(plan, plannedResource{
			ref:      ResourceRef{Kind: KindEvalSuite, Name: dir, Key: m.ResourceKey(KindEvalSuite, dir)},
			register: func() error { r.Evals.Register(dir); return nil },
			remove:   func() { r.Evals.Remove(dir) },
		})
	}
	return plan, nil
}

// Uninstall 按 Receipt 精确卸载一个场景（§6.3）：只移除该场景注册的运行时资源，
// 不做全局 reset，不影响其他场景；业务数据保留（数据保留策略申报见包注释）。
func (r *Registry) Uninstall(ctx context.Context, scenarioID string) error {
	r.instMu.Lock()
	defer r.instMu.Unlock()
	inst, ok := r.installed[scenarioID]
	if !ok {
		return fmt.Errorf("scenario[%s]: 未安装，无法卸载", scenarioID)
	}
	for i := len(inst.Receipt.Resources) - 1; i >= 0; i-- {
		r.removeRef(inst.Receipt.Resources[i])
	}
	delete(r.installed, scenarioID)
	if r.Recorder != nil {
		if err := r.Recorder.RecordUninstall(ctx, scenarioID); err != nil {
			return fmt.Errorf("scenario[%s]: 卸载台账落库失败（运行时资源已移除）: %w", scenarioID, err)
		}
	}
	return nil
}

// removeRef 按收据项移除一条运行时资源。
func (r *Registry) removeRef(ref ResourceRef) {
	switch ref.Kind {
	case KindRecordSchema:
		r.Records.Deregister(ref.Name)
	case KindConstraint:
		r.Constraints.Remove(ref.Name)
	case KindView:
		r.Views.Remove(ref.Name)
	case KindModeFeature:
		r.Modes.Remove(ModeFeature{Mode: ref.Name, Keywords: ref.Keywords})
	case KindButton:
		r.Buttons.Remove(ref.Name)
	case KindEvalSuite:
		r.Evals.Remove(ref.Name)
	}
}

// Installed 已安装场景 id（稳定排序，§6.5）。
func (r *Registry) Installed() []string {
	r.instMu.Lock()
	defer r.instMu.Unlock()
	out := make([]string, 0, len(r.installed))
	for id := range r.installed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Installation 取某场景的安装记录（声明 + 收据）。
func (r *Registry) Installation(scenarioID string) (*Installation, bool) {
	r.instMu.Lock()
	defer r.instMu.Unlock()
	inst, ok := r.installed[scenarioID]
	return inst, ok
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- 各缝的卸载移除（只删指定项，不清表）---

// Remove 移除某约束域（卸载用）。
func (r *ConstraintRegistry) Remove(domain string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, domain)
}

// Remove 移除某视图槽（卸载用）。
func (r *ViewExtensionRegistry) Remove(slot string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, slot)
}

// Remove 移除第一条 Mode 与 Keywords 完全一致的特性（卸载用；同 mode 其他场景的条目不受影响）。
func (r *ModeFeatureRegistry) Remove(mf ModeFeature) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, f := range r.features {
		if f.Mode == mf.Mode && strings.Join(f.Keywords, "\x00") == strings.Join(mf.Keywords, "\x00") {
			r.features = append(r.features[:i], r.features[i+1:]...)
			return
		}
	}
}

// Remove 移除某触发键的按钮（卸载用）。
func (r *ButtonRegistry) Remove(triggerKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, triggerKey)
}

// Remove 移除第一条匹配的 eval 目录（卸载用）。
func (r *EvalSuiteRegistry) Remove(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, d := range r.dirs {
		if d == dir {
			r.dirs = append(r.dirs[:i], r.dirs[i+1:]...)
			return
		}
	}
}
