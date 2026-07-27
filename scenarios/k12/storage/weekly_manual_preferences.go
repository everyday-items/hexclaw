package k12storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
)

func (s *Store) GetWeeklyManualPracticePreference(
	ctx context.Context,
	agentName, planSection string,
) (int, error) {
	var itemCount int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT item_count FROM k12_weekly_manual_practice_preferences
         WHERE agent_name=? AND plan_section=?`,
		strings.TrimSpace(agentName),
		strings.TrimSpace(planSection),
	).Scan(&itemCount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, records.ErrNotFound
	}
	return itemCount, err
}

func upsertWeeklyManualPracticePreferenceTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName, planSection string,
	itemCount int,
	at int64,
) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO k12_weekly_manual_practice_preferences
    (agent_name,plan_section,item_count,updated_at)
VALUES(?,?,?,?)
ON CONFLICT(agent_name,plan_section) DO UPDATE SET
    item_count=excluded.item_count,
    updated_at=excluded.updated_at`,
		agentName, planSection, itemCount, at)
	return err
}
