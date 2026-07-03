// Package builtin 提供 HexClaw 内置 Skill
//
// 内置 Skill 包括：
//   - search: 网络搜索（DuckDuckGo）
//   - weather: 天气查询（wttr.in，带自动重试）
//   - translate: 翻译（本地规则引擎）
//   - summary: 摘要（本地抽取式摘要）
//
// 所有内置 Skill 可通过配置独立开关。
package builtin

import (
	"github.com/hexagon-codes/toolkit/util/logger"
	"os"
	"path/filepath"

	"github.com/hexagon-codes/hexclaw/config"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/security"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/hub"
	"github.com/hexagon-codes/toolkit/os/sandbox"
)

// SkillDeps holds optional dependencies for skills that need external services.
type SkillDeps struct {
	SkillHub      *hub.Hub
	McpHub        *hub.McpHub
	McpMgr        *hexmcp.Manager
	CfgWriter     *config.Writer
	Workspace     string         // workspace dir for file ops (default ~/.hexclaw/workspace)
	CodeExecSkill *CodeExecSkill // populated by RegisterAdvanced if code_exec enabled
	FileAccess    *FileAccessBroker
	// SandboxReadablePaths 额外授予 code_exec 沙箱只读访问的宿主路径（Workspace 之外）。
	// 来源 = config.Skill.Sandbox.Filesystem.AllowedPaths（用户经数据连接器授权的本地目录）。
	SandboxReadablePaths []string
}

// RegisterAll 注册所有内置 Skill
//
// 根据配置开关，注册对应的内置 Skill 到注册中心。
func RegisterAll(registry *skill.DefaultRegistry, cfg config.BuiltinConfig) {
	if cfg.Search {
		if err := registry.Register(NewSearchSkill()); err != nil {
			logger.Error("注册搜索 Skill 失败", "error", err)
		}
	}

	if cfg.Weather {
		if err := registry.Register(NewWeatherSkill()); err != nil {
			logger.Error("注册天气 Skill 失败", "error", err)
		}
	}

	if cfg.Translate {
		if err := registry.Register(NewTranslateSkill()); err != nil {
			logger.Error("注册翻译 Skill 失败", "error", err)
		}
	}

	if cfg.Summary {
		if err := registry.Register(NewSummarySkill()); err != nil {
			logger.Error("注册摘要 Skill 失败", "error", err)
		}
	}

	if cfg.Browser {
		if err := registry.Register(NewBrowserSkill()); err != nil {
			logger.Error("注册浏览器 Skill 失败", "error", err)
		}
	}

	if cfg.Code {
		logger.Warn("[SECURITY WARNING] Code Skill 已启用：将在宿主机上直接执行任意代码（go run / python3 / node），" +
			"不提供内核级沙箱隔离。请确认当前进程运行于容器化或已隔离的沙箱环境，否则请在配置中关闭 builtin.code。")
		if err := registry.Register(NewCodeSkill()); err != nil {
			logger.Error("注册代码执行 Skill 失败", "error", err)
		}
	}

	if cfg.Shell {
		if err := registry.Register(NewShellSkill()); err != nil {
			logger.Error("注册 Shell Skill 失败", "error", err)
		}
	}

	if cfg.FileOps {
		ws := defaultWorkspace()
		if err := registry.Register(NewFileOpsSkill(ws)); err != nil {
			logger.Error("注册文件操作 Skill 失败", "error", err)
		}
		if err := registry.Register(NewFileEditSkill(ws)); err != nil {
			logger.Error("注册文件编辑 Skill 失败", "error", err)
		}
		if err := registry.Register(NewGrepSkill(ws)); err != nil {
			logger.Error("注册 Grep Skill 失败", "error", err)
		}
		if err := registry.Register(NewGlobSkill(ws)); err != nil {
			logger.Error("注册 Glob Skill 失败", "error", err)
		}
	}

	// 启动日志由 main 统一输出
}

// RegisterAdvanced registers skills that require external dependencies.
// Called from main.go after all services are initialized.
func RegisterAdvanced(registry *skill.DefaultRegistry, cfg config.BuiltinConfig, deps *SkillDeps) {
	if cfg.FileOps {
		broker := NewFileAccessBroker(deps.SandboxReadablePaths)
		if err := registry.Register(NewListDirectorySkill(broker)); err != nil {
			logger.Error("注册 ListDirectorySkill 失败", "error", err)
		}
		if err := registry.Register(NewReadFileSkill(broker)); err != nil {
			logger.Error("注册 ReadFileSkill 失败", "error", err)
		}
		if err := registry.Register(NewListAllowedDirectoriesSkill(broker)); err != nil {
			logger.Error("注册 ListAllowedDirectoriesSkill 失败", "error", err)
		}
		deps.FileAccess = broker
	}

	if cfg.CodeExec {
		ws := deps.Workspace
		if ws == "" {
			ws = defaultWorkspace()
		}
		sbCfg := sandbox.Config{
			Workspace: ws,
			Timeout:   30,
			Network:   cfg.CodeExecPolicy.CodeExecNetworkAllowed(),
			// 允许外网但禁止本机回环：无人值守 agent 的 code_exec 需要抓网页/调 API，
			// 但绝不该经 loopback 打本机管理端口（API server 自提权 / Ollama / 其它
			// sidecar）——那是 SSRF 与权限自提升面（GO-1/GO-2）。darwin 经 seatbelt
			// 强制；linux 目前 Network=true 不隔离 net namespace，回环仍可达（能力缺口）。
			DenyLoopback: true,
			// 用户经数据连接器授权的本地目录 → 沙箱只读放行，否则 code_exec 读不到（BUG-20260626）。
			ReadablePaths: deps.SandboxReadablePaths,
		}
		sb, err := sandbox.New(sbCfg)
		if err != nil {
			logger.Error("沙箱初始化失败，CodeExecSkill 不可用", "error", err)
		} else {
			codeExec := NewCodeExecSkill(sb, sbCfg)
			// 集中文件访问裁决：mode=file/project 触达宿主机的路径须过 allow-list（fail-closed）。
			// 复用 FileOps 已建的 broker；未建（FileOps 关闭）则按用户授权目录新建一个，
			// 授权集为空时 code_exec 仅能在沙箱 workspace 内运行外部路径全部被拒。
			broker := deps.FileAccess
			if broker == nil {
				broker = NewFileAccessBroker(deps.SandboxReadablePaths)
				deps.FileAccess = broker
			}
			codeExec.SetFileAccess(broker)
			if err := registry.Register(codeExec); err != nil {
				logger.Error("注册 CodeExecSkill 失败", "error", err)
			}
			deps.CodeExecSkill = codeExec
		}
	}

	// SkillWriter + Scanner
	scanner := security.NewSkillScanner()
	skillDir := defaultSkillDir()
	if err := registry.Register(NewSkillWriterSkill(skillDir, scanner)); err != nil {
		logger.Error("注册 SkillWriter 失败", "error", err)
	}

	// SkillInstaller (hub)
	if deps.SkillHub != nil {
		if err := registry.Register(NewSkillInstallerSkill(deps.SkillHub)); err != nil {
			logger.Error("注册 SkillInstaller 失败", "error", err)
		}
	}

	// McpInstaller (hub + manager + persistence)
	if deps.McpHub != nil && deps.McpMgr != nil {
		if err := registry.Register(NewMcpInstallerSkill(deps.McpHub, deps.McpMgr, deps.CfgWriter)); err != nil {
			logger.Error("注册 McpInstaller 失败", "error", err)
		}
	}
}

func defaultWorkspace() string {
	home, _ := os.UserHomeDir()
	ws := filepath.Join(home, ".hexclaw", "workspace")
	os.MkdirAll(ws, 0755)
	return ws
}

// DefaultWorkspace is the exported workspace root used as the resolveSafePath
// boundary for file-touching skills (FileOps, knowledge_ingest_path). Exposed so
// cmd/hexclaw can wire skills constructed outside RegisterAdvanced with the same
// sandbox root.
func DefaultWorkspace() string { return defaultWorkspace() }

func defaultSkillDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hexclaw", "skills")
}
