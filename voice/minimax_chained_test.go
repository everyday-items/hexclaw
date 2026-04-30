package voice

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func withVoiceFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagVoiceTTSChainV1: on,
	})
	return featureflag.WithContext(ctx, flags)
}

// fakeTTS 实现 TTSProvider 用于 ChainedTTS 测试。
type fakeTTS struct {
	name      string
	audio     []byte
	err       error
	called    int
}

func (f *fakeTTS) Name() string                          { return f.name }
func (f *fakeTTS) Voices() []VoiceInfo                   { return []VoiceInfo{{ID: f.name + "-v1"}} }
func (f *fakeTTS) SupportedFormats() []AudioFormat       { return []AudioFormat{FormatMP3} }
func (f *fakeTTS) Synthesize(_ context.Context, _ string, _ SynthesizeOptions) (*SynthesizeResult, error) {
	f.called++
	if f.err != nil {
		return nil, f.err
	}
	return &SynthesizeResult{Audio: f.audio, Format: FormatMP3, Size: len(f.audio)}, nil
}

func TestMiniMaxTTS_RejectsEmptyConfig(t *testing.T) {
	tts := NewMiniMaxTTS("", "")
	_, err := tts.Synthesize(context.Background(), "hi", SynthesizeOptions{})
	if err == nil {
		t.Error("空 apiKey/groupID 应报错")
	}
}

func TestMiniMaxTTS_RejectsEmptyText(t *testing.T) {
	tts := NewMiniMaxTTS("k", "g")
	if _, err := tts.Synthesize(context.Background(), "", SynthesizeOptions{}); err == nil {
		t.Error("空文本应报错")
	}
	if _, err := tts.Synthesize(context.Background(), "  ", SynthesizeOptions{}); err == nil {
		t.Error("纯空白应报错")
	}
}

func TestMiniMaxTTS_HappyPath(t *testing.T) {
	expectedAudio := []byte{0xff, 0xfb, 0x90, 0x00} // 假装 MP3 header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 校验 Authorization header
		if r.Header.Get("Authorization") != "Bearer key123" {
			http.Error(w, "auth", 401)
			return
		}
		// 校验 GroupId query
		if r.URL.Query().Get("GroupId") != "grp1" {
			http.Error(w, "group", 400)
			return
		}
		body := map[string]any{
			"data":      map[string]string{"audio": hex.EncodeToString(expectedAudio)},
			"base_resp": map[string]any{"status_code": 0, "status_msg": "ok"},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	tts := NewMiniMaxTTS("key123", "grp1", WithMiniMaxBaseURL(srv.URL))
	res, err := tts.Synthesize(context.Background(), "你好", SynthesizeOptions{Voice: "female-shaonv"})
	if err != nil {
		t.Fatalf("应成功；got %v", err)
	}
	if string(res.Audio) != string(expectedAudio) {
		t.Errorf("audio 不匹配；got %x", res.Audio)
	}
	if res.Format != FormatMP3 {
		t.Errorf("format wrong: %s", res.Format)
	}
}

func TestMiniMaxTTS_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"data":      map[string]string{"audio": ""},
			"base_resp": map[string]any{"status_code": 1004, "status_msg": "rate limit"},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	tts := NewMiniMaxTTS("k", "g", WithMiniMaxBaseURL(srv.URL))
	_, err := tts.Synthesize(context.Background(), "hi", SynthesizeOptions{})
	if err == nil {
		t.Error("api error 应被传播")
	}
}

func TestMiniMaxTTS_HTTP4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	tts := NewMiniMaxTTS("k", "g", WithMiniMaxBaseURL(srv.URL))
	if _, err := tts.Synthesize(context.Background(), "hi", SynthesizeOptions{}); err == nil {
		t.Error("HTTP 500 应报错")
	}
}

func TestChainedTTS_FlagOffOnlyTriesFirst(t *testing.T) {
	first := &fakeTTS{name: "a", err: errors.New("a-err")}
	second := &fakeTTS{name: "b", audio: []byte("b-audio")}
	chain := NewChainedTTS(first, second)

	ctx := withVoiceFlag(context.Background(), false)
	_, err := chain.Synthesize(ctx, "hi", SynthesizeOptions{})
	if err == nil {
		t.Error("flag OFF 时第一个失败应直接返回错误")
	}
	if second.called != 0 {
		t.Error("flag OFF 时不应尝试第二个 provider")
	}
}

func TestChainedTTS_FlagOnFallbackToSecond(t *testing.T) {
	first := &fakeTTS{name: "a", err: errors.New("a-err")}
	second := &fakeTTS{name: "b", audio: []byte("b-audio")}
	chain := NewChainedTTS(first, second)

	ctx := withVoiceFlag(context.Background(), true)
	res, err := chain.Synthesize(ctx, "hi", SynthesizeOptions{})
	if err != nil {
		t.Fatalf("应通过 fallback 成功；got %v", err)
	}
	if string(res.Audio) != "b-audio" {
		t.Errorf("应来自第二个 provider；got %s", res.Audio)
	}
	if first.called != 1 || second.called != 1 {
		t.Errorf("first 应被试 1 次，second 应被试 1 次；got %d %d", first.called, second.called)
	}
}

func TestChainedTTS_FlagOnAllFailReportsAll(t *testing.T) {
	first := &fakeTTS{name: "a", err: errors.New("a-err")}
	second := &fakeTTS{name: "b", err: errors.New("b-err")}
	chain := NewChainedTTS(first, second)

	ctx := withVoiceFlag(context.Background(), true)
	_, err := chain.Synthesize(ctx, "hi", SynthesizeOptions{})
	if err == nil {
		t.Fatal("全部失败应报错")
	}
	if !contains(err.Error(), "a-err") || !contains(err.Error(), "b-err") {
		t.Errorf("错误应汇总所有 provider 失败原因；got %v", err)
	}
}

func TestChainedTTS_NoProvidersErrors(t *testing.T) {
	chain := NewChainedTTS()
	if _, err := chain.Synthesize(context.Background(), "hi", SynthesizeOptions{}); err == nil {
		t.Error("空 chain 应报错")
	}
}

func TestChainedTTS_NilProvidersSkipped(t *testing.T) {
	first := &fakeTTS{name: "a", audio: []byte("a-audio")}
	chain := NewChainedTTS(nil, first, nil)

	ctx := withVoiceFlag(context.Background(), true)
	res, err := chain.Synthesize(ctx, "hi", SynthesizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Audio) != "a-audio" {
		t.Errorf("nil 应被跳过；got %s", res.Audio)
	}
}

func TestChainedTTS_NameDescribesChain(t *testing.T) {
	chain := NewChainedTTS(&fakeTTS{name: "a"}, &fakeTTS{name: "b"})
	name := chain.Name()
	if !contains(name, "a") || !contains(name, "b") {
		t.Errorf("Name 应列出所有 provider；got %s", name)
	}
}

func TestChainedTTS_VoicesDeduped(t *testing.T) {
	a := &fakeTTS{name: "x"}
	b := &fakeTTS{name: "x"} // 同名 → 同一 voice ID
	chain := NewChainedTTS(a, b)
	voices := chain.Voices()
	if len(voices) != 1 {
		t.Errorf("同 ID voice 应去重；got %v", voices)
	}
}

func contains(s, sub string) bool { return len(s) > 0 && len(sub) > 0 && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 编译期校验：MiniMaxTTS 实现了 TTSProvider
var _ TTSProvider = (*MiniMaxTTS)(nil)

// 防 unused import 触发的 lint，把 fmt 使用一次
var _ = fmt.Sprintf
