package security

import "sync/atomic"

// desktopMode 是进程级部署模式开关。桌面端 = 单用户自有机器，不存在第三方提示注入威胁，
// 内容注入 / 危险模式扫描的误杀代价 > 防护收益，故桌面模式整体放行这些内容拦截
// （ScanUserPrompt / ScanAssembled / SkillScanner.Scan）。服务端（多租户）默认开启。
//
// 仅在启动时由 cmd/hexclaw/main.go 依 --desktop 设置一次，运行期不再变更；用 atomic
// 以防与首个请求的扫描读发生数据竞争。
var desktopMode atomic.Bool

// SetDesktopMode 切换桌面模式。设为 true 后，内容注入 / 危险模式扫描全部放行。
func SetDesktopMode(on bool) { desktopMode.Store(on) }

// DesktopMode 返回当前是否为桌面模式。
func DesktopMode() bool { return desktopMode.Load() }
