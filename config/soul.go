package config

import (
	"os"
	"path/filepath"
	"strings"
)

// 默认助理（小蟹）人设(SOUL)的文件化存储。
//
// 对齐 OpenClaw / Hermes 的 `SOUL.md` 约定（行业隐共识）：人设 = 一份完整的
// Markdown 系统提示词文档，存为 `~/.hexclaw/SOUL.md`，而非配置文件里的字段。
// 文件即单一真源——引擎每轮对话读取、API 端点读写，无需重启、无共享状态竞态。

// SoulFilePath 返回默认助理人设文件路径：<home>/.hexclaw/SOUL.md。
func SoulFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hexclaw", "SOUL.md"), nil
}

// ReadSoul 读取自定义人设(SOUL)内容（trim 后）。文件不存在或读取失败时返回空串，
// 调用方据此回退到引擎内置默认人设。
func ReadSoul() string {
	path, err := SoulFilePath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// WriteSoul 写入自定义人设(SOUL)。soul 去空白后为空时删除 SOUL.md（恢复内置默认）。
func WriteSoul(soul string) error {
	path, err := SoulFilePath()
	if err != nil {
		return err
	}
	soul = strings.TrimSpace(soul)
	if soul == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 原子写（临时文件 + rename）：避免聊天热路径 ReadSoul 读到半截内容（消除 TOCTOU）。
	return ReconcileCommittedWrite(atomicWriteFile(path, []byte(soul+"\n"), 0o644))
}
