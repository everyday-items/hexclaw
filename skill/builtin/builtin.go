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
	"log"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
)

// RegisterAll 注册所有内置 Skill
//
// 根据配置开关，注册对应的内置 Skill 到注册中心。
func RegisterAll(registry *skill.DefaultRegistry, cfg config.BuiltinConfig) {
	if cfg.Search {
		if err := registry.Register(NewSearchSkill()); err != nil {
			log.Printf("注册搜索 Skill 失败: %v", err)
		}
	}

	if cfg.Weather {
		if err := registry.Register(NewWeatherSkill()); err != nil {
			log.Printf("注册天气 Skill 失败: %v", err)
		}
	}

	if cfg.Translate {
		if err := registry.Register(NewTranslateSkill()); err != nil {
			log.Printf("注册翻译 Skill 失败: %v", err)
		}
	}

	if cfg.Summary {
		if err := registry.Register(NewSummarySkill()); err != nil {
			log.Printf("注册摘要 Skill 失败: %v", err)
		}
	}

	if cfg.Browser {
		if err := registry.Register(NewBrowserSkill()); err != nil {
			log.Printf("注册浏览器 Skill 失败: %v", err)
		}
	}

	if cfg.Code {
		log.Println("[SECURITY WARNING] Code Skill 已启用：将在宿主机上直接执行任意代码（go run / python3 / node），" +
			"不提供内核级沙箱隔离。请确认当前进程运行于容器化或已隔离的沙箱环境，否则请在配置中关闭 builtin.code。")
		if err := registry.Register(NewCodeSkill()); err != nil {
			log.Printf("注册代码执行 Skill 失败: %v", err)
		}
	}

	if cfg.Shell {
		if err := registry.Register(NewShellSkill()); err != nil {
			log.Printf("注册 Shell Skill 失败: %v", err)
		}
	}

	// 启动日志由 main 统一输出
}
