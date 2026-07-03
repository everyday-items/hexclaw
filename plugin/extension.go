// extension.go 实现 v0.4.0 H5 Plugin / Extension System v1（默认启用，可由 feature flag 关闭）。
//
// 现状：plugin/plugin.go 提供 SkillPlugin/AdapterPlugin/HookPlugin 三种 plugin
// 类型，但缺少：
//   - Manifest 元信息（版本 / 依赖 / capabilities 声明）
//   - 沙箱化 ExtensionContext（限制 plugin 能调用的 API surface）
//   - 兼容性校验（host version vs plugin minVersion）
//
// H5 引入 Manifest + Capability + ExtensionContext，挂 flag plugin.extension.v1
// 控制是否启用校验。flag 关闭时 LoadPlugin 直接接受任何符合接口的 plugin（
// v0.3 行为）；flag 开启时校验 manifest 兼容性 + 限制 capabilities。
package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagPluginExtensionV1 控制 H5 Manifest 校验 + 扩展上下文是否生效。
const FlagPluginExtensionV1 = "plugin.extension.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagPluginExtensionV1,
		Default:      true,
		Description:  "Validate plugin Manifest (version / capabilities) and apply ExtensionContext sandbox.",
		Stage:        featureflag.StageGA,
		SinceVersion: "0.4.0",
	})
}

// Capability 是 plugin 申请的能力声明。Host 校验后只让 plugin 看到对应的 ExtensionContext API。
type Capability string

const (
	// CapReadSkills plugin 可读取 host 的 Skill registry。
	CapReadSkills Capability = "skills.read"
	// CapEmitEvents plugin 可发出 events.Emit。
	CapEmitEvents Capability = "events.emit"
	// CapNetwork plugin 可发起对外 HTTP 请求。
	CapNetwork Capability = "network"
	// CapFileRead plugin 可读取 host 沙箱目录文件。
	CapFileRead Capability = "fs.read"
	// CapFileWrite plugin 可在沙箱目录写文件（受 .pending 类似限制）。
	CapFileWrite Capability = "fs.write"
)

// Manifest 是 plugin 启动前必须声明的元信息。host 据此校验兼容性 + 决定授权范围。
type Manifest struct {
	// Name 插件唯一标识，建议反域名（"com.example.kb-bridge"）。
	Name string
	// Version 插件自身版本（语义化）。
	Version string
	// MinHostVersion 兼容的最低 host (HexClaw) 版本，例如 "0.4.0"。
	MinHostVersion string
	// Capabilities plugin 申请的能力清单；host 在校验通过后据此构造 ExtensionContext。
	Capabilities []Capability
	// Dependencies 此 plugin 依赖的其它 plugin（按 Name 引用）。host 必须先成功加载它们。
	Dependencies []string
	// Description 一句话说明（UI / 审批界面用）。
	Description string
}

// ExtensionPlugin 是 v0.4.0 H5 协议下的 plugin 必备接口（在原有 SkillPlugin /
// AdapterPlugin / HookPlugin 之上）。
type ExtensionPlugin interface {
	// Manifest 返回 plugin 元信息（在 LoadPlugin 时被 host 读取并校验）。
	Manifest() Manifest
}

// ManifestError 是 manifest 校验失败的错误类型，便于上层精确处理。
type ManifestError struct {
	PluginName string
	Reason     string
}

// Error 实现 error 接口。
func (e *ManifestError) Error() string {
	return fmt.Sprintf("plugin manifest invalid (%s): %s", e.PluginName, e.Reason)
}

// ValidateManifest 校验 manifest 基本字段；hostVersion 用于兼容性比较（语义版本字符串比较）。
//
// 调用方式：
//
//	if err := ValidateManifest(p.Manifest(), "0.4.0"); err != nil { ... }
//
// 校验规则：
//   - Name 非空且不含 path-traversal 字符
//   - Version 非空
//   - MinHostVersion 非空且 <= hostVersion（按字符串比较，假设遵循 SemVer 词法序）
//   - Capabilities 中每个值在已知白名单内
func ValidateManifest(m Manifest, hostVersion string) error {
	if strings.TrimSpace(m.Name) == "" {
		return &ManifestError{PluginName: "<unnamed>", Reason: "Name 不能为空"}
	}
	if strings.ContainsAny(m.Name, "/\\") || strings.Contains(m.Name, "..") {
		return &ManifestError{PluginName: m.Name, Reason: "Name 含非法路径字符"}
	}
	if strings.TrimSpace(m.Version) == "" {
		return &ManifestError{PluginName: m.Name, Reason: "Version 不能为空"}
	}
	if strings.TrimSpace(m.MinHostVersion) == "" {
		return &ManifestError{PluginName: m.Name, Reason: "MinHostVersion 不能为空"}
	}
	if compareVersion(m.MinHostVersion, hostVersion) > 0 {
		return &ManifestError{
			PluginName: m.Name,
			Reason:     fmt.Sprintf("MinHostVersion=%s 高于当前 host %s", m.MinHostVersion, hostVersion),
		}
	}
	known := knownCapabilities()
	for _, c := range m.Capabilities {
		if !known[c] {
			return &ManifestError{
				PluginName: m.Name,
				Reason:     fmt.Sprintf("未知 capability %q", c),
			}
		}
	}
	return nil
}

// knownCapabilities 返回 host 接受的 capability 白名单。
// 调用方加新 capability 必须同步更新这个表，避免 plugin 静默使用未审计能力。
func knownCapabilities() map[Capability]bool {
	return map[Capability]bool{
		CapReadSkills: true,
		CapEmitEvents: true,
		CapNetwork:    true,
		CapFileRead:   true,
		CapFileWrite:  true,
	}
}

// compareVersion 按字符串语义比较 SemVer（"a.b.c" 比较），支持简单 dotted。
// 返回 -1 / 0 / 1。
func compareVersion(a, b string) int {
	ap := splitVersion(a)
	bp := splitVersion(b)
	for i := 0; i < len(ap) || i < len(bp); i++ {
		var av, bv int
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVersion(v string) []int {
	v = strings.TrimSpace(v)
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		// 截断 -beta / +meta 等后缀
		num := strings.SplitN(p, "-", 2)[0]
		num = strings.SplitN(num, "+", 2)[0]
		n := 0
		for _, c := range num {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
	}
	return out
}

// ExtensionContext 是 plugin 在 Start 后从 host 拿到的"沙箱句柄"。
// 它只暴露 manifest 中声明的 capabilities 对应的 API；其它字段为 nil。
//
// 这样实现"最小特权"：plugin 看不到 manager 内部状态，也不能调用未申请的 capability。
type ExtensionContext struct {
	// HostVersion 当前 host 的 version（用于 plugin 自适配）。
	HostVersion string

	// 仅当 manifest 含 CapReadSkills 时为非 nil
	SkillsReader SkillReader
	// 仅当 manifest 含 CapEmitEvents 时为非 nil
	EmitEvent func(eventType string, data map[string]any)
	// 仅当 manifest 含 CapNetwork 时为非 nil；host 注入的限流 / 监控 HTTP client
	HTTPClient any
	// 仅当 manifest 含 CapFileRead 时为非 nil
	FSRead func(relPath string) ([]byte, error)
	// 仅当 manifest 含 CapFileWrite 时为非 nil；写入会落到 .pending（与 F2 K12 安全底线对齐）
	FSWritePending func(relPath string, data []byte) error
}

// SkillReader 是 CapReadSkills 暴露给 plugin 的只读接口。
type SkillReader interface {
	// SkillNames 返回所有已注册 Skill 的名称列表（去重，按字典序）。
	SkillNames() []string
}

// hasCapability 在 caps 中查找 c。
func hasCapability(caps []Capability, c Capability) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// BuildExtensionContext 根据 manifest 的 capabilities 构造一个最小特权 context。
//
// host 在 plugin Start 前调本函数，把 ctx 注入 plugin 的初始化。
// 未申请的 capability 字段保持 nil（plugin 调用前应 nil-check）。
//
// flag plugin.extension.v1 关闭时返回一个空 context（HostVersion 仍填）；
// flag 开启时按 manifest 注入对应 API。
func BuildExtensionContext(
	m Manifest,
	hostVersion string,
	hostFlags featureflag.Flags,
	skills SkillReader,
	emit func(eventType string, data map[string]any),
	httpClient any,
	fsRead func(string) ([]byte, error),
	fsWritePending func(string, []byte) error,
) *ExtensionContext {
	c := &ExtensionContext{HostVersion: hostVersion}
	if hostFlags == nil || !hostFlags.IsEnabled(FlagPluginExtensionV1) {
		return c
	}
	if hasCapability(m.Capabilities, CapReadSkills) {
		c.SkillsReader = skills
	}
	if hasCapability(m.Capabilities, CapEmitEvents) {
		c.EmitEvent = emit
	}
	if hasCapability(m.Capabilities, CapNetwork) {
		c.HTTPClient = httpClient
	}
	if hasCapability(m.Capabilities, CapFileRead) {
		c.FSRead = fsRead
	}
	if hasCapability(m.Capabilities, CapFileWrite) {
		c.FSWritePending = fsWritePending
	}
	return c
}

// SortedCapabilities 返回 m.Capabilities 按字典序的副本（用于审计 / UI 展示）。
func SortedCapabilities(m Manifest) []Capability {
	out := make([]Capability, len(m.Capabilities))
	copy(out, m.Capabilities)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ErrManifestRequired 是 plugin 不实现 ExtensionPlugin 时的错误。
var ErrManifestRequired = errors.New("plugin extension v1: plugin must implement ExtensionPlugin (Manifest method)")
