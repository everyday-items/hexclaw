package voice

import (
	"testing"
)

func TestAzureSTT_Name(t *testing.T) {
	stt := NewAzureSTT("test-key", "eastasia")
	if stt.Name() != "azure-stt" {
		t.Errorf("期望 azure-stt, 得到 %s", stt.Name())
	}
}

func TestAzureSTT_SupportedFormats(t *testing.T) {
	stt := NewAzureSTT("test-key", "eastasia")
	formats := stt.SupportedFormats()
	if len(formats) == 0 {
		t.Error("应返回至少一种支持的格式")
	}
}

func TestAzureSTT_SupportedLanguages(t *testing.T) {
	stt := NewAzureSTT("test-key", "eastasia")
	langs := stt.SupportedLanguages()
	found := false
	for _, l := range langs {
		if l == "zh-CN" {
			found = true
			break
		}
	}
	if !found {
		t.Error("应支持 zh-CN")
	}
}

func TestAzureSTT_TranscribeEmptyAudio(t *testing.T) {
	stt := NewAzureSTT("test-key", "eastasia")
	_, err := stt.Transcribe(t.Context(), nil, TranscribeOptions{})
	if err == nil {
		t.Error("空音频应返回错误")
	}
}

func TestAzureSTT_WithLanguage(t *testing.T) {
	stt := NewAzureSTT("test-key", "eastasia", AzureSTTWithLanguage("en-US"))
	if stt.language != "en-US" {
		t.Errorf("期望 en-US, 得到 %s", stt.language)
	}
}

func TestAzureTTS_Name(t *testing.T) {
	ttsProvider := NewAzureTTS("test-key", "eastasia")
	if ttsProvider.Name() != "azure-tts" {
		t.Errorf("期望 azure-tts, 得到 %s", ttsProvider.Name())
	}
}

func TestAzureTTS_Voices(t *testing.T) {
	ttsProvider := NewAzureTTS("test-key", "eastasia")
	voices := ttsProvider.Voices()
	if len(voices) == 0 {
		t.Error("应返回至少一种音色")
	}
	// 检查中文音色
	foundCN := false
	for _, v := range voices {
		if v.Language == "zh-CN" {
			foundCN = true
			break
		}
	}
	if !foundCN {
		t.Error("应包含中文音色")
	}
}

func TestAzureTTS_SynthesizeEmpty(t *testing.T) {
	ttsProvider := NewAzureTTS("test-key", "eastasia")
	_, err := ttsProvider.Synthesize(t.Context(), "", SynthesizeOptions{})
	if err == nil {
		t.Error("空文本应返回错误")
	}
}

func TestAzureTTS_WithVoice(t *testing.T) {
	ttsProvider := NewAzureTTS("test-key", "eastasia", AzureTTSWithVoice("zh-CN-YunxiNeural"))
	if ttsProvider.defaultVoice != "zh-CN-YunxiNeural" {
		t.Errorf("期望 zh-CN-YunxiNeural, 得到 %s", ttsProvider.defaultVoice)
	}
}
