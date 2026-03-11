package voice

import (
	"context"
	"fmt"
	"testing"
)

// mockSTT 模拟 STT Provider
type mockSTT struct {
	name      string
	result    *TranscribeResult
	err       error
}

func (m *mockSTT) Name() string { return m.name }
func (m *mockSTT) Transcribe(_ context.Context, _ []byte, _ TranscribeOptions) (*TranscribeResult, error) {
	return m.result, m.err
}
func (m *mockSTT) SupportedFormats() []AudioFormat    { return []AudioFormat{FormatWAV, FormatMP3} }
func (m *mockSTT) SupportedLanguages() []string       { return []string{"zh", "en"} }

// mockTTS 模拟 TTS Provider
type mockTTS struct {
	name   string
	result *SynthesizeResult
	err    error
}

func (m *mockTTS) Name() string { return m.name }
func (m *mockTTS) Synthesize(_ context.Context, _ string, _ SynthesizeOptions) (*SynthesizeResult, error) {
	return m.result, m.err
}
func (m *mockTTS) Voices() []VoiceInfo {
	return []VoiceInfo{{ID: "alloy", Name: "Alloy", Language: "en", Gender: "neutral"}}
}
func (m *mockTTS) SupportedFormats() []AudioFormat { return []AudioFormat{FormatMP3} }

// TestNewService 测试创建服务
func TestNewService(t *testing.T) {
	svc := NewService(nil, nil)
	if svc == nil {
		t.Fatal("服务不应为 nil")
	}
	if svc.HasSTT() {
		t.Error("无 STT 时 HasSTT 应返回 false")
	}
	if svc.HasTTS() {
		t.Error("无 TTS 时 HasTTS 应返回 false")
	}
}

// TestService_Transcribe 测试语音转文本
func TestService_Transcribe(t *testing.T) {
	stt := &mockSTT{
		name: "test-stt",
		result: &TranscribeResult{
			Text:       "你好世界",
			Language:   "zh",
			Duration:   2.5,
			Confidence: 0.95,
		},
	}

	svc := NewService(stt, nil)
	if !svc.HasSTT() {
		t.Fatal("HasSTT 应返回 true")
	}
	if svc.STTName() != "test-stt" {
		t.Errorf("STT 名称不匹配: %q", svc.STTName())
	}

	result, err := svc.Transcribe(context.Background(), []byte("audio-data"), TranscribeOptions{Language: "zh"})
	if err != nil {
		t.Fatalf("转录失败: %v", err)
	}
	if result.Text != "你好世界" {
		t.Errorf("转录文本不匹配: %q", result.Text)
	}
	if result.Language != "zh" {
		t.Errorf("语言不匹配: %q", result.Language)
	}
}

// TestService_Transcribe_NoProvider 测试无 STT Provider 时报错
func TestService_Transcribe_NoProvider(t *testing.T) {
	svc := NewService(nil, nil)
	_, err := svc.Transcribe(context.Background(), []byte("audio"), TranscribeOptions{})
	if err == nil {
		t.Error("无 STT Provider 时应返回错误")
	}
}

// TestService_Transcribe_EmptyAudio 测试空音频数据
func TestService_Transcribe_EmptyAudio(t *testing.T) {
	stt := &mockSTT{name: "test"}
	svc := NewService(stt, nil)
	_, err := svc.Transcribe(context.Background(), nil, TranscribeOptions{})
	if err == nil {
		t.Error("空音频数据应返回错误")
	}
}

// TestService_Synthesize 测试文本转语音
func TestService_Synthesize(t *testing.T) {
	tts := &mockTTS{
		name: "test-tts",
		result: &SynthesizeResult{
			Audio:    []byte("fake-audio-data"),
			Format:   FormatMP3,
			Duration: 1.5,
			Size:     15,
		},
	}

	svc := NewService(nil, tts)
	if !svc.HasTTS() {
		t.Fatal("HasTTS 应返回 true")
	}
	if svc.TTSName() != "test-tts" {
		t.Errorf("TTS 名称不匹配: %q", svc.TTSName())
	}

	result, err := svc.Synthesize(context.Background(), "你好", SynthesizeOptions{Voice: "alloy"})
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	if result.Format != FormatMP3 {
		t.Errorf("格式不匹配: %q", result.Format)
	}
	if len(result.Audio) == 0 {
		t.Error("音频数据不应为空")
	}
}

// TestService_Synthesize_NoProvider 测试无 TTS Provider 时报错
func TestService_Synthesize_NoProvider(t *testing.T) {
	svc := NewService(nil, nil)
	_, err := svc.Synthesize(context.Background(), "hello", SynthesizeOptions{})
	if err == nil {
		t.Error("无 TTS Provider 时应返回错误")
	}
}

// TestService_Synthesize_EmptyText 测试空文本
func TestService_Synthesize_EmptyText(t *testing.T) {
	tts := &mockTTS{name: "test"}
	svc := NewService(nil, tts)
	_, err := svc.Synthesize(context.Background(), "", SynthesizeOptions{})
	if err == nil {
		t.Error("空文本应返回错误")
	}
}

// TestService_TranscribeError 测试 STT Provider 返回错误
func TestService_TranscribeError(t *testing.T) {
	stt := &mockSTT{
		name: "error-stt",
		err:  fmt.Errorf("识别失败"),
	}

	svc := NewService(stt, nil)
	_, err := svc.Transcribe(context.Background(), []byte("audio"), TranscribeOptions{})
	if err == nil {
		t.Error("Provider 错误应传播")
	}
}

// TestService_Names_Nil 测试 nil Provider 时的名称
func TestService_Names_Nil(t *testing.T) {
	svc := NewService(nil, nil)
	if svc.STTName() != "" {
		t.Error("nil STT 时名称应为空")
	}
	if svc.TTSName() != "" {
		t.Error("nil TTS 时名称应为空")
	}
}
