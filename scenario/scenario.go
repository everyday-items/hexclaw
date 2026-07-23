// Package scenario 定义「场景包」接入平台的最小代码契约（架构设计 §6.4-6.5）。
//
// 它不是完整 pack runtime（无 install/lifecycle/隔离进程），只是一组**注入缝**——
// 场景包（如 scenarios/k12）通过它们声明式地把自己挂进平台：记录集、领域约束、
// chat 视图槽、agent-mode 特性、交互按钮、eval 套件。
//
// 铁律（AP-1）：本包 + 装配器**零业务条件**——不出现任何 "if pack==k12" / 领域词。
// 删掉 scenarios/k12 后没有 Pack 被 Assemble，平台回到干净的通用形态。
//
// 六缝：
//   - RecordSchemaRegistry（在 records 包）—— 记录集 schema
//   - ConstraintRegistry        —— 领域约束 provider（档位白名单正/倒查）
//   - ViewExtensionRegistry     —— chat 视图槽（L3 侧，配 api view descriptor）
//   - ModeFeatureRegistry       —— agent-mode 特性词（替代 engine 硬编码）
//   - ButtonRegistry            —— 声明式交互按钮（替代 engine 硬编码）
//   - EvalSuiteRegistry         —— eval 套件（本期文件系统约定）
package scenario

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/records"
)

// ConstraintProvider 领域约束（某种档位边界）。领域无关：平台只知道"某 grade 允许某些 key"。
type ConstraintProvider interface {
	// Allowed 正查：某约束档位下允许的能力键集合（白名单）。
	Allowed(ctx context.Context, grade string) ([]string, error)
	// FirstGrade 倒查：某知识点/能力键的首次出现档位（冷启动推断 / 超纲检测）。
	FirstGrade(ctx context.Context, key string) (grade string, ok bool)
}

// ViewExtension chat 视图槽声明（L3 侧）。字段对应 api view descriptor（§8.4），
// desktop chat shell 只渲染它、不认识任何具体业务视图。
type ViewExtension struct {
	HeaderTabs          []string // 会话头部 tab（由场景包声明）
	MessageBadges       []string // 消息装饰槽（如 verify-badge）
	ComposerPlaceholder string   // composer 提示语（同一 composer，仅提示语变）
	ComposerChips       []string // composer 预设 chip（由场景包声明）
	RecordCollections   []string // 关联记录集
	SidePanels          []string // 可选侧栏产物
	Actions             []string // 头部动作
	I18nKeys            []string // 文案 key（三语）
	SchemaVersion       int      // 描述符版本
}

// ModeFeature agent-mode 特性声明：一个 mode + 触发它的关键词（替代 engine 里的字面 if 链）。
type ModeFeature struct {
	Mode     string
	Keywords []string
}

// ButtonSpec 交互按钮声明（替代 engine 硬编码 BuildXxxPayload）。
type ButtonSpec struct {
	TriggerKey string   // metadata 触发键（如 expect_question_confirm）
	Prompt     string   // 按钮组提示
	Labels     []string // 按钮文案
	Actions    []string // 按钮 action
}

// Pack 一个场景包的声明式清单。装配器只 wire 它，绝不 inspect 内容做业务分支。
type Pack struct {
	Name           string
	RecordSchemas  []*records.RecordSchema
	Constraints    map[string]ConstraintProvider // key = 约束域名（如 "math"）
	ViewExtensions map[string]ViewExtension      // key = 视图槽名（如 "tutor"）
	ModeFeatures   []ModeFeature
	Buttons        []ButtonSpec
	EvalSuites     []string // eval 目录（文件系统约定）
}

// Registry 聚合六缝。平台持有一个；场景包经 Manifest v2 Install 进来
// （Assemble 保留为六缝直装契约，安装台账/卸载语义见 manifest.go）。
type Registry struct {
	Records     *records.RecordSchemaRegistry
	Constraints *ConstraintRegistry
	Views       *ViewExtensionRegistry
	Modes       *ModeFeatureRegistry
	Buttons     *ButtonRegistry
	Evals       *EvalSuiteRegistry

	// Recorder 可选的安装台账持久化端口（scenario_installations）；nil = 只内存台账。
	Recorder InstallationRecorder

	instMu    sync.Mutex
	installed map[string]*Installation // scenario id -> 安装记录（声明 + 收据）
}

// NewRegistry 建一套空注入缝。
func NewRegistry() *Registry {
	return &Registry{
		Records:     records.NewRecordSchemaRegistry(),
		Constraints: newConstraintRegistry(),
		Views:       newViewExtensionRegistry(),
		Modes:       newModeFeatureRegistry(),
		Buttons:     newButtonRegistry(),
		Evals:       newEvalSuiteRegistry(),
		installed:   make(map[string]*Installation),
	}
}

// Assemble 把一个 Pack 装配进平台。
//
// **纯声明式**：只遍历 pack 的声明逐项注册，不读 pack.Name 做任何分支。
// 任一项注册失败即整体失败（调用方决定回滚/告警）。
func (r *Registry) Assemble(p *Pack) error {
	if p == nil || p.Name == "" {
		return fmt.Errorf("scenario: pack 缺少 Name")
	}
	for _, s := range p.RecordSchemas {
		if err := r.Records.Register(s); err != nil {
			return fmt.Errorf("scenario[%s]: 注册记录集: %w", p.Name, err)
		}
	}
	for domain, cp := range p.Constraints {
		if err := r.Constraints.Register(domain, cp); err != nil {
			return fmt.Errorf("scenario[%s]: 注册约束 %s: %w", p.Name, domain, err)
		}
	}
	for slot, ve := range p.ViewExtensions {
		if err := r.Views.Register(slot, ve); err != nil {
			return fmt.Errorf("scenario[%s]: 注册视图槽 %s: %w", p.Name, slot, err)
		}
	}
	for _, mf := range p.ModeFeatures {
		if err := r.Modes.Register(mf); err != nil {
			return fmt.Errorf("scenario[%s]: 注册 mode 特性 %s: %w", p.Name, mf.Mode, err)
		}
	}
	for _, b := range p.Buttons {
		if err := r.Buttons.Register(b); err != nil {
			return fmt.Errorf("scenario[%s]: 注册按钮 %s: %w", p.Name, b.TriggerKey, err)
		}
	}
	for _, e := range p.EvalSuites {
		r.Evals.Register(e)
	}
	return nil
}

// --- 各缝的注册表实现（薄 map + 并发安全）---

// ConstraintRegistry 领域约束缝。
type ConstraintRegistry struct {
	mu sync.RWMutex
	m  map[string]ConstraintProvider
}

func newConstraintRegistry() *ConstraintRegistry {
	return &ConstraintRegistry{m: make(map[string]ConstraintProvider)}
}

// Register 注册某约束域的 provider。
func (r *ConstraintRegistry) Register(domain string, cp ConstraintProvider) error {
	if domain == "" || cp == nil {
		return fmt.Errorf("scenario: 约束域名或 provider 为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[domain]; ok {
		return fmt.Errorf("scenario: 约束域 %q 重复注册", domain)
	}
	r.m[domain] = cp
	return nil
}

// Get 取某域的约束 provider。
func (r *ConstraintRegistry) Get(domain string) (ConstraintProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp, ok := r.m[domain]
	return cp, ok
}

// ViewExtensionRegistry 视图槽缝。
type ViewExtensionRegistry struct {
	mu sync.RWMutex
	m  map[string]ViewExtension
}

func newViewExtensionRegistry() *ViewExtensionRegistry {
	return &ViewExtensionRegistry{m: make(map[string]ViewExtension)}
}

// Register 注册一个视图槽。
func (r *ViewExtensionRegistry) Register(slot string, ve ViewExtension) error {
	if slot == "" {
		return fmt.Errorf("scenario: 视图槽名为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[slot]; ok {
		return fmt.Errorf("scenario: 视图槽 %q 重复注册", slot)
	}
	r.m[slot] = ve
	return nil
}

// Get 取一个视图槽描述符（供 api view_contracts 序列化下发）。
func (r *ViewExtensionRegistry) Get(slot string) (ViewExtension, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ve, ok := r.m[slot]
	return ve, ok
}

// ModeFeatureRegistry agent-mode 特性缝（替代 engine/agent_mode.go 字面 if 链）。
type ModeFeatureRegistry struct {
	mu       sync.RWMutex
	features []ModeFeature
}

func newModeFeatureRegistry() *ModeFeatureRegistry {
	return &ModeFeatureRegistry{}
}

// Register 注册一个 mode 特性。
func (r *ModeFeatureRegistry) Register(mf ModeFeature) error {
	if mf.Mode == "" {
		return fmt.Errorf("scenario: ModeFeature 缺少 Mode")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.features = append(r.features, mf)
	return nil
}

// Match 按用户文本命中的 mode（查表分发，engine 不认识领域词）。空串 = 未命中。
func (r *ModeFeatureRegistry) Match(text string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, mf := range r.features {
		for _, kw := range mf.Keywords {
			if kw != "" && strings.Contains(text, kw) {
				return mf.Mode
			}
		}
	}
	return ""
}

// MatchesMode 判断 text 是否命中**指定 mode** 的特性词（供 engine 在原路由位置消费，保持优先级不变）。
func (r *ModeFeatureRegistry) MatchesMode(mode, text string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, mf := range r.features {
		if mf.Mode != mode {
			continue
		}
		for _, kw := range mf.Keywords {
			if kw != "" && strings.Contains(text, kw) {
				return true
			}
		}
	}
	return false
}

// ButtonRegistry 交互按钮缝。
type ButtonRegistry struct {
	mu sync.RWMutex
	m  map[string]ButtonSpec
}

func newButtonRegistry() *ButtonRegistry {
	return &ButtonRegistry{m: make(map[string]ButtonSpec)}
}

// Register 注册一个按钮（按 TriggerKey 索引）。
func (r *ButtonRegistry) Register(b ButtonSpec) error {
	if b.TriggerKey == "" {
		return fmt.Errorf("scenario: ButtonSpec 缺少 TriggerKey")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[b.TriggerKey]; ok {
		return fmt.Errorf("scenario: 按钮 %q 重复注册", b.TriggerKey)
	}
	r.m[b.TriggerKey] = b
	return nil
}

// Get 按触发键取按钮声明（engine 只渲染，不硬编码）。
func (r *ButtonRegistry) Get(triggerKey string) (ButtonSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.m[triggerKey]
	return b, ok
}

// Match 返回第一个"触发键被 isSet 判定为已触发"的按钮（isSet 由 engine 侧传入，保持本包领域中性）。
// 多按钮同时命中时返回其一（当前每场景仅少量按钮，非确定序不影响正确性）。
func (r *ButtonRegistry) Match(isSet func(triggerKey string) bool) (ButtonSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range r.m {
		if isSet(b.TriggerKey) {
			return b, true
		}
	}
	return ButtonSpec{}, false
}

// EvalSuiteRegistry eval 套件缝（本期文件系统约定：登记目录路径）。
type EvalSuiteRegistry struct {
	mu   sync.RWMutex
	dirs []string
}

func newEvalSuiteRegistry() *EvalSuiteRegistry { return &EvalSuiteRegistry{} }

// Register 登记一个 eval 目录。
func (r *EvalSuiteRegistry) Register(dir string) {
	if dir == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirs = append(r.dirs, dir)
}

// Dirs 返回已登记的 eval 目录。
func (r *EvalSuiteRegistry) Dirs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.dirs))
	copy(out, r.dirs)
	return out
}
