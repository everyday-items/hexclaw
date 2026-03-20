package voice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/internal/testutil/httpmock"
)

// TestNewOpenAISTT_DefaultModel 测试默认模型名称
func TestNewOpenAISTT_DefaultModel(t *testing.T) {
	stt := NewOpenAISTT("test-key", "")
	if stt.model != "whisper-1" {
		t.Errorf("默认模型应为 whisper-1，实际为 %q", stt.model)
	}
	if stt.Name() != "openai-whisper" {
		t.Errorf("名称应为 openai-whisper，实际为 %q", stt.Name())
	}
}

// TestNewOpenAISTT_CustomModel 测试自定义模型名称
func TestNewOpenAISTT_CustomModel(t *testing.T) {
	stt := NewOpenAISTT("test-key", "whisper-large-v3")
	if stt.model != "whisper-large-v3" {
		t.Errorf("模型应为 whisper-large-v3，实际为 %q", stt.model)
	}
}

// TestOpenAISTT_WithBaseURL 测试自定义 BaseURL
func TestOpenAISTT_WithBaseURL(t *testing.T) {
	stt := NewOpenAISTT("test-key", "", STTWithBaseURL("https://custom.api.com/v1"))
	if stt.baseURL != "https://custom.api.com/v1" {
		t.Errorf("BaseURL 不匹配: %q", stt.baseURL)
	}
}

// TestOpenAISTT_SupportedFormats 测试支持的音频格式
func TestOpenAISTT_SupportedFormats(t *testing.T) {
	stt := NewOpenAISTT("test-key", "")
	formats := stt.SupportedFormats()
	if len(formats) == 0 {
		t.Fatal("支持的格式列表不应为空")
	}

	// 验证包含 mp3 和 wav
	formatSet := make(map[AudioFormat]bool)
	for _, f := range formats {
		formatSet[f] = true
	}
	if !formatSet[FormatMP3] {
		t.Error("应支持 mp3 格式")
	}
	if !formatSet[FormatWAV] {
		t.Error("应支持 wav 格式")
	}
}

// TestOpenAISTT_SupportedLanguages 测试支持的语言列表
func TestOpenAISTT_SupportedLanguages(t *testing.T) {
	stt := NewOpenAISTT("test-key", "")
	langs := stt.SupportedLanguages()
	if len(langs) == 0 {
		t.Fatal("支持的语言列表不应为空")
	}

	// 验证包含中文和英文
	langSet := make(map[string]bool)
	for _, l := range langs {
		langSet[l] = true
	}
	if !langSet["zh"] {
		t.Error("应支持中文")
	}
	if !langSet["en"] {
		t.Error("应支持英文")
	}
}

// TestOpenAISTT_Transcribe_Success 测试成功转录
func TestOpenAISTT_Transcribe_Success(t *testing.T) {
	// 创建模拟服务器
	stt := NewOpenAISTT("test-key", "",
		STTWithBaseURL("https://voice.test"),
		STTWithHTTPClient(httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 验证请求方法和路径
			if r.Method != "POST" {
				t.Errorf("请求方法应为 POST，实际为 %s", r.Method)
			}
			if r.URL.Path != "/audio/transcriptions" {
				t.Errorf("请求路径不匹配: %s", r.URL.Path)
			}

			// 验证 Authorization 头
			auth := r.Header.Get("Authorization")
			if auth != "Bearer test-key" {
				t.Errorf("Authorization 头不匹配: %s", auth)
			}

			// 验证 Content-Type 是 multipart
			ct := r.Header.Get("Content-Type")
			if ct == "" {
				t.Error("Content-Type 不应为空")
			}

			// 解析 multipart form
			if err := r.ParseMultipartForm(10 << 20); err != nil {
				t.Errorf("解析 multipart 失败: %v", err)
			}

			// 验证 model 字段
			model := r.FormValue("model")
			if model != "whisper-1" {
				t.Errorf("model 字段不匹配: %s", model)
			}

			// 验证 language 字段
			lang := r.FormValue("language")
			if lang != "zh" {
				t.Errorf("language 字段不匹配: %s", lang)
			}

			// 返回模拟响应
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(whisperResponse{
				Text:     "你好世界",
				Language: "chinese",
				Duration: 2.5,
			})
		}))),
	)
	result, err := stt.Transcribe(context.Background(), []byte("fake-audio"), TranscribeOptions{
		Language: "zh",
		Format:   FormatWAV,
	})
	if err != nil {
		t.Fatalf("转录失败: %v", err)
	}
	if result.Text != "你好世界" {
		t.Errorf("转录文本不匹配: %q", result.Text)
	}
	if result.Duration != 2.5 {
		t.Errorf("时长不匹配: %f", result.Duration)
	}
}

// TestOpenAISTT_Transcribe_EmptyAudio 测试空音频数据
func TestOpenAISTT_Transcribe_EmptyAudio(t *testing.T) {
	stt := NewOpenAISTT("test-key", "")
	_, err := stt.Transcribe(context.Background(), nil, TranscribeOptions{})
	if err == nil {
		t.Error("空音频数据应返回错误")
	}
	_, err = stt.Transcribe(context.Background(), []byte{}, TranscribeOptions{})
	if err == nil {
		t.Error("空音频数据应返回错误")
	}
}

// TestOpenAISTT_Transcribe_APIError 测试 API 返回错误
func TestOpenAISTT_Transcribe_APIError(t *testing.T) {
	stt := NewOpenAISTT("test-key", "",
		STTWithBaseURL("https://voice.test"),
		STTWithHTTPClient(httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid file format"}}`))
		}))),
	)
	_, err := stt.Transcribe(context.Background(), []byte("bad-audio"), TranscribeOptions{})
	if err == nil {
		t.Error("API 错误应传播")
	}
}

// TestOpenAISTT_Transcribe_WithPrompt 测试带提示词的转录
func TestOpenAISTT_Transcribe_WithPrompt(t *testing.T) {
	stt := NewOpenAISTT("test-key", "",
		STTWithBaseURL("https://voice.test"),
		STTWithHTTPClient(httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.ParseMultipartForm(10 << 20)
			prompt := r.FormValue("prompt")
			if prompt != "HexClaw Agent" {
				t.Errorf("prompt 字段不匹配: %s", prompt)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(whisperResponse{
				Text:     "启动 HexClaw Agent",
				Language: "chinese",
				Duration: 1.0,
			})
		}))),
	)
	result, err := stt.Transcribe(context.Background(), []byte("audio"), TranscribeOptions{
		Prompt: "HexClaw Agent",
	})
	if err != nil {
		t.Fatalf("转录失败: %v", err)
	}
	if result.Text != "启动 HexClaw Agent" {
		t.Errorf("转录文本不匹配: %q", result.Text)
	}
}

// ---

// TestNewOpenAITTS_DefaultModel 测试默认模型名称
func TestNewOpenAITTS_DefaultModel(t *testing.T) {
	tts := NewOpenAITTS("test-key", "")
	if tts.model != "tts-1" {
		t.Errorf("默认模型应为 tts-1，实际为 %q", tts.model)
	}
	if tts.Name() != "openai-tts" {
		t.Errorf("名称应为 openai-tts，实际为 %q", tts.Name())
	}
}

// TestNewOpenAITTS_CustomModel 测试自定义模型名称
func TestNewOpenAITTS_CustomModel(t *testing.T) {
	tts := NewOpenAITTS("test-key", "tts-1-hd")
	if tts.model != "tts-1-hd" {
		t.Errorf("模型应为 tts-1-hd，实际为 %q", tts.model)
	}
}

// TestOpenAITTS_WithBaseURL 测试自定义 BaseURL
func TestOpenAITTS_WithBaseURL(t *testing.T) {
	tts := NewOpenAITTS("test-key", "", TTSWithBaseURL("https://custom.api.com/v1"))
	if tts.baseURL != "https://custom.api.com/v1" {
		t.Errorf("BaseURL 不匹配: %q", tts.baseURL)
	}
}

// TestOpenAITTS_SupportedFormats 测试支持的输出格式
func TestOpenAITTS_SupportedFormats(t *testing.T) {
	tts := NewOpenAITTS("test-key", "")
	formats := tts.SupportedFormats()
	if len(formats) == 0 {
		t.Fatal("支持的格式列表不应为空")
	}

	formatSet := make(map[AudioFormat]bool)
	for _, f := range formats {
		formatSet[f] = true
	}
	if !formatSet[FormatMP3] {
		t.Error("应支持 mp3 格式")
	}
	if !formatSet[FormatWAV] {
		t.Error("应支持 wav 格式")
	}
	if !formatSet[FormatPCM] {
		t.Error("应支持 pcm 格式")
	}
}

// TestOpenAITTS_Voices 测试音色列表
func TestOpenAITTS_Voices(t *testing.T) {
	tts := NewOpenAITTS("test-key", "")
	voices := tts.Voices()

	// 应有 6 种音色
	if len(voices) != 6 {
		t.Fatalf("应有 6 种音色，实际有 %d 种", len(voices))
	}

	// 验证包含 alloy 和 nova
	voiceIDs := make(map[string]bool)
	for _, v := range voices {
		voiceIDs[v.ID] = true
		// 每个音色应有完整信息
		if v.Name == "" {
			t.Errorf("音色 %s 的名称不应为空", v.ID)
		}
		if v.Language == "" {
			t.Errorf("音色 %s 的语言不应为空", v.ID)
		}
		if v.Gender == "" {
			t.Errorf("音色 %s 的性别不应为空", v.ID)
		}
	}
	if !voiceIDs["alloy"] {
		t.Error("应包含 alloy 音色")
	}
	if !voiceIDs["nova"] {
		t.Error("应包含 nova 音色")
	}
}

// TestOpenAITTS_Synthesize_Success 测试成功合成
func TestOpenAITTS_Synthesize_Success(t *testing.T) {
	fakeAudio := []byte("fake-mp3-audio-data-bytes")

	tts := NewOpenAITTS("test-key", "",
		TTSWithBaseURL("https://voice.test"),
		TTSWithHTTPClient(httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 验证请求方法和路径
			if r.Method != "POST" {
				t.Errorf("请求方法应为 POST，实际为 %s", r.Method)
			}
			if r.URL.Path != "/audio/speech" {
				t.Errorf("请求路径不匹配: %s", r.URL.Path)
			}

			// 验证 Authorization 头
			auth := r.Header.Get("Authorization")
			if auth != "Bearer test-key" {
				t.Errorf("Authorization 头不匹配: %s", auth)
			}

			// 验证 Content-Type
			ct := r.Header.Get("Content-Type")
			if ct != "application/json" {
				t.Errorf("Content-Type 应为 application/json，实际为 %s", ct)
			}

			// 解析请求体
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("解析请求体失败: %v", err)
			}

			// 验证请求参数
			if req["model"] != "tts-1" {
				t.Errorf("model 不匹配: %v", req["model"])
			}
			if req["input"] != "你好世界" {
				t.Errorf("input 不匹配: %v", req["input"])
			}
			if req["voice"] != "nova" {
				t.Errorf("voice 不匹配: %v", req["voice"])
			}

			// 返回模拟音频数据
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(fakeAudio)
		}))),
	)
	result, err := tts.Synthesize(context.Background(), "你好世界", SynthesizeOptions{
		Voice:  "nova",
		Format: FormatMP3,
		Speed:  1.0,
	})
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	if result.Format != FormatMP3 {
		t.Errorf("格式不匹配: %q", result.Format)
	}
	if len(result.Audio) != len(fakeAudio) {
		t.Errorf("音频数据大小不匹配: %d vs %d", len(result.Audio), len(fakeAudio))
	}
	if result.Size != len(fakeAudio) {
		t.Errorf("Size 字段不匹配: %d", result.Size)
	}
}

// TestOpenAITTS_Synthesize_EmptyText 测试空文本
func TestOpenAITTS_Synthesize_EmptyText(t *testing.T) {
	tts := NewOpenAITTS("test-key", "")
	_, err := tts.Synthesize(context.Background(), "", SynthesizeOptions{})
	if err == nil {
		t.Error("空文本应返回错误")
	}
}

// TestOpenAITTS_Synthesize_Defaults 测试默认参数
func TestOpenAITTS_Synthesize_Defaults(t *testing.T) {
	tts := NewOpenAITTS("test-key", "",
		TTSWithBaseURL("https://voice.test"),
		TTSWithHTTPClient(httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)

			// 验证默认音色
			if req["voice"] != "alloy" {
				t.Errorf("默认音色应为 alloy，实际为 %v", req["voice"])
			}
			// 验证默认格式
			if req["response_format"] != "mp3" {
				t.Errorf("默认格式应为 mp3，实际为 %v", req["response_format"])
			}
			// 验证默认语速
			speed, ok := req["speed"].(float64)
			if !ok || speed != 1.0 {
				t.Errorf("默认语速应为 1.0，实际为 %v", req["speed"])
			}

			_, _ = w.Write([]byte("audio"))
		}))),
	)
	_, err := tts.Synthesize(context.Background(), "test", SynthesizeOptions{})
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
}

// TestOpenAITTS_Synthesize_APIError 测试 API 返回错误
func TestOpenAITTS_Synthesize_APIError(t *testing.T) {
	tts := NewOpenAITTS("test-key", "",
		TTSWithBaseURL("https://voice.test"),
		TTSWithHTTPClient(httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Rate limit exceeded"}}`))
		}))),
	)
	_, err := tts.Synthesize(context.Background(), "test", SynthesizeOptions{})
	if err == nil {
		t.Error("API 错误应传播")
	}
}

// TestOpenAISTT_Transcribe_ContextCanceled 测试上下文取消
func TestOpenAISTT_Transcribe_ContextCanceled(t *testing.T) {
	stt := NewOpenAISTT("test-key", "",
		STTWithBaseURL("https://voice.test"),
		STTWithHTTPClient(&http.Client{
			Transport: httpmock.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}),
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	_, err := stt.Transcribe(ctx, []byte("audio"), TranscribeOptions{})
	if err == nil {
		t.Error("上下文取消时应返回错误")
	}
}

// TestOpenAITTS_Synthesize_ContextCanceled 测试上下文取消
func TestOpenAITTS_Synthesize_ContextCanceled(t *testing.T) {
	tts := NewOpenAITTS("test-key", "",
		TTSWithBaseURL("https://voice.test"),
		TTSWithHTTPClient(&http.Client{
			Transport: httpmock.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}),
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tts.Synthesize(ctx, "test", SynthesizeOptions{})
	if err == nil {
		t.Error("上下文取消时应返回错误")
	}
}

// TestOpenAISTT_WithHTTPClient 测试自定义 HTTP 客户端
func TestOpenAISTT_WithHTTPClient(t *testing.T) {
	client := &http.Client{Timeout: 30}
	stt := NewOpenAISTT("test-key", "", STTWithHTTPClient(client))
	if stt.client != client {
		t.Error("HTTP 客户端应为自定义客户端")
	}
}

// TestOpenAITTS_WithHTTPClient 测试自定义 HTTP 客户端
func TestOpenAITTS_WithHTTPClient(t *testing.T) {
	client := &http.Client{Timeout: 30}
	tts := NewOpenAITTS("test-key", "", TTSWithHTTPClient(client))
	if tts.client != client {
		t.Error("HTTP 客户端应为自定义客户端")
	}
}
