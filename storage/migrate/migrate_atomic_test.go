package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAtomicMigrationMustCommitItsVersionRecord(t *testing.T) {
	for _, test := range []struct {
		name string
		fn   AtomicMigrationFunc
	}{
		{
			name: "record callback omitted",
			fn: func(context.Context, *sql.DB, func(context.Context, *sql.Tx) error) error {
				return nil
			},
		},
		{
			name: "record callback rolled back",
			fn: func(ctx context.Context, db *sql.DB, record func(context.Context, *sql.Tx) error) error {
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				if err := record(ctx, tx); err != nil {
					return err
				}
				return tx.Rollback()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			migration := Migration{Version: 1, Description: "atomic contract", AtomicFunc: test.fn}
			if err := Run(context.Background(), db, []Migration{migration}); err == nil {
				t.Fatal("AtomicFunc returning nil without a committed version row must fail")
			}
			var rows int
			if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=1`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("uncommitted version rows=%d", rows)
			}
		})
	}
}
