package engineadapter

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// ArchiveRestoreAdapter is the production durability boundary for K12 restore.
// k12_* typed tables and agents.metadata are written through the same SQLite tx;
// Dispatcher keeps profile readers behind its write lock until that tx commits
// and the committed AgentConfig has been published in memory.
type ArchiveRestoreAdapter struct {
	db         *sql.DB
	records    *k12storage.Store
	dispatcher *router.Dispatcher
	agents     *router.SQLiteStore
}

func NewArchiveRestoreAdapter(
	db *sql.DB,
	recordStore *k12storage.Store,
	dispatcher *router.Dispatcher,
	agentStore *router.SQLiteStore,
) *ArchiveRestoreAdapter {
	return &ArchiveRestoreAdapter{db: db, records: recordStore, dispatcher: dispatcher, agents: agentStore}
}

var _ usecase.ArchiveRestorer = (*ArchiveRestoreAdapter)(nil)

func (a *ArchiveRestoreAdapter) RestoreArchive(
	ctx context.Context,
	agentName string,
	recs []*records.AgentRecord,
	profile *k12.ChildProfile,
) error {
	if a == nil || a.db == nil || a.records == nil || a.dispatcher == nil || a.agents == nil {
		return fmt.Errorf("k12 archive restore: atomic adapter is not fully configured")
	}
	if err := a.records.ValidateAgentRecords(agentName, recs); err != nil {
		return err
	}

	return a.dispatcher.UpdateAgentPersisted(agentName,
		func(current router.AgentConfig) (router.AgentConfig, error) {
			current.Metadata = k12.ReplaceProfileInMeta(current.Metadata, profile)
			return current, nil
		},
		func(updated *router.AgentConfig) error {
			tx, err := a.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin shared restore transaction: %w", err)
			}
			defer tx.Rollback()
			if err := a.records.ImportAgentRecordsTx(ctx, tx, agentName, recs); err != nil {
				return fmt.Errorf("merge records: %w", err)
			}
			if err := a.agents.SaveAgentTx(ctx, tx, updated); err != nil {
				return fmt.Errorf("replace profile metadata: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit shared restore transaction: %w", err)
			}
			return nil
		},
	)
}
