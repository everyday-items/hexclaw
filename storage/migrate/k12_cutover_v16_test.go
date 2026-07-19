package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12CutoverV16InstallsJournalAndAtomicChainTables(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"k12_cutover_chains", "k12_cutover_entrypoints", "k12_cutover_runs", "k12_migration_journal"} {
		has, err := tableExists(context.Background(), db, table)
		if err != nil || !has {
			t.Fatalf("%s missing: has=%v err=%v", table, has, err)
		}
	}
}
