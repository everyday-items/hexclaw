package migrate

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12CurriculumProgressRevisionV78RegistrationAndContract(t *testing.T) {
	migration := findV78Migration(t)
	if migration.AtomicFunc == nil || migration.Func != nil || migration.SQL != "" {
		t.Fatalf("migration v78 must be one additive AtomicFunc: %+v", migration)
	}

	t.Run("fresh migration chain has no synthetic lifecycle head", func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		defer db.Close()
		if _, err := db.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`); err != nil {
			t.Fatal(err)
		}
		if err := Run(context.Background(), db, All); err != nil {
			t.Fatalf("run fresh migration chain through V78: %v", err)
		}

		var heads, latest int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM k12_curriculum_progress_revisions`).Scan(&heads); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&latest); err != nil {
			t.Fatal(err)
		}
		if heads != 0 || latest != 78 {
			t.Fatalf("fresh V78 chain heads/latest=%d/%d, want 0/78", heads, latest)
		}
	})

	t.Run("empty current projection keeps lifecycle head absent", func(t *testing.T) {
		db := openV78LegacyProgressDB(t)
		if err := applyMigration(context.Background(), db, migration); err != nil {
			t.Fatalf("apply V78 to empty progress projection: %v", err)
		}

		var heads int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM k12_curriculum_progress_revisions`).Scan(&heads); err != nil {
			t.Fatal(err)
		}
		if heads != 0 {
			t.Fatalf("empty progress fabricated %d lifecycle heads, want 0", heads)
		}
		assertV78VersionRecorded(t, db)
	})

	t.Run("legacy current progress backfills exact head", func(t *testing.T) {
		db := openV78LegacyProgressDB(t)
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO agents(name) VALUES('mingming'),('xiaohong');
			INSERT INTO k12_curriculum_progress(agent_name,subject,revision,updated_at)
			VALUES('mingming','math',7,1700000007),
			      ('xiaohong','math',2,1700000002)`); err != nil {
			t.Fatalf("seed V78 legacy progress: %v", err)
		}

		if err := applyMigration(context.Background(), db, migration); err != nil {
			t.Fatalf("apply V78 to legacy progress: %v", err)
		}

		rows, err := db.QueryContext(t.Context(), `
			SELECT agent_name,subject,revision,updated_at
			FROM k12_curriculum_progress_revisions
			ORDER BY agent_name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		want := []struct {
			agent    string
			subject  string
			revision int
			updated  int64
		}{
			{agent: "mingming", subject: "math", revision: 7, updated: 1700000007},
			{agent: "xiaohong", subject: "math", revision: 2, updated: 1700000002},
		}
		for index := range want {
			if !rows.Next() {
				t.Fatalf("V78 backfill rows ended at %d, want %d", index, len(want))
			}
			var got struct {
				agent    string
				subject  string
				revision int
				updated  int64
			}
			if err := rows.Scan(&got.agent, &got.subject, &got.revision, &got.updated); err != nil {
				t.Fatal(err)
			}
			if got != want[index] {
				t.Fatalf("V78 backfill row %d=%+v, want %+v", index, got, want[index])
			}
		}
		if rows.Next() {
			t.Fatal("V78 backfill produced unexpected extra lifecycle head")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		assertV78VersionRecorded(t, db)
	})

	t.Run("legacy non-math projection does not block math head backfill", func(t *testing.T) {
		db := openV78LegacyProgressDB(t)
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO agents(name) VALUES('mingming');
			INSERT INTO k12_curriculum_progress(agent_name,subject,revision,updated_at)
			VALUES('mingming','math',5,1700000005),
			      ('mingming','science',4,1700000004)`); err != nil {
			t.Fatalf("seed V78 mixed-subject legacy progress: %v", err)
		}

		if err := applyMigration(context.Background(), db, migration); err != nil {
			t.Fatalf("apply V78 to mixed-subject legacy progress: %v", err)
		}

		var headSubject string
		var headRevision int
		if err := db.QueryRowContext(t.Context(), `
			SELECT subject,revision FROM k12_curriculum_progress_revisions
			WHERE agent_name='mingming'`).Scan(&headSubject, &headRevision); err != nil {
			t.Fatal(err)
		}
		if headSubject != "math" || headRevision != 5 {
			t.Fatalf("V78 mixed-subject head=%s/%d, want math/5", headSubject, headRevision)
		}
		var legacyScienceRevision int
		if err := db.QueryRowContext(t.Context(), `
			SELECT revision FROM k12_curriculum_progress
			WHERE agent_name='mingming' AND subject='science'`).Scan(&legacyScienceRevision); err != nil {
			t.Fatal(err)
		}
		if legacyScienceRevision != 4 {
			t.Fatalf("V78 changed legacy science projection revision=%d, want 4", legacyScienceRevision)
		}
		assertV78ForeignKeysClean(t, db)
		assertV78VersionRecorded(t, db)
	})

	t.Run("head constraints and agent cascade", func(t *testing.T) {
		db := openV78LegacyProgressDB(t)
		if _, err := db.ExecContext(t.Context(), `INSERT INTO agents(name) VALUES('mingming'),('zero-revision')`); err != nil {
			t.Fatal(err)
		}
		if err := applyMigration(context.Background(), db, migration); err != nil {
			t.Fatalf("apply V78 for constraint checks: %v", err)
		}

		if _, err := db.ExecContext(t.Context(), `INSERT INTO k12_curriculum_progress_revisions
			(agent_name,subject,revision,updated_at)
			VALUES('mingming','math',1,1700000001)`); err != nil {
			t.Fatalf("insert canonical V78 head: %v", err)
		}
		for _, test := range []struct {
			name string
			sql  string
		}{
			{
				name: "one head per agent subject",
				sql: `INSERT INTO k12_curriculum_progress_revisions
					(agent_name,subject,revision,updated_at)
					VALUES('mingming','math',2,1700000002)`,
			},
			{
				name: "subject is math",
				sql: `INSERT INTO k12_curriculum_progress_revisions
					(agent_name,subject,revision,updated_at)
					VALUES('mingming','science',1,1700000001)`,
			},
			{
				name: "revision starts at one",
				sql: `INSERT INTO k12_curriculum_progress_revisions
					(agent_name,subject,revision,updated_at)
					VALUES('zero-revision','math',0,1700000001)`,
			},
			{
				name: "agent foreign key",
				sql: `INSERT INTO k12_curriculum_progress_revisions
					(agent_name,subject,revision,updated_at)
					VALUES('unknown','math',1,1700000001)`,
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				if _, err := db.ExecContext(t.Context(), test.sql); err == nil {
					t.Fatalf("V78 accepted invalid lifecycle head: %s", test.name)
				}
			})
		}

		if _, err := db.ExecContext(t.Context(), `DELETE FROM agents WHERE name='mingming'`); err != nil {
			t.Fatalf("delete V78 parent agent: %v", err)
		}
		var remaining int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM k12_curriculum_progress_revisions`).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatalf("V78 agent cascade left %d lifecycle heads", remaining)
		}
		assertV78ForeignKeysClean(t, db)
	})

	t.Run("version failure rolls back table and backfill", func(t *testing.T) {
		db := openV78LegacyProgressDB(t)
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO agents(name) VALUES('mingming');
			INSERT INTO k12_curriculum_progress(agent_name,subject,revision,updated_at)
			VALUES('mingming','math',3,1700000003)`); err != nil {
			t.Fatal(err)
		}
		forced := errors.New("forced version failure")
		err := migration.AtomicFunc(context.Background(), db, func(context.Context, *sql.Tx) error {
			return forced
		})
		if !errors.Is(err, forced) {
			t.Fatalf("V78 did not return record-version failure: %v", err)
		}
		var tableCount int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master
			WHERE type='table' AND name='k12_curriculum_progress_revisions'`).Scan(&tableCount); err != nil {
			t.Fatal(err)
		}
		if tableCount != 0 {
			t.Fatalf("V78 record-version failure left lifecycle table: count=%d", tableCount)
		}
		var versionCount int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations WHERE version=78`).Scan(&versionCount); err != nil {
			t.Fatal(err)
		}
		if versionCount != 0 {
			t.Fatalf("V78 record-version failure left version rows: count=%d", versionCount)
		}
	})
}

func findV78Migration(t *testing.T) Migration {
	t.Helper()
	for _, migration := range All {
		if migration.Version == 78 {
			return migration
		}
	}
	t.Fatal("migration v78 is not registered in migrate.All")
	return Migration{}
}

func openV78LegacyProgressDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `
		PRAGMA foreign_keys=ON;
		CREATE TABLE schema_migrations(
			version INTEGER PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			applied_at INTEGER NOT NULL
		);
		CREATE TABLE agents(name TEXT PRIMARY KEY);
		CREATE TABLE k12_curriculum_progress(
			agent_name TEXT NOT NULL,
			subject TEXT NOT NULL,
			revision INTEGER NOT NULL CHECK(revision >= 1),
			updated_at INTEGER NOT NULL,
			PRIMARY KEY(agent_name,subject),
			FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
		)`); err != nil {
		t.Fatalf("create V78 legacy schema: %v", err)
	}
	return db
}

func assertV78VersionRecorded(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations WHERE version=78`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("V78 migration ledger count=%d, want 1", count)
	}
}

func assertV78ForeignKeysClean(t *testing.T, db *sql.DB) {
	t.Helper()
	var violations int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("V78 foreign-key check found %d conflicts", violations)
	}
}
