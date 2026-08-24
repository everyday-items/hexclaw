package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12GradingFinalAnnotatedAssetV89PreservesLegacyRowsAndAddsOwnerScopedRelation(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}

	beforeV89 := make([]Migration, 0, len(All))
	for _, migration := range All {
		if migration.Version < 89 {
			beforeV89 = append(beforeV89, migration)
		}
	}
	if err := Run(ctx, db, beforeV89); err != nil {
		t.Fatalf("migrate before V89: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_grading_jobs(
		record_id,agent_name,status,dedupe_key,created_at,updated_at
	) VALUES('job-v89','mingming','projecting','dedupe-v89',100,100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_grading_final_artifacts(
		artifact_id,agent_name,job_id,structure_version,coverage_status,
		total_count,published_count,skipped_count,ordered_current_digests_json,
		canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at,
		finalization_generation
	) VALUES('artifact-v89','mingming','job-v89',1,'complete',1,1,0,
		'["receipt-v89"]','# legacy',?,'summary-v89',100,100,0)`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}

	if err := Run(ctx, db, []Migration{K12GradingFinalAnnotatedAssetV89}); err != nil {
		t.Fatalf("migrate V89: %v", err)
	}
	var legacyRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM k12_grading_final_artifact_assets
		WHERE artifact_id='artifact-v89'`).Scan(&legacyRows); err != nil || legacyRows != 0 {
		t.Fatalf("legacy final artifact relation rows=%d err=%v", legacyRows, err)
	}

	digest := strings.Repeat("b", 64)
	assetID := "asset://mingming/" + digest + ".png"
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_page_assets(
		owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,
		pixel_width,pixel_height,orientation_policy,orientation_policy_version,
		transform_chain_json,storage_state,ready_at,last_error,created_at,updated_at
	) VALUES('guardian-1',?,'mingming',?,'image/png',10,1,1,
		'verified','source-pixel-exif-v1','[]','ready',100,'',100,100)`, assetID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_grading_final_artifact_assets(
		artifact_id,agent_name,annotated_asset_owner_scope,annotated_asset_id,annotated_mime,
		annotated_digest,original_source_digest
	) VALUES('artifact-v89','mingming','guardian-1',?,'image/png',?,?)`,
		assetID, digest, strings.Repeat("c", 64)); err != nil {
		t.Fatalf("insert owner-scoped annotated asset relation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE k12_grading_final_artifact_assets
		SET original_source_digest=? WHERE artifact_id='artifact-v89'`, strings.Repeat("d", 64)); err == nil {
		t.Fatal("annotated asset relation must be immutable")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_grading_final_artifact_assets(
		artifact_id,agent_name,annotated_asset_owner_scope,annotated_asset_id,annotated_mime,
		annotated_digest,original_source_digest
	) VALUES('missing-artifact','mingming','guardian-1',?,'image/png',?,?)`,
		assetID, digest, strings.Repeat("c", 64)); err == nil {
		t.Fatal("annotated asset relation without a final artifact must fail closed")
	}
}
