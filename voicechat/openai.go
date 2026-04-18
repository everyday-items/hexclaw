package voicechat

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

// OpenAI gpt-4o-audio-preview 协议：
//   POST {base}/chat/completions
//   {
//     "model": "gpt-4o-audio-preview",
//     "modalities": ["text", "audio"],
//     "audio": { "voice": "alloy", "format": "wav" },
//     "messages": [
//       { "role": "user", "content": [
//           { "type": "input_audio", "input_audio": { "data": "<b64>", "format": "wav" } }
//       ]}
//     ]
//   }
//
// 响应：message.audio.{ data, transcript, id }，message.audio.transcript = 模型回应文本
//
// 文档：https://platform.openai.com/docs/guides/audio

type openAIVoiceChat struct {
	apiKey  string
	baseURL string
	models  []string
	httpc   *http.Client
	timeout time.Duration
}

// NewOpenAIVoiceChat 创建 OpenAI gpt-4o-audio Provider。
func NewOpenAIVoiceChat(apiKey, baseURL string) Provider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &openAIVoiceChat{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		models:  []string{"gpt-4o-audio-preview", "gpt-4o-mini-audio-preview"},
		httpc:   http.DefaultClient,
		timeout: 90 * time.Second,
	}
}

func (p *openAIVoiceChat) Name() string              { return "openai-gpt4o-audio" }
func (p *openAIVoiceChat) SupportedModels() []string { return p.models }

type oaChatReq struct {
	Model      string         `json:"model"`
	Modalities []string       `json:"modalities"`
	Audio      *oaAudioConfig `json:"audio,omitempty"`
	Messages   []oaMessage    `json:"messages"`
}

type oaAudioConfig struct {
	Voice  string `json:"voice"`
	Format string `json:"format"`
}

type oaMessage struct {
	Role    string   `json:"role"`
	Content []oaPart `json:"content,omitempty"`
}

type oaPart struct {
	Type       string        `json:"type"`
	Text       string        `json:"text,omitempty"`
	InputAudio *oaInputAudio `json:"input_audio,omitempty"`
}

type oaInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type oaChatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Audio   *struct {
				ID         string `json:"id"`
				Data       string `json:"data"`
				Transcript string `json:"transcript"`
			} `json:"audio"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (p *openAIVoiceChat) Chat(ctx context.Context, req Request) (*Result, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("%s: 未配置 API Key", p.Name())
	}

	model := req.Model
	if model == "" {
		model = p.models[0]
	}
	voice := req.Voice
	if voice == "" {
		voice = "alloy"
	}
	format := req.Format
	if format == "" {
		format = "wav"
	}

	messages := []oaMessage{}
	if req.System != "" {
		messages = append(messages, oaMessage{Role: "system", Content: []oaPart{{Type: "text", Text: req.System}}})
	}
	// 历史轮次（简化：仅文本，复用音频 ID 留作 v0.5）
	for _, h := range req.History {
		if h.Text == "" {
			continue
		}
		messages = append(messages, oaMessage{Role: h.Role, Content: []oaPart{{Type: "text", Text: h.Text}}})
	}
	// 当前用户输入
	userParts := []oaPart{}
	if req.Text != "" {
		userParts = append(userParts, oaPart{Type: "text", Text: req.Text})
	}
	if req.AudioB64 != "" {
		userParts = append(userParts, oaPart{
			Type:       "input_audio",
			InputAudio: &oaInputAudio{Data: req.AudioB64, Format: format},
		})
	}
	messages = append(messages, oaMessage{Role: "user", Content: userParts})

	body := oaChatReq{
		Model:      model,
		Modalities: []string{"text", "audio"},
		Audio:      &oaAudioConfig{Voice: voice, Format: format},
		Messages:   messages,
	}
	payload, _ := json.Marshal(body)

	rctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(rctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	start := time.Now()
	resp, err := p.httpc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))

	if resp.StatusCode >= 400 {
		var er oaChatResp
		if json.Unmarshal(respBody, &er) == nil && er.Error != nil {
			return nil, fmt.Errorf("%s HTTP %d: %s", p.Name(), resp.StatusCode, er.Error.Message)
		}
		return nil, fmt.Errorf("%s HTTP %d", p.Name(), resp.StatusCode)
	}

	var parsed oaChatResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("%s: %s", p.Name(), parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("%s: 无回应", p.Name())
	}
	msg := parsed.Choices[0].Message
	res := &Result{
		Provider:   p.Name(),
		Model:      model,
		Format:     format,
		Transcript: msg.Content,
		UsageMs:    time.Since(start).Milliseconds(),
	}
	if msg.Audio != nil {
		res.ResponseB64 = msg.Audio.Data
		if msg.Audio.Transcript != "" {
			res.Transcript = msg.Audio.Transcript
		}
	}
	return res, nil
}
