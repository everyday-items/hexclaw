package usecase

import (
	"database/sql"
	"path/filepath"
	"testing"

	internaltestdb "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase/internal/testdb"
)

func openMigratedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := internaltestdb.Open(filepath.Join(t.TempDir(), "k12.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
