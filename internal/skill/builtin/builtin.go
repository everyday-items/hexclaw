// Package builtin 提供 HexClaw 内置 Skill
//
// 内置 Skill 包括：
//   - search: 网络搜索（DuckDuckGo）
//   - weather: 天气查询（wttr.in）
//   - translate: 翻译（占位，走 LLM 主路径）
//   - summary: 摘要（占位，走 LLM 主路径）
//
// 所有内置 Skill 可通过配置独立开关。
package builtin

import (
	"log"

	"github.com/everyday-items/hexclaw/internal/config"
	"github.com/everyday-items/hexclaw/internal/skill"
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

	log.Printf("内置 Skill 已注册: %d 个", len(registry.All()))
}
