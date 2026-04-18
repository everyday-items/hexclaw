// Package voicechat 提供"音频输入 + 音频输出"的语音对话能力。
//
// 与 voice 包（STT/TTS）的差异：
//   - voice 是工具：单向 audio→text 或 text→audio
//   - voicechat 是对话：audio→audio 单次往返，模型理解语调/情绪并回应语音
//
// 支持的模型：
//   - OpenAI gpt-4o-audio-preview（chat/completions 加 modalities=['text','audio']）
//   - 智谱 glm-4-voice（专用 endpoint）
//   - 后续可扩展 Anthropic Claude voice / Gemini Live
package voicechat

import (
	"context"
	"fmt"
)

// Provider 语音对话 Provider 接口。
type Provider interface {
	Name() string
	// Chat 单次 audio→audio 对话。Request.AudioB64 为用户音频（base64），
	// 返回模型语音回应 + 文本转写。
	Chat(ctx context.Context, req Request) (*Result, error)
	SupportedModels() []string
}

// Request 语音对话请求。
type Request struct {
	Model    string `json:"model"`             // 模型 ID（如 gpt-4o-audio-preview）
	AudioB64 string `json:"audio,omitempty"`   // 用户语音（base64），与 Text 二选一
	Text     string `json:"text,omitempty"`    // 文本输入兜底（用户可能想打字+听语音）
	Voice    string `json:"voice,omitempty"`   // 输出音色（gpt-4o：alloy/echo/fable/...）
	Format   string `json:"format,omitempty"`  // 输出格式 wav/mp3，默认 wav
	System   string `json:"system,omitempty"`  // System prompt（可选）
	History  []Turn `json:"history,omitempty"` // 历史消息（用于多轮）
}

// Turn 历史轮次。
type Turn struct {
	Role    string `json:"role"`               // user / assistant
	Text    string `json:"text,omitempty"`     // 文本（不带音频时简化）
	AudioID string `json:"audio_id,omitempty"` // 历史音频 ID（gpt-4o 复用）
}

// Result 模型回应。
type Result struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	ResponseB64 string `json:"audio,omitempty"`      // 模型语音输出（base64）
	Transcript  string `json:"transcript,omitempty"` // 模型文本转写（始终返回）
	UserText    string `json:"user_text,omitempty"`  // 用户音频转写（gpt-4o 也返回）
	Format      string `json:"format,omitempty"`     // 实际输出格式
	UsageMs     int64  `json:"usage_ms,omitempty"`
}

// Service 多 Provider 路由。
type Service struct {
	providers       map[string]Provider
	modelIndex      map[string]Provider
	defaultProvider string
}

// NewService 创建服务。
func NewService(providers map[string]Provider, defaultName string) *Service {
	s := &Service{
		providers:       make(map[string]Provider, len(providers)),
		modelIndex:      make(map[string]Provider),
		defaultProvider: defaultName,
	}
	for k, p := range providers {
		if p == nil {
			continue
		}
		s.providers[k] = p
		for _, m := range p.SupportedModels() {
			if _, exists := s.modelIndex[m]; !exists {
				s.modelIndex[m] = p
			}
		}
	}
	return s
}

func (s *Service) resolve(name, model string) Provider {
	if name != "" {
		if p, ok := s.providers[name]; ok {
			return p
		}
	}
	if model != "" {
		if p, ok := s.modelIndex[model]; ok {
			return p
		}
	}
	if s.defaultProvider != "" {
		return s.providers[s.defaultProvider]
	}
	return nil
}

// Chat 调用语音对话。
func (s *Service) Chat(ctx context.Context, providerName string, req Request) (*Result, error) {
	if req.AudioB64 == "" && req.Text == "" {
		return nil, fmt.Errorf("audio 与 text 至少需要一个")
	}
	p := s.resolve(providerName, req.Model)
	if p == nil {
		return nil, fmt.Errorf("未找到可用的语音对话 Provider（provider=%q model=%q）", providerName, req.Model)
	}
	return p.Chat(ctx, req)
}

// HasProvider 是否注册了任意 Provider。
func (s *Service) HasProvider() bool { return len(s.providers) > 0 }

func (s *Service) Providers() []string {
	names := make([]string, 0, len(s.providers))
	for k := range s.providers {
		names = append(names, k)
	}
	return names
}

func (s *Service) Models() []string {
	models := make([]string, 0, len(s.modelIndex))
	for m := range s.modelIndex {
		models = append(models, m)
	}
	return models
}
