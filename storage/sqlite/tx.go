package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

// txStore 事务内的存储包装
//
// 将所有操作路由到事务对象 (sql.Tx)，而非直接操作数据库。
// 由 Store.WithTx() 创建，不应直接使用。
type txStore struct {
	tx *sql.Tx
}

func (s *txStore) Init(_ context.Context) error {
	return fmt.Errorf("不能在事务中调用 Init")
}

func (s *txStore) Close() error {
	return fmt.Errorf("不能在事务中调用 Close")
}

func (s *txStore) CreateSession(ctx context.Context, session *storage.Session) error {
	now := time.Now()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = now
	}
	if session.Status == 0 {
		session.Status = 1
	}
	if session.Meta == "" {
		session.Meta = "{}"
	}
	_, err := s.tx.ExecContext(ctx,
		`INSERT INTO sessions (`+sessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionInsertArgs(session)...,
	)
	return err
}

func (s *txStore) GetSession(ctx context.Context, id string) (*storage.Session, error) {
	row := s.tx.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id,
	)
	sess, err := scanSession(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	return sess, nil
}

func (s *txStore) FindSessionByScope(ctx context.Context, userID, platform, instanceID, chatID string) (*storage.Session, error) {
	row := s.tx.QueryRowContext(ctx,
		`SELECT `+sessionCols+`
		 FROM sessions
		 WHERE user_id = ? AND platform = ? AND instance_id = ? AND chat_id = ? AND status = 1
		 ORDER BY updated_at DESC, created_at DESC
		 LIMIT 1`,
		userID, platform, instanceID, chatID,
	)
	sess, err := scanSession(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	return sess, nil
}

func (s *txStore) ListSessions(ctx context.Context, userID string, limit, offset int) ([]*storage.Session, error) {
	rows, err := s.tx.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE user_id = ? AND status >= 0 ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*storage.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

func (s *txStore) DeleteSession(ctx context.Context, id string) error {
	_, err := s.tx.ExecContext(ctx,
		`UPDATE sessions SET status = -1, updated_at = ? WHERE id = ?`,
		time.Now(), id,
	)
	return err
}

func (s *txStore) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	if msg.ContentType == "" {
		msg.ContentType = "text"
	}
	if msg.Meta == "" {
		msg.Meta = "{}"
	}
	_, err := s.tx.ExecContext(ctx,
		`INSERT INTO messages (`+messageCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageInsertArgs(msg)...,
	)
	if err != nil {
		return err
	}

	preview := msg.Content
	if len(preview) > 200 {
		preview = preview[:200]
	}
	_, err = s.tx.ExecContext(ctx,
		`UPDATE sessions SET
			updated_at = ?,
			message_count = message_count + 1,
			total_prompt_tokens = total_prompt_tokens + ?,
			total_completion_tokens = total_completion_tokens + ?,
			last_message_preview = ?
		 WHERE id = ?`,
		time.Now(), msg.PromptTokens, msg.CompletionTokens, preview, msg.SessionID,
	)
	return err
}

func (s *txStore) DeleteMessage(ctx context.Context, id string) error {
	_, err := s.tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id)
	return err
}

func (s *txStore) ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]*storage.MessageRecord, error) {
	rows, err := s.tx.QueryContext(ctx,
		`SELECT `+messageCols+` FROM messages WHERE session_id = ? ORDER BY created_at ASC LIMIT ? OFFSET ?`,
		sessionID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []*storage.MessageRecord
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func (s *txStore) UpdateMessageFeedback(ctx context.Context, id, feedback string) error {
	switch feedback {
	case "", "like", "dislike":
	default:
		return fmt.Errorf("无效反馈值: %s", feedback)
	}

	result, err := s.tx.ExecContext(ctx, `UPDATE messages SET feedback = ? WHERE id = ?`, feedback, id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func (s *txStore) SaveCost(ctx context.Context, record *storage.CostRecord) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	if record.Meta == "" {
		record.Meta = "{}"
	}
	_, err := s.tx.ExecContext(ctx,
		`INSERT INTO cost_records (id, user_id, session_id, message_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost, meta, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.UserID, record.SessionID, record.MessageID, record.Provider, record.Model,
		record.PromptTokens, record.CompletionTokens, record.TotalTokens, record.Cost, record.Meta, record.CreatedAt,
	)
	return err
}

func (s *txStore) GetUserCost(ctx context.Context, userID string, since time.Time) (float64, error) {
	var total sql.NullFloat64
	err := s.tx.QueryRowContext(ctx,
		`SELECT SUM(cost) FROM cost_records WHERE user_id = ? AND created_at >= ?`,
		userID, since,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Float64, nil
}

func (s *txStore) GetGlobalCost(ctx context.Context, since time.Time) (float64, error) {
	var total sql.NullFloat64
	err := s.tx.QueryRowContext(ctx,
		`SELECT SUM(cost) FROM cost_records WHERE created_at >= ?`, since,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Float64, nil
}

func (s *txStore) UpdateSession(ctx context.Context, session *storage.Session) error {
	session.UpdatedAt = time.Now()
	_, err := s.tx.ExecContext(ctx,
		`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		session.Title, session.UpdatedAt, session.ID,
	)
	return err
}

func (s *txStore) SearchMessages(_ context.Context, _, _ string, _, _ int) ([]*storage.SearchResult, int, error) {
	return nil, 0, fmt.Errorf("不支持在事务中搜索消息")
}

func (s *txStore) ForkSession(_ context.Context, _, _, _ string) (*storage.Session, error) {
	return nil, fmt.Errorf("不支持在事务中创建分支")
}

func (s *txStore) ListSessionBranches(_ context.Context, _ string) ([]*storage.Session, error) {
	return nil, fmt.Errorf("不支持在事务中列出分支")
}

func (s *txStore) WithTx(_ context.Context, _ func(storage.Store) error) error {
	return fmt.Errorf("不支持嵌套事务")
}

func (s *txStore) CleanupOldSessions(_ context.Context, _ int) (int64, error) {
	return 0, fmt.Errorf("不能在事务中调用 CleanupOldSessions")
}
