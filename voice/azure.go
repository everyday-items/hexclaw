package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AzureSTT Azure 语音转文本 Provider
//
// 使用 Azure Cognitive Services Speech REST API。
// 支持 100+ 语言，高精度中文识别。
type AzureSTT struct {
	subscriptionKey string
	region          string
	language        string
	client          *http.Client
}

// AzureSTTOption 配置选项
type AzureSTTOption func(*AzureSTT)

// AzureSTTWithLanguage 设置默认语言
func AzureSTTWithLanguage(lang string) AzureSTTOption {
	return func(s *AzureSTT) { s.language = lang }
}

// NewAzureSTT 创建 Azure STT Provider
func NewAzureSTT(subscriptionKey, region string, opts ...AzureSTTOption) *AzureSTT {
	s := &AzureSTT{
		subscriptionKey: subscriptionKey,
		region:          region,
		language:        "zh-CN",
		client:          &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *AzureSTT) Name() string { return "azure-stt" }

func (s *AzureSTT) SupportedFormats() []AudioFormat {
	return []AudioFormat{FormatWAV, FormatMP3, FormatOGG, FormatFLAC}
}

func (s *AzureSTT) SupportedLanguages() []string {
	return []string{"zh-CN", "en-US", "ja-JP", "ko-KR", "fr-FR", "de-DE", "es-ES"}
}

func (s *AzureSTT) Transcribe(ctx context.Context, audio []byte, opts TranscribeOptions) (*TranscribeResult, error) {
	if len(audio) == 0 {
		return nil, fmt.Errorf("音频数据为空")
	}

	lang := opts.Language
	if lang == "" {
		lang = s.language
	}

	url := fmt.Sprintf("https://%s.stt.speech.microsoft.com/speech/recognition/conversation/cognitiveservices/v1?language=%s&format=detailed",
		s.region, lang)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(audio))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", s.subscriptionKey)
	contentType := "audio/wav"
	switch opts.Format {
	case "mp3":
		contentType = "audio/mpeg"
	case "ogg":
		contentType = "audio/ogg"
	case "flac":
		contentType = "audio/flac"
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Azure STT 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("azure STT 返回 %d: %s", resp.StatusCode, string(body))
	}

	var result azureSTTResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	confidence := 0.0
	if len(result.NBest) > 0 {
		confidence = result.NBest[0].Confidence
	}

	return &TranscribeResult{
		Text:       result.DisplayText,
		Language:   lang,
		Duration:   float64(result.Duration) / 10000000, // ticks to seconds
		Confidence: confidence,
	}, nil
}

type azureSTTResponse struct {
	RecognitionStatus string         `json:"RecognitionStatus"`
	DisplayText       string         `json:"DisplayText"`
	Duration          int64          `json:"Duration"`
	NBest             []azureNBest   `json:"NBest"`
}

type azureNBest struct {
	Confidence float64 `json:"Confidence"`
	Display    string  `json:"Display"`
}

// AzureTTS Azure 文本转语音 Provider
//
// 使用 Azure Cognitive Services TTS REST API。
// 支持 400+ 神经网络语音，特别适合中文。
type AzureTTS struct {
	subscriptionKey string
	region          string
	defaultVoice    string
	client          *http.Client
}

// AzureTTSOption 配置选项
type AzureTTSOption func(*AzureTTS)

// AzureTTSWithVoice 设置默认音色
func AzureTTSWithVoice(voice string) AzureTTSOption {
	return func(t *AzureTTS) { t.defaultVoice = voice }
}

// NewAzureTTS 创建 Azure TTS Provider
func NewAzureTTS(subscriptionKey, region string, opts ...AzureTTSOption) *AzureTTS {
	t := &AzureTTS{
		subscriptionKey: subscriptionKey,
		region:          region,
		defaultVoice:    "zh-CN-XiaoxiaoNeural",
		client:          &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *AzureTTS) Name() string { return "azure-tts" }

func (t *AzureTTS) SupportedFormats() []AudioFormat {
	return []AudioFormat{FormatMP3, FormatWAV, FormatOGG}
}

func (t *AzureTTS) Voices() []VoiceInfo {
	return []VoiceInfo{
		{ID: "zh-CN-XiaoxiaoNeural", Name: "晓晓", Language: "zh-CN", Gender: "female", Description: "温暖亲和的中文女声"},
		{ID: "zh-CN-YunxiNeural", Name: "云希", Language: "zh-CN", Gender: "male", Description: "阳光开朗的中文男声"},
		{ID: "zh-CN-YunjianNeural", Name: "云健", Language: "zh-CN", Gender: "male", Description: "沉稳大气的中文男声"},
		{ID: "zh-CN-XiaoyiNeural", Name: "晓艺", Language: "zh-CN", Gender: "female", Description: "活泼可爱的中文女声"},
		{ID: "en-US-JennyNeural", Name: "Jenny", Language: "en-US", Gender: "female", Description: "自然流畅的英文女声"},
		{ID: "en-US-GuyNeural", Name: "Guy", Language: "en-US", Gender: "male", Description: "专业稳重的英文男声"},
		{ID: "ja-JP-NanamiNeural", Name: "七海", Language: "ja-JP", Gender: "female", Description: "温柔的日文女声"},
	}
}

func (t *AzureTTS) Synthesize(ctx context.Context, text string, opts SynthesizeOptions) (*SynthesizeResult, error) {
	if text == "" {
		return nil, fmt.Errorf("文本内容为空")
	}

	voice := opts.Voice
	if voice == "" {
		voice = t.defaultVoice
	}
	format := opts.Format
	if format == "" {
		format = FormatMP3
	}

	// 构建 SSML（转义用户文本防止 SSML 注入）
	ssml := fmt.Sprintf(`<speak version='1.0' xml:lang='zh-CN'>
		<voice name='%s'>%s</voice>
	</speak>`, escapeXML(voice), escapeXML(text))

	url := fmt.Sprintf("https://%s.tts.speech.microsoft.com/cognitiveservices/v1", t.region)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBufferString(ssml))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", t.subscriptionKey)
	req.Header.Set("Content-Type", "application/ssml+xml")

	outputFormat := "audio-16khz-128kbitrate-mono-mp3"
	switch format {
	case FormatWAV:
		outputFormat = "riff-16khz-16bit-mono-pcm"
	case FormatOGG:
		outputFormat = "ogg-16khz-16bit-mono-opus"
	}
	req.Header.Set("X-Microsoft-OutputFormat", outputFormat)

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Azure TTS 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("azure TTS 返回 %d: %s", resp.StatusCode, string(body))
	}

	audio, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20)) // 50MB 限制
	if err != nil {
		return nil, fmt.Errorf("读取音频数据失败: %w", err)
	}

	return &SynthesizeResult{
		Audio:  audio,
		Format: format,
		Size:   len(audio),
	}, nil
}

// escapeXML 转义 XML 特殊字符（防止 SSML 注入）
func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
