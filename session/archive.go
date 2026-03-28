package session

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

// Archiver 会话转录归档器
//
// 在 Compactor 压缩前，将待删除的旧消息归档到 Markdown 文件。
// 归档路径: ~/.hexclaw/transcripts/{sessionID}_{timestamp}.md
// 对标 OpenClaw Session Transcripts。
type Archiver struct {
	dir string // 归档目录
}

// NewArchiver 创建归档器
func NewArchiver(dir string) *Archiver {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".hexclaw", "transcripts")
	}
	return &Archiver{dir: dir}
}

// Archive 将消息列表归档为 Markdown 文件
//
// 返回归档文件路径。如果消息为空则不归档。
func (a *Archiver) Archive(sessionID string, messages []*storage.MessageRecord) (string, error) {
	if len(messages) == 0 {
		return "", nil
	}

	if err := os.MkdirAll(a.dir, 0755); err != nil {
		return "", fmt.Errorf("create archive dir: %w", err)
	}

	ts := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("%s_%s.md", sessionID, ts)
	path := filepath.Join(a.dir, filename)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Session Transcript: %s\n\n", sessionID))
	sb.WriteString(fmt.Sprintf("> Archived at: %s\n", time.Now().Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("> Messages: %d\n\n", len(messages)))
	sb.WriteString("---\n\n")

	for _, msg := range messages {
		role := msg.Role
		ts := msg.CreatedAt.Format("15:04:05")
		sb.WriteString(fmt.Sprintf("### [%s] %s\n\n", ts, role))
		sb.WriteString(msg.Content)
		sb.WriteString("\n\n")
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		return "", fmt.Errorf("write archive: %w", err)
	}

	log.Printf("会话归档完成: session=%s file=%s messages=%d", sessionID, filename, len(messages))
	return path, nil
}
