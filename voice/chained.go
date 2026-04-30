// chained.go 实现 v0.4.0 F7 多 TTS Provider 串联（feature-flag gated）。
//
// ChainedTTS 按声明顺序依次尝试每个 Provider；第一个 Synthesize 成功的胜出。
// 推荐配置（K12 默认）：
//
//	chain := voice.NewChainedTTS(
//	    voice.NewMiniMaxTTS(key, group), // 优先：中文音色 + 速度
//	    voice.NewEdgeTTS(),              // 兜底：免费无需 Key
//	)
//
// flag voice.tts.chain.v1：alpha 默认 OFF。flag 关闭时 NewChainedTTS 仍可被构造，
// 但 Synthesize 会跳过链路，直接调用第一个 Provider —— 这样调用方可以用 ChainedTTS
// 的接口，而 fallback 行为由 flag 控制。
package voice

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagVoiceTTSChainV1 控制 F7 ChainedTTS fallback 是否生效。
const FlagVoiceTTSChainV1 = "voice.tts.chain.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagVoiceTTSChainV1,
		Default:      true, // alpha 强制 OFF
		Description:  "Enable ChainedTTS fallback (try each Provider in order until one succeeds).",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// ChainedTTS 把多个 TTSProvider 按优先级串联。
type ChainedTTS struct {
	providers []TTSProvider
}

// NewChainedTTS 构造一个 ChainedTTS。nil provider 会被跳过。
func NewChainedTTS(providers ...TTSProvider) *ChainedTTS {
	out := &ChainedTTS{providers: make([]TTSProvider, 0, len(providers))}
	for _, p := range providers {
		if p != nil {
			out.providers = append(out.providers, p)
		}
	}
	return out
}

// Name 返回 chain 中第一个 Provider 的 name 加 "-chained" 后缀。
func (c *ChainedTTS) Name() string {
	if len(c.providers) == 0 {
		return "chained-tts(empty)"
	}
	names := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		names = append(names, p.Name())
	}
	return "chained-tts[" + strings.Join(names, ",") + "]"
}

// Voices 合并所有 Provider 的 voice 列表。重复 ID 以第一个 Provider 为准。
func (c *ChainedTTS) Voices() []VoiceInfo {
	seen := map[string]bool{}
	var out []VoiceInfo
	for _, p := range c.providers {
		for _, v := range p.Voices() {
			if seen[v.ID] {
				continue
			}
			seen[v.ID] = true
			out = append(out, v)
		}
	}
	return out
}

// SupportedFormats 取所有 Provider 的并集（去重）。
func (c *ChainedTTS) SupportedFormats() []AudioFormat {
	seen := map[AudioFormat]bool{}
	var out []AudioFormat
	for _, p := range c.providers {
		for _, f := range p.SupportedFormats() {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// MultiTTSConfig 是 BuildMultiTTS 的输入：按声明顺序尝试每个 entry。
//
// 推荐用法（K12 默认）：
//
//	cfg := []voice.MultiTTSConfig{
//	    {Provider: "minimax", APIKey: "...", GroupID: "..."},   // 优先：中文音色 + 速度
//	    {Provider: "edge-tts"},                                  // 兜底：免费无需 Key
//	}
//	tts := voice.BuildMultiTTS(cfg)
type MultiTTSConfig struct {
	Provider string // "minimax" | "openai-tts" | "azure-tts" | "edge-tts"
	APIKey   string // 大多数 Provider 必需
	GroupID  string // MiniMax 必需
	BaseURL  string // 私有部署可选
	Voice    string // 默认音色
	Region   string // Azure 必需
}

// BuildMultiTTS 按声明顺序构造 ChainedTTS。空 config 返回 nil（调用方应 nil-check）。
//
// 仅认识本包内已实现的 Provider；未识别的 Provider 名静默跳过。
func BuildMultiTTS(configs []MultiTTSConfig) *ChainedTTS {
	providers := make([]TTSProvider, 0, len(configs))
	for _, c := range configs {
		switch c.Provider {
		case "minimax":
			if c.APIKey != "" && c.GroupID != "" {
				opts := []MiniMaxTTSOption{}
				if c.BaseURL != "" {
					opts = append(opts, WithMiniMaxBaseURL(c.BaseURL))
				}
				providers = append(providers, NewMiniMaxTTS(c.APIKey, c.GroupID, opts...))
			}
		case "edge-tts", "edge":
			providers = append(providers, NewEdgeTTS())
		case "openai-tts", "openai":
			if c.APIKey != "" {
				providers = append(providers, NewOpenAITTS(c.APIKey, "tts-1"))
			}
		case "azure-tts", "azure":
			if c.APIKey != "" && c.Region != "" {
				providers = append(providers, NewAzureTTS(c.APIKey, c.Region))
			}
		}
	}
	if len(providers) == 0 {
		return nil
	}
	return NewChainedTTS(providers...)
}

// Synthesize 按顺序尝试 Provider；
//   - flag voice.tts.chain.v1 OFF：只调第一个 Provider，失败即返回（v0.3 行为）
//   - flag ON：依次尝试，记录每个失败原因；全部失败时返回汇总错误
func (c *ChainedTTS) Synthesize(ctx context.Context, text string, opts SynthesizeOptions) (*SynthesizeResult, error) {
	if len(c.providers) == 0 {
		return nil, fmt.Errorf("chained-tts: no providers configured")
	}
	if !featureflag.Enabled(ctx, FlagVoiceTTSChainV1) {
		return c.providers[0].Synthesize(ctx, text, opts)
	}
	var errs []string
	for _, p := range c.providers {
		res, err := p.Synthesize(ctx, text, opts)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", p.Name(), err))
	}
	return nil, fmt.Errorf("chained-tts: all providers failed: %s", strings.Join(errs, "; "))
}
