package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/everyday-items/hexclaw/storage"
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
	_, err := s.tx.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, platform, title, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		session.ID, session.UserID, session.Platform, session.Title, session.CreatedAt, session.UpdatedAt,
	)
	return err
}

func (s *txStore) GetSession(ctx context.Context, id string) (*storage.Session, error) {
	row := s.tx.QueryRowContext(ctx,
		`SELECT id, user_id, platform, title, created_at, updated_at FROM sessions WHERE id = ?`, id,
	)
	var sess storage.Session
	if err := row.Scan(&sess.ID, &sess.UserID, &sess.Platform, &sess.Title, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *txStore) ListSessions(ctx context.Context, userID string, limit, offset int) ([]*storage.Session, error) {
	rows, err := s.tx.QueryContext(ctx,
		`SELECT id, user_id, platform, title, created_at, updated_at FROM sessions WHERE user_id = ? ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*storage.Session
	for rows.Next() {
		var sess storage.Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Platform, &sess.Title, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, &sess)
	}
	return sessions, rows.Err()
}

func (s *txStore) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, id); err != nil {
		return err
	}
	_, err := s.tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

func (s *txStore) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	_, err := s.tx.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content, metadata, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.SessionID, msg.Role, msg.Content, msg.Metadata, msg.CreatedAt,
	)
	if err != nil {
		return err
	}
	_, err = s.tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, time.Now(), msg.SessionID,
	)
	return err
}

func (s *txStore) ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]*storage.MessageRecord, error) {
	rows, err := s.tx.QueryContext(ctx,
		`SELECT id, session_id, role, content, metadata, created_at FROM messages WHERE session_id = ? ORDER BY created_at ASC LIMIT ? OFFSET ?`,
		sessionID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []*storage.MessageRecord
	for rows.Next() {
		var msg storage.MessageRecord
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Role, &msg.Content, &msg.Metadata, &msg.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, &msg)
	}
	return messages, rows.Err()
}

func (s *txStore) SaveCost(ctx context.Context, record *storage.CostRecord) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	_, err := s.tx.ExecContext(ctx,
		`INSERT INTO cost_records (id, user_id, provider, model, tokens, cost, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.UserID, record.Provider, record.Model, record.Tokens, record.Cost, record.CreatedAt,
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

func (s *txStore) WithTx(_ context.Context, _ func(storage.Store) error) error {
	return fmt.Errorf("不支持嵌套事务")
}
