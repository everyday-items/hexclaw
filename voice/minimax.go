// minimax.go 实现 v0.4.0 F7 MiniMax TTS Provider。
//
// MiniMax T2A v2 API 文档参考：https://platform.minimaxi.com/document/T2A%20V2
// 端点：POST {base_url}/v1/t2a_v2?GroupId={group_id}
// 请求体（关键字段）：
//
//	{
//	  "model":          "speech-01-turbo",
//	  "text":           "...",
//	  "stream":         false,
//	  "voice_setting":  {"voice_id": "male-qn-qingse", "speed": 1.0, "vol": 1.0, "pitch": 0},
//	  "audio_setting":  {"sample_rate": 32000, "bitrate": 128000, "format": "mp3", "channel": 1}
//	}
//
// 响应（JSON）携带 hex 编码的 audio：
//
//	{"data": {"audio": "<hex>"}, "extra_info": {...}, "trace_id": "..."}
//
// 本实现不引入 stream 模式（v0.4.0 范围内只做 oneshot）；voice_id 默认 male-qn-qingse，
// 可通过 SynthesizeOptions.Voice 覆盖。
package voice

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MiniMaxTTS 实现 TTSProvider。
type MiniMaxTTS struct {
	apiKey  string
	groupID string
	baseURL string
	model   string
	client  *http.Client
}

// MiniMaxTTSOption 配置 MiniMaxTTS。
type MiniMaxTTSOption func(*MiniMaxTTS)

// WithMiniMaxBaseURL 覆盖默认 base_url（用于私有部署 / mock）。
func WithMiniMaxBaseURL(url string) MiniMaxTTSOption {
	return func(t *MiniMaxTTS) { t.baseURL = url }
}

// WithMiniMaxModel 选择模型（默认 speech-01-turbo）。
func WithMiniMaxModel(model string) MiniMaxTTSOption {
	return func(t *MiniMaxTTS) { t.model = model }
}

// WithMiniMaxHTTPClient 注入自定义 HTTP client（测试 mock 时常用）。
func WithMiniMaxHTTPClient(c *http.Client) MiniMaxTTSOption {
	return func(t *MiniMaxTTS) { t.client = c }
}

// NewMiniMaxTTS 构造 MiniMax TTS。apiKey/groupID 不能为空。
func NewMiniMaxTTS(apiKey, groupID string, opts ...MiniMaxTTSOption) *MiniMaxTTS {
	t := &MiniMaxTTS{
		apiKey:  apiKey,
		groupID: groupID,
		baseURL: "https://api.minimaxi.com",
		model:   "speech-01-turbo",
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name 返回 "minimax-tts"。
func (t *MiniMaxTTS) Name() string { return "minimax-tts" }

// Voices 返回常见预置音色（截至 2025-Q1；用户可通过 SynthesizeOptions.Voice 用任意 voice_id）。
func (t *MiniMaxTTS) Voices() []VoiceInfo {
	return []VoiceInfo{
		{ID: "male-qn-qingse", Name: "青涩男声", Language: "zh", Gender: "male"},
		{ID: "male-qn-jingying", Name: "精英男声", Language: "zh", Gender: "male"},
		{ID: "female-shaonv", Name: "少女女声", Language: "zh", Gender: "female"},
		{ID: "female-tianmei", Name: "甜美女声", Language: "zh", Gender: "female"},
		{ID: "presenter_male", Name: "主持男声", Language: "zh", Gender: "male"},
	}
}

// SupportedFormats 仅返回 MP3（API 也支持 pcm/flac，但 oneshot 默认走 mp3）。
func (t *MiniMaxTTS) SupportedFormats() []AudioFormat {
	return []AudioFormat{FormatMP3}
}

// Synthesize 调用 T2A v2 接口合成语音。
func (t *MiniMaxTTS) Synthesize(ctx context.Context, text string, opts SynthesizeOptions) (*SynthesizeResult, error) {
	if t.apiKey == "" || t.groupID == "" {
		return nil, fmt.Errorf("minimax: apiKey/groupID 未配置")
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("minimax: text empty")
	}

	voiceID := strings.TrimSpace(opts.Voice)
	if voiceID == "" {
		voiceID = "male-qn-qingse"
	}
	speed := opts.Speed
	if speed <= 0 {
		speed = 1.0
	}

	body := map[string]any{
		"model":  t.model,
		"text":   text,
		"stream": false,
		"voice_setting": map[string]any{
			"voice_id": voiceID,
			"speed":    speed,
			"vol":      1.0,
			"pitch":    0,
		},
		"audio_setting": map[string]any{
			"sample_rate": 32000,
			"bitrate":     128000,
			"format":      "mp3",
			"channel":     1,
		},
	}
	payload, _ := json.Marshal(body)

	url := fmt.Sprintf("%s/v1/t2a_v2?GroupId=%s", strings.TrimRight(t.baseURL, "/"), t.groupID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("minimax: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimax: http: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("minimax: http %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		Data struct {
			Audio string `json:"audio"`
		} `json:"data"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("minimax: parse: %w body=%s", err, string(respBody))
	}
	if parsed.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("minimax: api error %d: %s", parsed.BaseResp.StatusCode, parsed.BaseResp.StatusMsg)
	}
	if parsed.Data.Audio == "" {
		return nil, fmt.Errorf("minimax: empty audio in response")
	}

	audio, err := hex.DecodeString(parsed.Data.Audio)
	if err != nil {
		return nil, fmt.Errorf("minimax: decode hex audio: %w", err)
	}
	return &SynthesizeResult{
		Audio:  audio,
		Format: FormatMP3,
		Size:   len(audio),
	}, nil
}
