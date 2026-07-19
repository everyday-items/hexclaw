package k12cutover

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

func cutoverDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(K12CutoverDDL + `; CREATE TABLE domain_values (chain TEXT PRIMARY KEY, value TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	for _, chain := range []Chain{ChainGrading, ChainReview, ChainPracticeReturn} {
		if _, err := db.Exec(`INSERT INTO domain_values(chain,value) VALUES(?, 'old')`, chain); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestChainCutoverIsAtomicOrderedAndHasNoPerEntryFallback(t *testing.T) {
	ctx := context.Background()
	db := cutoverDB(t)
	m, err := NewManager(db, DefaultDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected halfway")
	err = m.Switch(ctx, SwitchRequest{RunID: "run-grading-fail", Chain: ChainGrading, BackupDigest: "sha256:before"},
		func(tx *sql.Tx, journal *JournalWriter) error {
			if _, err := tx.Exec(`UPDATE domain_values SET value='new' WHERE chain=?`, ChainGrading); err != nil {
				return err
			}
			if err := journal.Record(Entry{EntityKind: "domain_values", EntityID: string(ChainGrading), Operation: "update", BeforeJSON: `{"value":"old"}`, AfterJSON: `{"value":"new"}`}); err != nil {
				return err
			}
			return injected
		})
	if !errors.Is(err, injected) {
		t.Fatalf("want injected failure, got %v", err)
	}
	assertChain(t, m, ChainGrading, StateOld, "old")
	assertChain(t, m, ChainReview, StateOld, "old")
	assertChain(t, m, ChainPracticeReturn, StateOld, "old")

	if err := switchValue(ctx, m, "run-practice-too-early", ChainPracticeReturn); err == nil {
		t.Fatal("practice-return must not switch before grading/review")
	}
	for i, chain := range []Chain{ChainGrading, ChainReview, ChainPracticeReturn} {
		if err := switchValue(ctx, m, fmt.Sprintf("run-%d", i), chain); err != nil {
			t.Fatalf("switch %s: %v", chain, err)
		}
		assertChain(t, m, chain, StateNew, "new")
		entries, err := m.Entrypoints(ctx, chain)
		if err != nil {
			t.Fatal(err)
		}
		for name, implementation := range entries {
			if implementation != ImplementationNew {
				t.Fatalf("chain %s entry %s retained fallback %s", chain, name, implementation)
			}
		}
	}
}

// REG-DD-005: every one of the three independently switched domain chains is
// exercised at a real mid-transaction fault point. The failed chain must leave
// no run/journal residue and, critically, must not change either of the other
// chains. Downstream prerequisites are switched before the injected run so the
// test reaches the mutation point instead of merely testing dependency denial.
func TestEveryDomainChainRollsBackAtomicallyAtInjectedMidSwitchFault(t *testing.T) {
	for _, failedChain := range []Chain{ChainGrading, ChainReview, ChainPracticeReturn} {
		t.Run(string(failedChain), func(t *testing.T) {
			ctx := context.Background()
			db := cutoverDB(t)
			m, err := NewManager(db, DefaultDefinitions())
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Initialize(ctx); err != nil {
				t.Fatal(err)
			}

			// Satisfy only this chain's declared dependencies. These completed
			// chains are the control group and must remain new after the fault.
			for index, chain := range []Chain{ChainGrading, ChainReview} {
				if chain == failedChain {
					break
				}
				if err := switchValue(ctx, m, fmt.Sprintf("prerequisite-%d", index), chain); err != nil {
					t.Fatalf("switch prerequisite %s: %v", chain, err)
				}
			}

			runID := "fault-" + string(failedChain)
			injected := errors.New("injected after domain write and journal append")
			err = m.Switch(ctx, SwitchRequest{RunID: runID, Chain: failedChain, BackupDigest: "sha256:before"},
				func(tx *sql.Tx, journal *JournalWriter) error {
					if _, err := tx.Exec(`UPDATE domain_values SET value='new' WHERE chain=?`, failedChain); err != nil {
						return err
					}
					if err := journal.Record(Entry{
						EntityKind: "domain_values", EntityID: string(failedChain), Operation: "update",
						BeforeJSON: `{"value":"old"}`, AfterJSON: `{"value":"new"}`,
					}); err != nil {
						return err
					}
					return injected
				})
			if !errors.Is(err, injected) {
				t.Fatalf("Switch error=%v, want injected fault", err)
			}

			for _, chain := range []Chain{ChainGrading, ChainReview, ChainPracticeReturn} {
				wantState, wantValue := StateOld, "old"
				if chain != failedChain {
					for _, dependency := range m.definitions[failedChain].DependsOn {
						if chain == dependency || (failedChain == ChainPracticeReturn && chain == ChainGrading) {
							wantState, wantValue = StateNew, "new"
						}
					}
				}
				assertChain(t, m, chain, wantState, wantValue)
			}

			var runs, journalRows int
			if err := db.QueryRow(`SELECT COUNT(*) FROM k12_cutover_runs WHERE run_id=?`, runID).Scan(&runs); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM k12_migration_journal WHERE run_id=?`, runID).Scan(&journalRows); err != nil {
				t.Fatal(err)
			}
			if runs != 0 || journalRows != 0 {
				t.Fatalf("failed switch leaked run/journal rows: runs=%d journal=%d", runs, journalRows)
			}
			entries, err := m.Entrypoints(ctx, failedChain)
			if err != nil {
				t.Fatal(err)
			}
			for name, implementation := range entries {
				if implementation != ImplementationOld {
					t.Fatalf("failed chain %s entrypoint %s leaked implementation=%s", failedChain, name, implementation)
				}
			}

			// A rolled-back transaction releases both the chain and run ID for a
			// deterministic retry; no backup restore is needed because the journal
			// was transactionally complete and never committed.
			if err := switchValue(ctx, m, runID, failedChain); err != nil {
				t.Fatalf("safe retry after atomic rollback: %v", err)
			}
			assertChain(t, m, failedChain, StateNew, "new")
		})
	}
}

func TestIncrementalRollbackReplaysJournalAndRecordsExternalCompensation(t *testing.T) {
	ctx := context.Background()
	db := cutoverDB(t)
	m, _ := NewManager(db, DefaultDefinitions())
	if err := m.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Switch(ctx, SwitchRequest{RunID: "run-grading", Chain: ChainGrading, BackupDigest: "sha256:before"},
		func(tx *sql.Tx, journal *JournalWriter) error {
			if _, err := tx.Exec(`UPDATE domain_values SET value='new' WHERE chain=?`, ChainGrading); err != nil {
				return err
			}
			if err := journal.Record(Entry{EntityKind: "domain_values", EntityID: string(ChainGrading), Operation: "update", BeforeJSON: `{"value":"old"}`, AfterJSON: `{"value":"new"}`}); err != nil {
				return err
			}
			return journal.Record(Entry{EntityKind: "channel_message", EntityID: "external-1", Operation: "send", ExternalEffect: ExternalDelivered})
		}); err != nil {
		t.Fatal(err)
	}
	handler := &rollbackRecorder{}
	if err := m.Rollback(ctx, "run-grading", true, handler); err != nil {
		t.Fatal(err)
	}
	assertChain(t, m, ChainGrading, StateOld, "old")
	if handler.reversed != 1 || handler.compensated != 1 {
		t.Fatalf("rollback actions reversed=%d compensated=%d", handler.reversed, handler.compensated)
	}
	entries, err := m.Journal(ctx, "run-grading")
	if err != nil {
		t.Fatal(err)
	}
	if entries[1].CompensationStatus != CompensationRecorded {
		t.Fatalf("delivered external effect must stay visible as compensated, got %+v", entries[1])
	}
}

func TestRollbackRequiresBackupOnlyWhenJournalIsDeclaredIncomplete(t *testing.T) {
	db := cutoverDB(t)
	m, _ := NewManager(db, DefaultDefinitions())
	if err := m.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := switchValue(context.Background(), m, "run", ChainGrading); err != nil {
		t.Fatal(err)
	}
	err := m.Rollback(context.Background(), "run", false, &rollbackRecorder{})
	if !errors.Is(err, ErrBackupRestoreRequired) {
		t.Fatalf("incomplete journal must require explicit backup path, got %v", err)
	}
	assertChain(t, m, ChainGrading, StateNew, "new")
}

func switchValue(ctx context.Context, m *Manager, runID string, chain Chain) error {
	return m.Switch(ctx, SwitchRequest{RunID: runID, Chain: chain, BackupDigest: "sha256:before"}, func(tx *sql.Tx, journal *JournalWriter) error {
		var before string
		if err := tx.QueryRow(`SELECT value FROM domain_values WHERE chain=?`, chain).Scan(&before); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE domain_values SET value='new' WHERE chain=?`, chain); err != nil {
			return err
		}
		return journal.Record(Entry{EntityKind: "domain_values", EntityID: string(chain), Operation: "update", BeforeJSON: fmt.Sprintf(`{"value":%q}`, before), AfterJSON: `{"value":"new"}`})
	})
}

func assertChain(t *testing.T, m *Manager, chain Chain, state State, value string) {
	t.Helper()
	got, err := m.State(context.Background(), chain)
	if err != nil || got != state {
		t.Fatalf("chain %s state=%s err=%v want=%s", chain, got, err, state)
	}
	var gotValue string
	if err := m.db.QueryRow(`SELECT value FROM domain_values WHERE chain=?`, chain).Scan(&gotValue); err != nil || gotValue != value {
		t.Fatalf("chain %s value=%s err=%v want=%s", chain, gotValue, err, value)
	}
}

type rollbackRecorder struct{ reversed, compensated int }

func (r *rollbackRecorder) Reverse(_ context.Context, tx *sql.Tx, entry Entry) error {
	r.reversed++
	if entry.EntityKind == "domain_values" {
		_, err := tx.Exec(`UPDATE domain_values SET value='old' WHERE chain=?`, entry.EntityID)
		return err
	}
	return nil
}

func (r *rollbackRecorder) RecordCompensation(_ context.Context, _ *sql.Tx, _ Entry) error {
	r.compensated++
	return nil
}
