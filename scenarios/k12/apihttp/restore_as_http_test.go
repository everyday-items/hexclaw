package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type restoreAsHTTPMigrator struct {
	restoreCalls  int
	rollbackCalls int
	plan          usecase.RestoreAsPlan
	rollback      usecase.RestoreAsRollbackRequest
}

func (m *restoreAsHTTPMigrator) RestoreArchiveAs(_ context.Context, plan usecase.RestoreAsPlan) (usecase.RestoreAsResult, error) {
	m.restoreCalls++
	m.plan = plan
	return usecase.RestoreAsResult{
		MigrationID: "migration-http", SourceAgent: plan.SourceAgent, TargetAgent: plan.TargetAgent,
		Status: usecase.RestoreMigrationCompleted, Restored: len(plan.MigratedArchive.Records),
		OriginalArchiveDigest: plan.OriginalArchiveDigest, MigratedChecksum: plan.MigratedArchive.Checksum,
		SnapshotDigest: "snapshot-http", JournalEntries: 3, OriginalArchivePreserved: true,
	}, nil
}

func (m *restoreAsHTTPMigrator) RollbackRestoreAs(_ context.Context, req usecase.RestoreAsRollbackRequest) (usecase.RestoreAsResult, error) {
	m.rollbackCalls++
	m.rollback = req
	return usecase.RestoreAsResult{
		MigrationID: req.MigrationID, TargetAgent: req.TargetAgent,
		Status: usecase.RestoreMigrationRolledBack, Idempotent: false,
	}, nil
}

func TestRestoreAsHTTPRequiresExplicitEnvelopeAndExposesMigrationEvidenceAndRollback(t *testing.T) {
	h, migrator := newRestoreAsHTTPServer(t)
	bak := &usecase.Hexbak{
		Version: 3, ArchiveID: "archive-http", AgentName: "source-child", ExportedAt: 100,
		Records: []*records.AgentRecord{{
			RecordID: "record-http", AgentName: "source-child", Collection: k12.CollectionMistakes,
			SchemaVersion: 1, Status: k12.StatusNew, Fields: `{"question":"q"}`,
			DedupeKey: "record-http", Tags: `[]`, Version: 1, CreatedAt: 1, UpdatedAt: 1,
		}},
		Profile: &k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上"},
	}
	if err := usecase.SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"archive": bak, "source_agent": "source-child", "target_agent": "target-child",
		"guardian_confirmed": true, "idempotency_key": "restore-http-1",
	})
	rec, out := do(t, h, http.MethodPost, "/restore-as", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore-as status=%d body=%s", rec.Code, rec.Body.String())
	}
	if migrator.restoreCalls != 1 || migrator.plan.TargetAgent != "target-child" {
		t.Fatalf("migrator calls=%d plan=%+v", migrator.restoreCalls, migrator.plan)
	}
	if out["migration_id"] != "migration-http" || out["status"] != usecase.RestoreMigrationCompleted {
		t.Fatalf("response=%+v", out)
	}
	if out["original_archive_preserved"] != true || out["snapshot_digest"] != "snapshot-http" {
		t.Fatalf("evidence omitted: %+v", out)
	}

	rec, out = do(t, h, http.MethodPost, "/restore-as/migration-http/rollback",
		`{"target_agent":"target-child","guardian_confirmed":true}`)
	if rec.Code != http.StatusOK || out["status"] != usecase.RestoreMigrationRolledBack {
		t.Fatalf("rollback status=%d out=%+v", rec.Code, out)
	}
	if migrator.rollbackCalls != 1 || migrator.rollback.MigrationID != "migration-http" {
		t.Fatalf("rollback calls=%d req=%+v", migrator.rollbackCalls, migrator.rollback)
	}
}

func TestRestoreAsHTTPCancelledConfirmationMakesZeroPersistenceCalls(t *testing.T) {
	h, migrator := newRestoreAsHTTPServer(t)
	rec, _ := do(t, h, http.MethodPost, "/restore-as",
		`{"archive":{"version":3,"archive_id":"a","agent_name":"source","exported_at":1,"records":[],"checksum":"x"},"source_agent":"source","target_agent":"target","guardian_confirmed":false,"idempotency_key":"k"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	if migrator.restoreCalls != 0 {
		t.Fatalf("cancel reached persistence: %d", migrator.restoreCalls)
	}
}

func TestRestoreAsHTTPInvalidAssetManifestIsBadRequestBeforePersistence(t *testing.T) {
	h, migrator := newRestoreAsHTTPServer(t)
	bak := &usecase.Hexbak{
		Version: 3, AgentName: "source-child", ExportedAt: 100,
		Records: []*records.AgentRecord{{
			RecordID: "record-with-missing-asset", AgentName: "source-child",
			Collection: k12.CollectionCreativeWork, SchemaVersion: 1, Status: k12.WorkStatusDraft,
			Fields:    `{"source_asset_id":"asset://source-child/` + strings.Repeat("0", 64) + `.png"}`,
			DedupeKey: "missing-asset", Tags: `[]`, Version: 1, CreatedAt: 1, UpdatedAt: 1,
		}},
	}
	if err := usecase.SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"archive": bak, "source_agent": "source-child", "target_agent": "target-child",
		"guardian_confirmed": true, "idempotency_key": "restore-http-invalid-asset",
	})
	rec, _ := do(t, h, http.MethodPost, "/restore-as", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid asset manifest status=%d body=%s", rec.Code, rec.Body.String())
	}
	if migrator.restoreCalls != 0 {
		t.Fatalf("invalid asset manifest reached persistence: %d", migrator.restoreCalls)
	}
}

func newRestoreAsHTTPServer(t *testing.T) (http.Handler, *restoreAsHTTPMigrator) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('target-child')`); err != nil {
		t.Fatal(err)
	}
	k, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	migrator := &restoreAsHTTPMigrator{}
	k.Deps.ArchiveMigrator = migrator
	return apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps}), migrator
}
