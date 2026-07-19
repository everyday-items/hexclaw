// Package k12cutover provides the executable DD-005/DD-006 release boundary:
// three K12 domain chains switch independently, while every entrypoint inside
// one chain changes in the same SQLite transaction as its migration journal.
package k12cutover

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type Chain string

const (
	ChainGrading        Chain = "grading"
	ChainReview         Chain = "review"
	ChainPracticeReturn Chain = "practice_return"
)

type State string

const (
	StateOld         State = "old"
	StateSwitching   State = "switching"
	StateNew         State = "new"
	StateRollingBack State = "rolling_back"
)

const (
	ImplementationOld = "old"
	ImplementationNew = "new"

	ExternalDelivered    = "delivered"
	CompensationRecorded = "recorded"
)

var ErrBackupRestoreRequired = errors.New("k12 cutover journal incomplete: explicit backup restore required")

type Definition struct {
	Chain       Chain
	DependsOn   []Chain
	Entrypoints []string
}

func DefaultDefinitions() []Definition {
	return []Definition{
		{Chain: ChainGrading, Entrypoints: []string{"desktop", "http", "dingtalk", "webhook"}},
		{Chain: ChainReview, DependsOn: []Chain{ChainGrading}, Entrypoints: []string{"desktop", "http", "dingtalk", "cron", "webhook"}},
		{Chain: ChainPracticeReturn, DependsOn: []Chain{ChainReview}, Entrypoints: []string{"desktop", "http", "dingtalk", "cron", "webhook"}},
	}
}

const K12CutoverDDL = migrate.K12CutoverV16DDL

type Manager struct {
	db          *sql.DB
	definitions map[Chain]Definition
	order       []Chain
}

func NewManager(db *sql.DB, definitions []Definition) (*Manager, error) {
	if db == nil {
		return nil, fmt.Errorf("k12cutover: nil db")
	}
	if len(definitions) != 3 {
		return nil, fmt.Errorf("k12cutover: exact-set requires three domain chains")
	}
	wantOrder := []Chain{ChainGrading, ChainReview, ChainPracticeReturn}
	defs := make(map[Chain]Definition, len(definitions))
	for _, definition := range definitions {
		if _, exists := defs[definition.Chain]; exists {
			return nil, fmt.Errorf("k12cutover: duplicate chain %q", definition.Chain)
		}
		if len(definition.Entrypoints) == 0 {
			return nil, fmt.Errorf("k12cutover: chain %q has no entrypoints", definition.Chain)
		}
		seen := map[string]bool{}
		for _, entrypoint := range definition.Entrypoints {
			entrypoint = strings.TrimSpace(entrypoint)
			if entrypoint == "" || seen[entrypoint] {
				return nil, fmt.Errorf("k12cutover: chain %q invalid entrypoint exact-set", definition.Chain)
			}
			seen[entrypoint] = true
		}
		definition.Entrypoints = append([]string(nil), definition.Entrypoints...)
		definition.DependsOn = append([]Chain(nil), definition.DependsOn...)
		defs[definition.Chain] = definition
	}
	for _, chain := range wantOrder {
		if _, exists := defs[chain]; !exists {
			return nil, fmt.Errorf("k12cutover: missing chain %q", chain)
		}
	}
	return &Manager{db: db, definitions: defs, order: wantOrder}, nil
}

func (m *Manager) Initialize(ctx context.Context) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, chain := range m.order {
		definition := m.definitions[chain]
		if _, err := tx.ExecContext(ctx, `INSERT INTO k12_cutover_chains(chain,state,updated_at)
            VALUES(?, 'old', ?) ON CONFLICT(chain) DO NOTHING`, chain, now); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_cutover_entrypoints WHERE chain=?`, chain).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			for _, entrypoint := range definition.Entrypoints {
				if _, err := tx.ExecContext(ctx, `INSERT INTO k12_cutover_entrypoints
                    (chain,entrypoint,implementation,updated_at) VALUES(?,?,'old',?)`, chain, entrypoint, now); err != nil {
					return err
				}
			}
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT entrypoint FROM k12_cutover_entrypoints WHERE chain=? ORDER BY entrypoint`, chain)
		if err != nil {
			return err
		}
		var stored []string
		for rows.Next() {
			var entrypoint string
			if err := rows.Scan(&entrypoint); err != nil {
				rows.Close()
				return err
			}
			stored = append(stored, entrypoint)
		}
		rows.Close()
		want := append([]string(nil), definition.Entrypoints...)
		sort.Strings(want)
		if strings.Join(stored, "\x00") != strings.Join(want, "\x00") {
			return fmt.Errorf("k12cutover: persisted entrypoint exact-set drift for %s: got=%v want=%v", chain, stored, want)
		}
	}
	return tx.Commit()
}

type SwitchRequest struct {
	RunID        string
	Chain        Chain
	BackupDigest string
}

type Entry struct {
	Ordinal            int
	Chain              Chain
	EntityKind         string
	EntityID           string
	Operation          string
	BeforeJSON         string
	AfterJSON          string
	ExternalEffect     string
	CompensationStatus string
}

type JournalWriter struct {
	ctx   context.Context
	tx    *sql.Tx
	runID string
	chain Chain
	next  int
}

func (w *JournalWriter) Record(entry Entry) error {
	entry.EntityKind = strings.TrimSpace(entry.EntityKind)
	entry.EntityID = strings.TrimSpace(entry.EntityID)
	entry.Operation = strings.TrimSpace(entry.Operation)
	if entry.EntityKind == "" || entry.EntityID == "" || entry.Operation == "" {
		return fmt.Errorf("k12cutover: journal entry missing entity/operation")
	}
	w.next++
	_, err := w.tx.ExecContext(w.ctx, `INSERT INTO k12_migration_journal
        (run_id,ordinal,chain,entity_kind,entity_id,operation,before_json,after_json,external_effect,created_at)
        VALUES(?,?,?,?,?,?,?,?,?,?)`, w.runID, w.next, w.chain, entry.EntityKind, entry.EntityID,
		entry.Operation, entry.BeforeJSON, entry.AfterJSON, entry.ExternalEffect, time.Now().Unix())
	return err
}

func (m *Manager) Switch(ctx context.Context, req SwitchRequest, apply func(*sql.Tx, *JournalWriter) error) error {
	definition, exists := m.definitions[req.Chain]
	if !exists || strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.BackupDigest) == "" || apply == nil {
		return fmt.Errorf("k12cutover: invalid switch request")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM k12_cutover_chains WHERE chain=?`, req.Chain).Scan(&state); err != nil {
		return err
	}
	if state != StateOld {
		return fmt.Errorf("k12cutover: chain %s state %s cannot switch", req.Chain, state)
	}
	for _, dependency := range definition.DependsOn {
		var dependencyState State
		if err := tx.QueryRowContext(ctx, `SELECT state FROM k12_cutover_chains WHERE chain=?`, dependency).Scan(&dependencyState); err != nil {
			return err
		}
		if dependencyState != StateNew {
			return fmt.Errorf("k12cutover: chain %s dependency %s is %s", req.Chain, dependency, dependencyState)
		}
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_cutover_runs
        (run_id,chain,status,backup_digest,started_at) VALUES(?,?,'switching',?,?)`,
		req.RunID, req.Chain, req.BackupDigest, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_cutover_chains SET state='switching',active_run_id=?,updated_at=? WHERE chain=?`, req.RunID, now, req.Chain); err != nil {
		return err
	}
	journal := &JournalWriter{ctx: ctx, tx: tx, runID: req.RunID, chain: req.Chain}
	if err := apply(tx, journal); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE k12_cutover_entrypoints SET implementation='new',updated_at=? WHERE chain=?`, now, req.Chain)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); int(changed) != len(definition.Entrypoints) {
		return fmt.Errorf("k12cutover: chain %s entrypoint exact-set changed=%d want=%d", req.Chain, changed, len(definition.Entrypoints))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_cutover_chains SET state='new',updated_at=? WHERE chain=?`, now, req.Chain); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_cutover_runs SET status='completed',completed_at=? WHERE run_id=?`, now, req.RunID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) State(ctx context.Context, chain Chain) (State, error) {
	var state State
	err := m.db.QueryRowContext(ctx, `SELECT state FROM k12_cutover_chains WHERE chain=?`, chain).Scan(&state)
	return state, err
}

func (m *Manager) Entrypoints(ctx context.Context, chain Chain) (map[string]string, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT entrypoint,implementation FROM k12_cutover_entrypoints WHERE chain=?`, chain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var entrypoint, implementation string
		if err := rows.Scan(&entrypoint, &implementation); err != nil {
			return nil, err
		}
		out[entrypoint] = implementation
	}
	return out, rows.Err()
}

type RollbackHandler interface {
	Reverse(context.Context, *sql.Tx, Entry) error
	RecordCompensation(context.Context, *sql.Tx, Entry) error
}

func (m *Manager) Rollback(ctx context.Context, runID string, journalComplete bool, handler RollbackHandler) error {
	if !journalComplete {
		return ErrBackupRestoreRequired
	}
	if handler == nil {
		return fmt.Errorf("k12cutover: nil rollback handler")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var chain Chain
	var runStatus string
	if err := tx.QueryRowContext(ctx, `SELECT chain,status FROM k12_cutover_runs WHERE run_id=?`, runID).Scan(&chain, &runStatus); err != nil {
		return err
	}
	if runStatus != "completed" {
		return fmt.Errorf("k12cutover: run %s status %s cannot rollback", runID, runStatus)
	}
	for candidate, definition := range m.definitions {
		for _, dependency := range definition.DependsOn {
			if dependency != chain {
				continue
			}
			var dependentState State
			if err := tx.QueryRowContext(ctx, `SELECT state FROM k12_cutover_chains WHERE chain=?`, candidate).Scan(&dependentState); err != nil {
				return err
			}
			if dependentState == StateNew {
				return fmt.Errorf("k12cutover: rollback dependent chain %s first", candidate)
			}
		}
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE k12_cutover_chains SET state='rolling_back',updated_at=? WHERE chain=?`, now, chain); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_cutover_runs SET status='rolling_back' WHERE run_id=?`, runID); err != nil {
		return err
	}
	entries, err := queryJournal(ctx, tx, runID, true)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.ExternalEffect == ExternalDelivered {
			if err := handler.RecordCompensation(ctx, tx, entry); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE k12_migration_journal SET compensation_status=? WHERE run_id=? AND ordinal=?`, CompensationRecorded, runID, entry.Ordinal); err != nil {
				return err
			}
			continue
		}
		if err := handler.Reverse(ctx, tx, entry); err != nil {
			return err
		}
	}
	definition := m.definitions[chain]
	res, err := tx.ExecContext(ctx, `UPDATE k12_cutover_entrypoints SET implementation='old',updated_at=? WHERE chain=?`, now, chain)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); int(changed) != len(definition.Entrypoints) {
		return fmt.Errorf("k12cutover: rollback entrypoint exact-set changed=%d want=%d", changed, len(definition.Entrypoints))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_cutover_chains SET state='old',active_run_id='',updated_at=? WHERE chain=?`, now, chain); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_cutover_runs SET status='rolled_back',completed_at=? WHERE run_id=?`, now, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) Journal(ctx context.Context, runID string) ([]Entry, error) {
	return queryJournal(ctx, m.db, runID, false)
}

type journalQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryJournal(ctx context.Context, q journalQueryer, runID string, reverse bool) ([]Entry, error) {
	direction := "ASC"
	if reverse {
		direction = "DESC"
	}
	rows, err := q.QueryContext(ctx, `SELECT ordinal,chain,entity_kind,entity_id,operation,before_json,
        after_json,external_effect,compensation_status FROM k12_migration_journal
        WHERE run_id=? ORDER BY ordinal `+direction, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var entry Entry
		if err := rows.Scan(&entry.Ordinal, &entry.Chain, &entry.EntityKind, &entry.EntityID,
			&entry.Operation, &entry.BeforeJSON, &entry.AfterJSON, &entry.ExternalEffect,
			&entry.CompensationStatus); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}
