package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 缺失可选 voice 段时，桌面端依赖的免费 edge-tts 默认必须只在内存生效。
// 该回归先钉住启动不应通过 Desktop 侧读改写配置来补默认值。
func TestDesktopDefaultsKeepVoiceInMemoryWhenConfigOmitsVoice(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "hexclaw.yaml")
	before := []byte("server:\n  host: 127.0.0.1\n")
	if err := os.WriteFile(configPath, before, 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("load config fixture: %v", err)
	}
	if !cfg.Voice.Enabled {
		t.Fatal("voice default must be enabled in memory")
	}
	if cfg.Voice.TTS.Provider != "edge-tts" {
		t.Fatalf("voice default provider = %q, want edge-tts", cfg.Voice.TTS.Provider)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("loading defaults must not rewrite the owner config")
	}
}
