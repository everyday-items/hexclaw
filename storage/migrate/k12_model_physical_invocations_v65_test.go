package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ModelPhysicalInvocationsV65IsRegisteredAndAdditive(t *testing.T) {
	var migration *Migration
	for index := range All {
		if All[index].Version == 65 {
			migration = &All[index]
			break
		}
	}
	if migration == nil {
		t.Fatal("migration v65 is not registered in migrate.All")
	}
	if migration.Func == nil || migration.SQL != "" || migration.AtomicFunc != nil {
		t.Fatalf("migration v65 must be an optional additive Func migration: %+v", migration)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("run full migration chain: %v", err)
	}

	has, err := tableExists(ctx, db, "k12_model_physical_invocations")
	if err != nil || !has {
		t.Fatalf("physical invocation ledger missing: has=%v err=%v", has, err)
	}
	for _, column := range []string{
		"physical_invocation_id",
		"parent_invocation_id",
		"agent_name",
		"job_id",
		"stage",
		"physical_unit",
		"request_digest",
		"route_snapshot_json",
		"request_policy_snapshot_json",
		"status",
		"attempt",
		"result_digest",
		"result_content",
		"external_request_id",
		"failure_kind",
		"created_at",
		"updated_at",
	} {
		hasColumn, columnErr := columnExists(
			ctx,
			db,
			"k12_model_physical_invocations",
			column,
		)
		if columnErr != nil || !hasColumn {
			t.Fatalf("physical invocation column %s: has=%v err=%v", column, hasColumn, columnErr)
		}
	}
	has, err = tableExists(ctx, db, "k12_recognition_fallback_authorizations")
	if err != nil || !has {
		t.Fatalf(
			"recognition fallback authorization ledger missing: has=%v err=%v",
			has,
			err,
		)
	}
	for _, column := range []string{
		"parent_invocation_id",
		"agent_name",
		"job_id",
		"whole_physical_invocation_id",
		"whole_result_digest",
		"whole_result_content",
		"created_at",
	} {
		hasColumn, columnErr := columnExists(
			ctx,
			db,
			"k12_recognition_fallback_authorizations",
			column,
		)
		if columnErr != nil || !hasColumn {
			t.Fatalf(
				"fallback authorization column %s: has=%v err=%v",
				column,
				hasColumn,
				columnErr,
			)
		}
	}
}

func TestK12ModelPhysicalInvocationsV65NoOpsWithoutK12Schema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := K12ModelPhysicalInvocationsV65.Func(ctx, db); err != nil {
		t.Fatalf("optional v65 migration without K12 schema: %v", err)
	}
	has, err := tableExists(ctx, db, "k12_model_physical_invocations")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("v65 must not create a dangling child ledger without the K12 parent schema")
	}

	if _, err := db.ExecContext(ctx, `
CREATE TABLE k12_grading_jobs(record_id TEXT PRIMARY KEY);
`); err != nil {
		t.Fatalf("create selective legacy fixture: %v", err)
	}
	if err := K12ModelPhysicalInvocationsV65.Func(ctx, db); err != nil {
		t.Fatalf("optional v65 migration without parent invocation ledger: %v", err)
	}
	has, err = tableExists(ctx, db, "k12_model_physical_invocations")
	if err != nil || has {
		t.Fatalf("selective legacy fixture created dangling child ledger: has=%v err=%v", has, err)
	}
}

func TestK12ModelPhysicalInvocationsV65NoOpsWithIncompleteLegacyParent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
CREATE TABLE agents(name TEXT PRIMARY KEY);
CREATE TABLE k12_grading_jobs(record_id TEXT PRIMARY KEY);
CREATE TABLE k12_model_invocations(invocation_id TEXT PRIMARY KEY);
`); err != nil {
		t.Fatalf("create incomplete legacy parent fixture: %v", err)
	}

	if err := K12ModelPhysicalInvocationsV65.Func(ctx, db); err != nil {
		t.Fatalf("optional v65 must tolerate an incomplete legacy parent: %v", err)
	}
	has, err := tableExists(ctx, db, "k12_model_physical_invocations")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("v65 created a dangling child ledger against an incomplete parent")
	}
}

func TestK12ModelPhysicalInvocationsV65CascadesPrivateContentAndAuthorizationByOwnerOrJob(
	t *testing.T,
) {
	tests := []struct {
		name      string
		deleteSQL string
		deleteArg string
	}{
		{
			name:      "job",
			deleteSQL: `DELETE FROM k12_grading_jobs WHERE record_id=?`,
			deleteArg: "job-private-cascade",
		},
		{
			name:      "owner",
			deleteSQL: `DELETE FROM agents WHERE name=?`,
			deleteArg: "agent-private-cascade",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "v65-private-cascade.db")
			db, err := sql.Open(
				"sqlite",
				path+"?_pragma=foreign_keys(1)",
			)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			if _, err := db.ExecContext(ctx, `
CREATE TABLE agents(name TEXT PRIMARY KEY);
CREATE TABLE k12_grading_jobs(record_id TEXT PRIMARY KEY);
CREATE TABLE k12_model_invocations(
    invocation_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    stage TEXT NOT NULL
);`); err != nil {
				t.Fatalf("create v65 parent fixture: %v", err)
			}
			if err := K12ModelPhysicalInvocationsV65.Func(ctx, db); err != nil {
				t.Fatalf("apply v65: %v", err)
			}
			if _, err := db.ExecContext(
				ctx,
				`INSERT INTO agents(name) VALUES(?)`,
				"agent-private-cascade",
			); err != nil {
				t.Fatalf("seed v65 owner: %v", err)
			}
			if _, err := db.ExecContext(
				ctx,
				`INSERT INTO k12_grading_jobs(record_id) VALUES(?)`,
				"job-private-cascade",
			); err != nil {
				t.Fatalf("seed v65 job: %v", err)
			}
			if _, err := db.ExecContext(
				ctx,
				`INSERT INTO k12_model_invocations(
				     invocation_id,agent_name,job_id,stage
				 ) VALUES(?,?,?,'recognizing')`,
				"parent-private-cascade",
				"agent-private-cascade",
				"job-private-cascade",
			); err != nil {
				t.Fatalf("seed v65 parent: %v", err)
			}
			const privateContent = `private-provider-result`
			if _, err := db.ExecContext(
				ctx,
				`INSERT INTO k12_model_physical_invocations(
				     physical_invocation_id,parent_invocation_id,agent_name,
				     job_id,stage,physical_unit,request_digest,
				     route_snapshot_json,request_policy_snapshot_json,status,
				     attempt,result_digest,result_content,external_request_id,
				     failure_kind,created_at,updated_at
				 ) VALUES(
				     'whole-private-cascade','parent-private-cascade',
				     'agent-private-cascade','job-private-cascade',
				     'recognizing','whole_page','sha256:request','{}','',
				     'succeeded',1,'sha256:result',?,'upstream','','1','1'
				 )`,
				privateContent,
			); err != nil {
				t.Fatalf("seed v65 private physical row: %v", err)
			}
			if _, err := db.ExecContext(
				ctx,
				`INSERT INTO k12_recognition_fallback_authorizations(
				     parent_invocation_id,agent_name,job_id,
				     whole_physical_invocation_id,whole_result_digest,
				     whole_result_content,created_at
				 ) VALUES(
				     'parent-private-cascade','agent-private-cascade',
				     'job-private-cascade','whole-private-cascade',
				     'sha256:result',?,1
				 )`,
				privateContent,
			); err != nil {
				t.Fatalf("seed v65 private authorization row: %v", err)
			}

			if _, err := db.ExecContext(
				ctx,
				test.deleteSQL,
				test.deleteArg,
			); err != nil {
				t.Fatalf("delete %s: %v", test.name, err)
			}
			for _, table := range []string{
				"k12_model_physical_invocations",
				"k12_recognition_fallback_authorizations",
			} {
				var count int
				if err := db.QueryRowContext(
					ctx,
					`SELECT COUNT(*) FROM `+table,
				).Scan(&count); err != nil {
					t.Fatalf("count %s after %s deletion: %v", table, test.name, err)
				}
				if count != 0 {
					t.Errorf(
						"%s rows after %s deletion=%d want=0",
						table,
						test.name,
						count,
					)
				}
			}
		})
	}
}
