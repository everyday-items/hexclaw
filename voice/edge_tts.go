package voice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// EdgeTTS Microsoft Edge 免费 TTS Provider
//
// 使用 Edge 浏览器内置的 TTS 服务，免费、无需 API Key。
// 特别适合中文场景，音质接近 Azure Neural TTS。
type EdgeTTS struct {
	defaultVoice string
	client       *http.Client
}

// EdgeTTSOption 配置选项
type EdgeTTSOption func(*EdgeTTS)

// EdgeTTSWithVoice 设置默认音色
func EdgeTTSWithVoice(voice string) EdgeTTSOption {
	return func(t *EdgeTTS) { t.defaultVoice = voice }
}

// NewEdgeTTS 创建 Edge TTS Provider（免费，无需 Key）
func NewEdgeTTS(opts ...EdgeTTSOption) *EdgeTTS {
	t := &EdgeTTS{
		defaultVoice: "zh-CN-XiaoxiaoNeural",
		client:       &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *EdgeTTS) Name() string { return "edge-tts" }

func (t *EdgeTTS) SupportedFormats() []AudioFormat {
	return []AudioFormat{FormatMP3}
}

func (t *EdgeTTS) Voices() []VoiceInfo {
	return []VoiceInfo{
		{ID: "zh-CN-XiaoxiaoNeural", Name: "晓晓", Language: "zh-CN", Gender: "female", Description: "温暖亲和"},
		{ID: "zh-CN-YunxiNeural", Name: "云希", Language: "zh-CN", Gender: "male", Description: "阳光开朗"},
		{ID: "zh-CN-YunjianNeural", Name: "云健", Language: "zh-CN", Gender: "male", Description: "沉稳大气"},
		{ID: "zh-CN-XiaoyiNeural", Name: "晓艺", Language: "zh-CN", Gender: "female", Description: "活泼可爱"},
		{ID: "zh-CN-YunyangNeural", Name: "云扬", Language: "zh-CN", Gender: "male", Description: "新闻播报"},
		{ID: "zh-TW-HsiaoChenNeural", Name: "曉臻", Language: "zh-TW", Gender: "female", Description: "台湾女声"},
		{ID: "en-US-JennyNeural", Name: "Jenny", Language: "en-US", Gender: "female", Description: "自然英文女声"},
		{ID: "en-US-GuyNeural", Name: "Guy", Language: "en-US", Gender: "male", Description: "稳重英文男声"},
		{ID: "ja-JP-NanamiNeural", Name: "七海", Language: "ja-JP", Gender: "female", Description: "温柔日文女声"},
		{ID: "ko-KR-SunHiNeural", Name: "선히", Language: "ko-KR", Gender: "female", Description: "韩文女声"},
	}
}

// Synthesize 通过 Edge TTS HTTP 接口合成语音
//
// 使用 Edge 浏览器的 TTS 端点，返回 MP3 格式音频。
func (t *EdgeTTS) Synthesize(ctx context.Context, text string, opts SynthesizeOptions) (*SynthesizeResult, error) {
	if text == "" {
		return nil, fmt.Errorf("文本内容为空")
	}

	voiceName := opts.Voice
	if voiceName == "" {
		voiceName = t.defaultVoice
	}

	speed := opts.Speed
	if speed <= 0 {
		speed = 1.0
	}

	// 构建 SSML
	ratePercent := int((speed - 1.0) * 100)
	rateStr := fmt.Sprintf("%+d%%", ratePercent)

	ssml := fmt.Sprintf(`<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='zh-CN'>
		<voice name='%s'>
			<prosody rate='%s'>%s</prosody>
		</voice>
	</speak>`, escapeXML(voiceName), escapeXML(rateStr), escapeXML(text))

	// Edge TTS 使用与 Azure 兼容的端点
	endpoint := "https://eastus.api.speech.microsoft.com/cognitiveservices/v1"

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBufferString(ssml))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/ssml+xml")
	req.Header.Set("X-Microsoft-OutputFormat", "audio-16khz-128kbitrate-mono-mp3")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Origin", "https://azure.microsoft.com")
	req.Header.Set("Referer", "https://azure.microsoft.com/")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Edge TTS 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("edge TTS 返回 %d: %s", resp.StatusCode, string(body))
	}

	audio, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20)) // 50MB 限制
	if err != nil {
		return nil, fmt.Errorf("读取音频数据失败: %w", err)
	}

	return &SynthesizeResult{
		Audio:  audio,
		Format: FormatMP3,
		Size:   len(audio),
	}, nil
}

