package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ParentConfirmationV38AddsIndependentConfirmationFact(t *testing.T) {
	db, err := sql.Open("sqlite", "file:k12-parent-confirmation-v38?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE k12_mistakes (
		record_id TEXT PRIMARY KEY
	)`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12ParentConfirmationV38}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`PRAGMA table_info(k12_mistakes)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		found = found || name == "parent_confirmed_at"
	}
	if !found {
		t.Fatal("k12_mistakes.parent_confirmed_at missing")
	}
}

func TestK12ParentConfirmationV38IsRegisteredAtItsVersion(t *testing.T) {
	found := false
	for _, migration := range All {
		found = found || migration.Version == K12ParentConfirmationV38.Version
	}
	if !found {
		t.Fatalf("migration v%d is not registered", K12ParentConfirmationV38.Version)
	}
}
