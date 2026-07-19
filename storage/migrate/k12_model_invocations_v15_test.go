package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ModelInvocationsV15IsInstalledByNumberedMigration(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}
	has, err := tableExists(context.Background(), db, "k12_model_invocations")
	if err != nil || !has {
		t.Fatalf("k12_model_invocations missing: has=%v err=%v", has, err)
	}
}
