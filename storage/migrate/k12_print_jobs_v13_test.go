package migrate_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

func TestK12PrintJobsV13SchemaAndPaperNoUniqueness(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All[:13]); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("latest migration=%d err=%v, want 13", version, err)
	}
	for _, table := range []string{"k12_print_jobs", "k12_paper_no_counters"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("missing %s: n=%d err=%v", table, n, err)
		}
	}
	for _, column := range []string{"paper_no", "source_digest", "prepared_fields_json", "native_receipt_id", "attempt_count", "base_set_version"} {
		if !hasColumn(t, db, "k12_print_jobs", column) {
			t.Fatalf("k12_print_jobs missing %s", column)
		}
	}

	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('a'),('b')`); err != nil {
		t.Fatal(err)
	}
	insertSet := `INSERT INTO k12_practice_sets
		(record_id,agent_name,schema_version,status,dedupe_key,tags_json,source_session_id,version,created_at,updated_at,source_kind,title,paper_no,delivery_status)
		VALUES(?,?,1,'assigned',?,'[]','',0,1,1,'weekly','卷',?,'not_sent')`
	if _, err := db.Exec(insertSet, "set-a1", "a", "d-a1", "P-2629-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insertSet, "set-a2", "a", "d-a2", "P-2629-01"); err == nil {
		t.Fatal("same Tutor duplicate paper_no must violate V13 unique index")
	}
	if _, err := db.Exec(insertSet, "set-b1", "b", "d-b1", "P-2629-01"); err != nil {
		t.Fatalf("cross Tutor same paper_no is allowed: %v", err)
	}
}

func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}
