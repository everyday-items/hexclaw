package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openBUG20260725Latest(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:bug-20260725-learning-profile?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatalf("migrate latest: %v", err)
	}
	return db
}

func requireBUG20260725Table(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	ok, err := tableExists(context.Background(), db, table)
	if err != nil {
		t.Fatalf("inspect %s: %v", table, err)
	}
	if !ok {
		t.Fatalf("missing approved durable table %s", table)
	}
}

func requireBUG20260725Columns(t *testing.T, db *sql.DB, table string, columns ...string) {
	t.Helper()
	requireBUG20260725Table(t, db, table)
	for _, column := range columns {
		ok, err := columnExists(context.Background(), db, table, column)
		if err != nil {
			t.Fatalf("inspect %s.%s: %v", table, column, err)
		}
		if !ok {
			t.Fatalf("missing approved column %s.%s", table, column)
		}
	}
}

func TestBUG20260725011CandidateHashAndAtomicCommitAreDurable(t *testing.T) {
	db := openBUG20260725Latest(t)

	t.Run("candidate selection aggregate", func(t *testing.T) {
		requireBUG20260725Columns(t, db, "k12_practice_candidate_selections",
			"selection_id", "source_mistake_id", "target_set_record_id",
			"state", "next_batch_ordinal", "revision")
		requireBUG20260725Columns(t, db, "k12_practice_candidates",
			"candidate_id", "selection_id", "candidate_kind", "batch_ordinal",
			"candidate_ordinal", "normalized_content_hash", "state",
			"problem_json", "failure_message")
	})

	t.Run("target practice set hash uniqueness", func(t *testing.T) {
		requireBUG20260725Columns(t, db, "k12_practice_set_items",
			"set_record_id", "normalized_content_hash")

		rows, err := db.Query("PRAGMA index_list('k12_practice_set_items')")
		if err != nil {
			t.Fatal(err)
		}
		var uniqueIndexes []string
		for rows.Next() {
			var seq, unique, partial int
			var name, origin string
			if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			if unique == 1 {
				uniqueIndexes = append(uniqueIndexes, name)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}

		found := false
		for _, index := range uniqueIndexes {
			info, err := db.Query("PRAGMA index_info('" + strings.ReplaceAll(index, "'", "''") + "')")
			if err != nil {
				t.Fatal(err)
			}
			var columns []string
			for info.Next() {
				var seqno, cid int
				var name string
				if err := info.Scan(&seqno, &cid, &name); err != nil {
					_ = info.Close()
					t.Fatal(err)
				}
				columns = append(columns, name)
			}
			if err := info.Close(); err != nil {
				t.Fatal(err)
			}
			if len(columns) == 2 && columns[0] == "set_record_id" &&
				columns[1] == "normalized_content_hash" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("missing unique (set_record_id, normalized_content_hash) dedupe boundary")
		}
	})

	t.Run("atomic idempotent commit ledger", func(t *testing.T) {
		requireBUG20260725Columns(t, db, "k12_practice_candidate_commits",
			"commit_id", "selection_id", "target_set_record_id",
			"selected_hashes_digest", "added_count", "result_json",
			"idempotency_key", "created_at")
	})
}

func TestBUG20260725013DeferThisWeekIsNotMasteryOrSuppression(t *testing.T) {
	db := openBUG20260725Latest(t)
	requireBUG20260725Columns(t, db, "k12_mistake_review_states",
		"agent_name", "mistake_record_id", "state", "deferred_iso_year",
		"deferred_iso_week", "prior_schedule_json", "revision", "updated_at")
	requireBUG20260725Columns(t, db, "k12_mistake_review_commands",
		"idempotency_key", "command_type", "from_state", "to_state",
		"prior_schedule_json", "created_at")

	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type='table' AND name='k12_mistake_review_states'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{
		"scheduled", "deferred_this_week", "suppressed", "mastered",
	} {
		if !strings.Contains(ddl, state) {
			t.Fatalf("review state exact-set missing %q in DDL", state)
		}
	}
}

func TestBUG20260725017SuppressionHasOneRestorablePriorSchedule(t *testing.T) {
	db := openBUG20260725Latest(t)
	requireBUG20260725Columns(t, db, "k12_mistake_review_states",
		"state", "prior_schedule_json", "revision")
	requireBUG20260725Columns(t, db, "k12_mistake_review_commands",
		"idempotency_key", "command_type", "from_state", "to_state",
		"prior_schedule_json", "created_at")

	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type='table' AND name='k12_mistake_review_commands'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"defer_this_week", "suppress", "restore"} {
		if !strings.Contains(ddl, command) {
			t.Fatalf("review command exact-set missing %q in DDL", command)
		}
	}
}
