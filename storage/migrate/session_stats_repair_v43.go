package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"unicode/utf8"
)

// SessionStatsRepairV43 repairs historical drift caused by the pre-V43
// DeleteMessage path, which deleted message rows without updating the
// denormalized session list statistics.
var SessionStatsRepairV43 = Migration{
	Version:     43,
	Description: "BUG-20260726 会话消息统计按事实表一次性校准",
	AtomicFunc:  migrateSessionStatsRepairV43,
}

func migrateSessionStatsRepairV43(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启会话统计校准事务: %w", err)
	}
	defer tx.Rollback()

	sessionsExist, err := txTableExists(ctx, tx, "sessions")
	if err != nil {
		return fmt.Errorf("检查 sessions 表: %w", err)
	}
	messagesExist, err := txTableExists(ctx, tx, "messages")
	if err != nil {
		return fmt.Errorf("检查 messages 表: %w", err)
	}
	if !sessionsExist || !messagesExist {
		if err := recordVersion(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}

	rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions`)
	if err != nil {
		return fmt.Errorf("读取待校准会话: %w", err)
	}
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("读取待校准会话 ID: %w", err)
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭待校准会话游标: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历待校准会话: %w", err)
	}

	for _, sessionID := range sessionIDs {
		var count, promptTokens, completionTokens int
		var lastContent string
		if err := tx.QueryRowContext(ctx, `
			SELECT
				COUNT(*),
				COALESCE(SUM(prompt_tokens), 0),
				COALESCE(SUM(completion_tokens), 0),
				COALESCE((
					SELECT content
					FROM messages
					WHERE session_id = ?
					ORDER BY created_at DESC, rowid DESC
					LIMIT 1
				), '')
			FROM messages
			WHERE session_id = ?`,
			sessionID, sessionID,
		).Scan(&count, &promptTokens, &completionTokens, &lastContent); err != nil {
			return fmt.Errorf("汇总会话 %s: %w", sessionID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET
				message_count = ?,
				total_prompt_tokens = ?,
				total_completion_tokens = ?,
				last_message_preview = ?
			WHERE id = ?`,
			count,
			promptTokens,
			completionTokens,
			sessionPreviewV43(lastContent, 200),
			sessionID,
		); err != nil {
			return fmt.Errorf("校准会话 %s: %w", sessionID, err)
		}
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func sessionPreviewV43(value string, byteLimit int) string {
	if len(value) <= byteLimit {
		return value
	}
	cut := byteLimit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
