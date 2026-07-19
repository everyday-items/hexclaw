package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12StructuredFeedbackV14IsPresentAndReentrant(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}
	has, err := columnExists(context.Background(), db, "k12_work_feedback", "structured_feedback_json")
	if err != nil || !has {
		t.Fatalf("structured_feedback_json missing: has=%v err=%v", has, err)
	}
	if err := migrateK12StructuredFeedbackV14(context.Background(), db); err != nil {
		t.Fatalf("V14 must be safely reentrant: %v", err)
	}
}
