// Package voice 提供语音交互能力
//
// 定义 STT (Speech-to-Text) 和 TTS (Text-to-Speech) 接口，
// 支持多种语音 Provider 的接入。
//
// 目前支持的 Provider：
//   - OpenAI Whisper (STT) + OpenAI TTS
//   - 自定义 HTTP 接口 (通用)
//
// 语音交互流程：
//
//	用户语音 → STT 转文本 → Agent 处理 → TTS 转语音 → 返回用户
//
// 对标 OpenClaw Voice Mode。
//
// 用法：
//
//	svc := voice.NewService(sttProvider, ttsProvider)
//	text, _ := svc.Transcribe(ctx, audioData, "zh")
//	audio, _ := svc.Synthesize(ctx, "你好", "zh")
package voice

import (
	"context"
	"fmt"
)

// AudioFormat 音频格式
type AudioFormat string

const (
	FormatWAV  AudioFormat = "wav"
	FormatMP3  AudioFormat = "mp3"
	FormatOGG  AudioFormat = "ogg"
	FormatFLAC AudioFormat = "flac"
	FormatPCM  AudioFormat = "pcm"
)

// STTProvider 语音转文本 Provider 接口
//
// 将音频数据转换为文本。
// 支持多种音频格式和语言。
type STTProvider interface {
	// Name 返回 Provider 名称（如 "openai-whisper"）
	Name() string

	// Transcribe 将音频转换为文本
	//
	// 参数:
	//   - audio: 音频数据
	//   - opts: 转录选项（语言、格式等）
	//
	// 返回文本结果和可能的错误
	Transcribe(ctx context.Context, audio []byte, opts TranscribeOptions) (*TranscribeResult, error)

	// SupportedFormats 返回支持的音频格式列表
	SupportedFormats() []AudioFormat

	// SupportedLanguages 返回支持的语言代码列表
	SupportedLanguages() []string
}

// TTSProvider 文本转语音 Provider 接口
//
// 将文本合成为音频数据。
// 支持多种音色和语言。
type TTSProvider interface {
	// Name 返回 Provider 名称（如 "openai-tts"）
	Name() string

	// Synthesize 将文本合成为音频
	//
	// 参数:
	//   - text: 要合成的文本
	//   - opts: 合成选项（音色、格式等）
	//
	// 返回音频数据和可能的错误
	Synthesize(ctx context.Context, text string, opts SynthesizeOptions) (*SynthesizeResult, error)

	// Voices 返回可用的音色列表
	Voices() []VoiceInfo

	// SupportedFormats 返回支持的输出格式
	SupportedFormats() []AudioFormat
}

// TranscribeOptions STT 转录选项
type TranscribeOptions struct {
	Language string      `json:"language,omitempty"` // 语言代码（如 "zh", "en"），空则自动检测
	Format   AudioFormat `json:"format,omitempty"`   // 音频格式，空则自动检测
	Prompt   string      `json:"prompt,omitempty"`   // 提示词（帮助识别特定术语）
}

// TranscribeResult STT 转录结果
type TranscribeResult struct {
	Text       string  `json:"text"`                 // 转录文本
	Language   string  `json:"language,omitempty"`    // 检测到的语言
	Duration   float64 `json:"duration,omitempty"`    // 音频时长（秒）
	Confidence float64 `json:"confidence,omitempty"`  // 置信度 (0-1)
}

// SynthesizeOptions TTS 合成选项
type SynthesizeOptions struct {
	Voice  string      `json:"voice,omitempty"`  // 音色名称
	Format AudioFormat `json:"format,omitempty"` // 输出格式，默认 mp3
	Speed  float64     `json:"speed,omitempty"`  // 语速（0.25-4.0），默认 1.0
}

// SynthesizeResult TTS 合成结果
type SynthesizeResult struct {
	Audio    []byte      `json:"-"`               // 音频数据
	Format   AudioFormat `json:"format"`          // 输出格式
	Duration float64     `json:"duration"`        // 音频时长（秒）
	Size     int         `json:"size"`            // 数据大小（字节）
}

// VoiceInfo 音色信息
type VoiceInfo struct {
	ID          string `json:"id"`          // 音色 ID
	Name        string `json:"name"`        // 音色名称
	Language    string `json:"language"`    // 主要语言
	Gender      string `json:"gender"`      // 性别: male/female/neutral
	Description string `json:"description"` // 描述
}

// Service 语音交互服务
//
// 整合 STT 和 TTS 能力，提供完整的语音交互流程。
type Service struct {
	stt STTProvider
	tts TTSProvider
}

// NewService 创建语音交互服务
//
// stt 和 tts 可以为 nil，此时对应功能不可用。
func NewService(stt STTProvider, tts TTSProvider) *Service {
	return &Service{stt: stt, tts: tts}
}

// Transcribe 语音转文本
//
// 使用配置的 STT Provider 将音频转为文本。
func (s *Service) Transcribe(ctx context.Context, audio []byte, opts TranscribeOptions) (*TranscribeResult, error) {
	if s.stt == nil {
		return nil, fmt.Errorf("STT Provider 未配置")
	}
	if len(audio) == 0 {
		return nil, fmt.Errorf("音频数据为空")
	}
	return s.stt.Transcribe(ctx, audio, opts)
}

// Synthesize 文本转语音
//
// 使用配置的 TTS Provider 将文本合成为音频。
func (s *Service) Synthesize(ctx context.Context, text string, opts SynthesizeOptions) (*SynthesizeResult, error) {
	if s.tts == nil {
		return nil, fmt.Errorf("TTS Provider 未配置")
	}
	if text == "" {
		return nil, fmt.Errorf("文本内容为空")
	}
	return s.tts.Synthesize(ctx, text, opts)
}

// HasSTT 检查是否配置了 STT Provider
func (s *Service) HasSTT() bool {
	return s.stt != nil
}

// HasTTS 检查是否配置了 TTS Provider
func (s *Service) HasTTS() bool {
	return s.tts != nil
}

// STTName 返回 STT Provider 名称
func (s *Service) STTName() string {
	if s.stt == nil {
		return ""
	}
	return s.stt.Name()
}

// TTSName 返回 TTS Provider 名称
func (s *Service) TTSName() string {
	if s.tts == nil {
		return ""
	}
	return s.tts.Name()
}
