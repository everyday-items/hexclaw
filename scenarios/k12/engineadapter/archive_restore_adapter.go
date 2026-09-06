package engineadapter

import (
	"context"
	"database/sql"
	"errors"
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
var _ usecase.HexbakArchiveRestorer = (*ArchiveRestoreAdapter)(nil)

// RestoreHexbak restores current and historical records/profile, confirmed OCR,
// V19 Problem/Attempt and packed content files. SQLite writes commit together;
// files installed before commit are tracked and removed on every error.
func (a *ArchiveRestoreAdapter) RestoreHexbak(ctx context.Context, bak *usecase.Hexbak) error {
	if a == nil || a.db == nil || a.records == nil || a.dispatcher == nil || a.agents == nil {
		return fmt.Errorf("k12 archive restore: atomic adapter is not fully configured")
	}
	if err := usecase.VerifyHexbak(bak); err != nil {
		return err
	}
	if err := a.records.ValidateAgentRecords(bak.AgentName, bak.Records); err != nil {
		return err
	}
	return a.dispatcher.UpdateAgentPersisted(bak.AgentName,
		func(current router.AgentConfig) (router.AgentConfig, error) {
			current.Metadata = k12.ReplaceProfileInMeta(current.Metadata, bak.Profile)
			return current, nil
		},
		func(updated *router.AgentConfig) error {
			tx, err := a.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin shared hexbak restore transaction: %w", err)
			}
			defer tx.Rollback()
			if err := a.records.ImportAgentRecordsTx(ctx, tx, bak.AgentName, bak.Records); err != nil {
				return fmt.Errorf("merge hexbak records: %w", err)
			}
			if err := a.records.ImportCreativeWorksArchiveV7Tx(ctx, tx, bak.AgentName, bak.CurrentCreativeWorks); err != nil {
				return fmt.Errorf("restore current creative works: %w", err)
			}
			if err := a.records.ImportProblemAttemptSnapshotsTx(
				ctx, tx, bak.AgentName, bak.ProblemAttempts,
			); err != nil {
				return fmt.Errorf("merge Problem/Attempt ledger: %w", err)
			}
			if bak.ProblemSource != nil {
				if err := a.records.ImportProblemSourceArchiveV6Tx(
					ctx, tx, bak.AgentName, *bak.ProblemSource,
				); err != nil {
					return fmt.Errorf("merge problem-source durability closure: %w", err)
				}
			}
			if err := a.agents.SaveAgentTx(ctx, tx, updated); err != nil {
				return fmt.Errorf("replace hexbak profile metadata: %w", err)
			}
			if err := a.records.ImportCreativeWorkOCREvidenceTx(
				ctx, tx, bak.AgentName, bak.CreativeWorkOCR,
			); err != nil {
				return fmt.Errorf("merge confirmed creative-work OCR evidence: %w", err)
			}
			installed, err := ensureArchiveAssets(bak)
			if err != nil {
				return fmt.Errorf("install hexbak archive assets: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return errors.Join(
					fmt.Errorf("commit shared hexbak restore transaction: %w", err),
					removeCreatedRestoreAssets(installed),
				)
			}
			return nil
		},
	)
}

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
