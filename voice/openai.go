package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// OpenAISTT OpenAI Whisper 语音转文本 Provider
//
// 使用 OpenAI /v1/audio/transcriptions API。
// 兼容所有 OpenAI API 兼容的 STT 服务（如私有部署、中转服务等）。
//
// 支持的音频格式: mp3, wav, ogg, flac
// 支持的语言: 中文、英文、日文、韩文等多种语言
//
// 用法:
//
//	stt := voice.NewOpenAISTT("sk-xxx", "whisper-1")
//	result, err := stt.Transcribe(ctx, audioData, voice.TranscribeOptions{Language: "zh"})
type OpenAISTT struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// STTOption OpenAI STT 配置选项函数
type STTOption func(*OpenAISTT)

// STTWithBaseURL 设置自定义 API 端点
//
// 用于连接 OpenAI API 兼容的中转/私有部署服务。
// 示例: STTWithBaseURL("https://my-proxy.example.com/v1")
func STTWithBaseURL(url string) STTOption {
	return func(s *OpenAISTT) { s.baseURL = url }
}

// STTWithHTTPClient 设置自定义 HTTP 客户端
//
// 用于自定义超时、代理等 HTTP 配置。
func STTWithHTTPClient(client *http.Client) STTOption {
	return func(s *OpenAISTT) { s.client = client }
}

// NewOpenAISTT 创建 OpenAI Whisper STT Provider
//
// 参数:
//   - apiKey: OpenAI API Key
//   - model: 模型名称，为空则默认 "whisper-1"
//   - opts: 可选配置（自定义 BaseURL、HTTP 客户端等）
func NewOpenAISTT(apiKey, model string, opts ...STTOption) *OpenAISTT {
	if model == "" {
		model = "whisper-1"
	}
	s := &OpenAISTT{
		apiKey:  apiKey,
		model:   model,
		baseURL: "https://api.openai.com/v1",
		client:  &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Name 返回 Provider 名称
func (s *OpenAISTT) Name() string { return "openai-whisper" }

// SupportedFormats 返回支持的音频格式列表
func (s *OpenAISTT) SupportedFormats() []AudioFormat {
	return []AudioFormat{FormatMP3, FormatWAV, FormatOGG, FormatFLAC}
}

// SupportedLanguages 返回支持的语言代码列表
//
// 遵循 ISO-639-1 语言代码标准。
// Whisper 实际支持更多语言，这里列出常用语言。
func (s *OpenAISTT) SupportedLanguages() []string {
	return []string{"zh", "en", "ja", "ko", "fr", "de", "es", "it", "pt", "ru", "ar"}
}

// Transcribe 将音频转为文本
//
// 通过 OpenAI /v1/audio/transcriptions API 将音频数据转录为文本。
// 使用 multipart/form-data 上传音频文件。
//
// 参数:
//   - audio: 音频数据（支持 mp3, wav, ogg, flac 格式）
//   - opts: 转录选项（语言、格式、提示词等）
//
// 返回:
//   - TranscribeResult: 包含转录文本、检测到的语言、音频时长
//   - error: 请求失败或解析失败时返回错误
func (s *OpenAISTT) Transcribe(ctx context.Context, audio []byte, opts TranscribeOptions) (*TranscribeResult, error) {
	if len(audio) == 0 {
		return nil, fmt.Errorf("音频数据为空")
	}

	// 构建 multipart form 请求体
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// 添加音频文件字段
	// 根据指定的格式确定文件扩展名，默认为 wav
	ext := "wav"
	if opts.Format != "" {
		ext = string(opts.Format)
	}
	part, err := w.CreateFormFile("file", "audio."+ext)
	if err != nil {
		return nil, fmt.Errorf("创建 multipart 失败: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return nil, fmt.Errorf("写入音频数据失败: %w", err)
	}

	// 添加模型参数
	_ = w.WriteField("model", s.model)

	// 如果指定了语言，添加 language 字段（帮助提升识别准确率）
	if opts.Language != "" {
		_ = w.WriteField("language", opts.Language)
	}

	// 如果指定了提示词，添加 prompt 字段（帮助识别特定术语）
	if opts.Prompt != "" {
		_ = w.WriteField("prompt", opts.Prompt)
	}

	// 请求 verbose_json 格式以获取语言和时长信息
	_ = w.WriteField("response_format", "verbose_json")
	w.Close()

	// 发送 HTTP 请求
	url := s.baseURL + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 STT API 失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("STT API 返回 %d: %s", resp.StatusCode, string(body))
	}

	// 解析响应
	var result whisperResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 STT 响应失败: %w", err)
	}

	return &TranscribeResult{
		Text:     result.Text,
		Language: result.Language,
		Duration: result.Duration,
	}, nil
}

// whisperResponse OpenAI Whisper API verbose_json 响应结构
type whisperResponse struct {
	Text     string  `json:"text"`
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
}

// ---

// OpenAITTS OpenAI 文本转语音 Provider
//
// 使用 OpenAI /v1/audio/speech API 将文本合成为音频。
// 兼容所有 OpenAI API 兼容的 TTS 服务。
//
// 支持 6 种内置音色: alloy, echo, fable, onyx, nova, shimmer
// 支持的输出格式: mp3, ogg, flac, wav, pcm
//
// 用法:
//
//	tts := voice.NewOpenAITTS("sk-xxx", "tts-1")
//	result, err := tts.Synthesize(ctx, "你好世界", voice.SynthesizeOptions{Voice: "nova"})
type OpenAITTS struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// TTSOption OpenAI TTS 配置选项函数
type TTSOption func(*OpenAITTS)

// TTSWithBaseURL 设置自定义 API 端点
//
// 用于连接 OpenAI API 兼容的中转/私有部署服务。
func TTSWithBaseURL(url string) TTSOption {
	return func(t *OpenAITTS) { t.baseURL = url }
}

// TTSWithHTTPClient 设置自定义 HTTP 客户端
//
// 用于自定义超时、代理等 HTTP 配置。
func TTSWithHTTPClient(client *http.Client) TTSOption {
	return func(t *OpenAITTS) { t.client = client }
}

// NewOpenAITTS 创建 OpenAI TTS Provider
//
// 参数:
//   - apiKey: OpenAI API Key
//   - model: 模型名称，为空则默认 "tts-1"（也支持 "tts-1-hd" 高质量版本）
//   - opts: 可选配置（自定义 BaseURL、HTTP 客户端等）
func NewOpenAITTS(apiKey, model string, opts ...TTSOption) *OpenAITTS {
	if model == "" {
		model = "tts-1"
	}
	t := &OpenAITTS{
		apiKey:  apiKey,
		model:   model,
		baseURL: "https://api.openai.com/v1",
		client:  &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name 返回 Provider 名称
func (t *OpenAITTS) Name() string { return "openai-tts" }

// SupportedFormats 返回支持的输出音频格式
func (t *OpenAITTS) SupportedFormats() []AudioFormat {
	return []AudioFormat{FormatMP3, FormatOGG, FormatFLAC, FormatWAV, FormatPCM}
}

// Voices 返回可用的音色列表
//
// OpenAI TTS 提供 6 种内置音色，均支持多语言。
func (t *OpenAITTS) Voices() []VoiceInfo {
	return []VoiceInfo{
		{ID: "alloy", Name: "Alloy", Language: "multi", Gender: "neutral", Description: "中性温和"},
		{ID: "echo", Name: "Echo", Language: "multi", Gender: "male", Description: "沉稳男声"},
		{ID: "fable", Name: "Fable", Language: "multi", Gender: "female", Description: "叙事女声"},
		{ID: "onyx", Name: "Onyx", Language: "multi", Gender: "male", Description: "深沉男声"},
		{ID: "nova", Name: "Nova", Language: "multi", Gender: "female", Description: "活泼女声"},
		{ID: "shimmer", Name: "Shimmer", Language: "multi", Gender: "female", Description: "柔和女声"},
	}
}

// Synthesize 将文本转为语音
//
// 通过 OpenAI /v1/audio/speech API 将文本合成为音频数据。
//
// 参数:
//   - text: 要合成的文本（不能为空）
//   - opts: 合成选项（音色、格式、语速等）
//
// 返回:
//   - SynthesizeResult: 包含音频数据、格式、大小
//   - error: 请求失败时返回错误
func (t *OpenAITTS) Synthesize(ctx context.Context, text string, opts SynthesizeOptions) (*SynthesizeResult, error) {
	if text == "" {
		return nil, fmt.Errorf("文本内容为空")
	}

	// 设置默认值
	voice := opts.Voice
	if voice == "" {
		voice = "alloy"
	}
	format := opts.Format
	if format == "" {
		format = FormatMP3
	}
	speed := opts.Speed
	if speed <= 0 {
		speed = 1.0
	}

	// 构建 JSON 请求体
	reqBody, _ := json.Marshal(map[string]any{
		"model":           t.model,
		"input":           text,
		"voice":           voice,
		"response_format": string(format),
		"speed":           speed,
	})

	// 发送 HTTP 请求
	url := t.baseURL + "/audio/speech"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 TTS API 失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("TTS API 返回 %d: %s", resp.StatusCode, string(body))
	}

	// 读取音频数据
	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取音频数据失败: %w", err)
	}

	return &SynthesizeResult{
		Audio:  audio,
		Format: format,
		Size:   len(audio),
	}, nil
}
