package records

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
)

var registerDedupeFaultDriver sync.Once

type dedupeFaultDriver struct{}
type dedupeFaultConn struct{}
type dedupeFaultResult struct{}

func (dedupeFaultDriver) Open(string) (driver.Conn, error)  { return dedupeFaultConn{}, nil }
func (dedupeFaultConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (dedupeFaultConn) Close() error                        { return nil }
func (dedupeFaultConn) Begin() (driver.Tx, error)           { return nil, errors.New("unsupported") }
func (dedupeFaultConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return dedupeFaultResult{}, nil
}
func (dedupeFaultConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, errors.New("dedupe lookup failed")
}
func (dedupeFaultResult) LastInsertId() (int64, error) { return 0, nil }
func (dedupeFaultResult) RowsAffected() (int64, error) { return 0, nil }

func TestPut_DedupeLookupErrorIsReturned(t *testing.T) {
	registerDedupeFaultDriver.Do(func() { sql.Register("records_dedupe_fault", dedupeFaultDriver{}) })
	db, err := sql.Open("records_dedupe_fault", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	reg := NewRecordSchemaRegistry()
	if err := reg.Register(mockSchema()); err != nil {
		t.Fatal(err)
	}
	s := NewStore(db, reg)
	created, err := s.Put(context.Background(), &AgentRecord{AgentName: "mingming", Collection: "notes", SourceSession: "same"})
	if created || err == nil {
		t.Fatalf("dedupe lookup failure must be returned, created=%v err=%v", created, err)
	}
}
