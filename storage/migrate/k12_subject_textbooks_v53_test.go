package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12SubjectTextbooksV53_BackfillsMathAndRepairsDerivedMirror(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "subject-textbooks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := make([]Migration, 0, 52)
	for _, migration := range All {
		if migration.Version <= 52 {
			base = append(base, migration)
		}
	}
	if err := Run(context.Background(), db, base); err != nil {
		t.Fatal(err)
	}

	mustExec(t, db, `INSERT INTO agents(name,metadata) VALUES
        ('legacy_only',?),
        ('canonical_wins',?),
        ('no_math',?)`,
		`{"k12.textbook_edition":"人教版","other":"keep"}`,
		`{"k12.textbook_edition":"旧镜像","k12.textbook_edition.math":"北师大版","k12.textbook_edition.chinese":"统编版"}`,
		`{"k12.textbook_edition.chinese":"统编版"}`)

	runMigration(t, db, 53)
	runMigration(t, db, 53)

	readMeta := func(agent string) map[string]string {
		t.Helper()
		var raw string
		if err := db.QueryRow(`SELECT metadata FROM agents WHERE name=?`, agent).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var meta map[string]string
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatal(err)
		}
		return meta
	}

	legacy := readMeta("legacy_only")
	if legacy["k12.textbook_edition.math"] != "人教版" ||
		legacy["k12.textbook_edition"] != "人教版" ||
		legacy["other"] != "keep" {
		t.Fatalf("legacy migration=%v", legacy)
	}
	canonical := readMeta("canonical_wins")
	if canonical["k12.textbook_edition.math"] != "北师大版" ||
		canonical["k12.textbook_edition"] != "北师大版" ||
		canonical["k12.textbook_edition.chinese"] != "统编版" {
		t.Fatalf("canonical precedence/mirror=%v", canonical)
	}
	noMath := readMeta("no_math")
	if _, exists := noMath["k12.textbook_edition"]; exists {
		t.Fatalf("migration invented math mirror: %v", noMath)
	}
}

func TestBUG20260726034_V53NoOpsWithoutAgents(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "partial-schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := Run(context.Background(), db, []Migration{K12SubjectTextbooksV53}); err != nil {
		t.Fatalf("BUG-20260726-034: partial schema without agents must be a no-op: %v", err)
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=53`).
		Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("BUG-20260726-034: v53 receipt count=%d want 1", applied)
	}
}
