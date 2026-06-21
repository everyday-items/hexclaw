package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSoulReadWriteRoundtrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows

	// 初始无 SOUL.md → 空（调用方回退内置默认）
	if got := ReadSoul(); got != "" {
		t.Fatalf("初始应为空，得到 %q", got)
	}

	// 写入（含首尾空白，应被 trim）
	if err := WriteSoul("  你是定制小蟹。\n规则：简洁。  "); err != nil {
		t.Fatalf("WriteSoul: %v", err)
	}
	if got := ReadSoul(); got != "你是定制小蟹。\n规则：简洁。" {
		t.Fatalf("ReadSoul 不符: %q", got)
	}

	// 落盘位置 = <home>/.hexclaw/SOUL.md
	path, err := SoulFilePath()
	if err != nil {
		t.Fatalf("SoulFilePath: %v", err)
	}
	if path != filepath.Join(home, ".hexclaw", "SOUL.md") {
		t.Fatalf("路径不符: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("SOUL.md 未落盘: %v", err)
	}

	// 空内容 → 删除文件（恢复内置默认）
	if err := WriteSoul("   "); err != nil {
		t.Fatalf("WriteSoul empty: %v", err)
	}
	if got := ReadSoul(); got != "" {
		t.Fatalf("清空后应为空，得到 %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("清空后 SOUL.md 应被删除")
	}

	// 删除不存在文件应幂等不报错
	if err := WriteSoul(""); err != nil {
		t.Fatalf("重复清空应幂等: %v", err)
	}
}
