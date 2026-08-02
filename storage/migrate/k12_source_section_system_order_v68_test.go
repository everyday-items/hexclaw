package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12SourceSectionSystemOrderV68AddsOnlyExplicitDualFactColumns(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	beforeV68 := make([]Migration, 0, len(All))
	for _, migration := range All {
		if migration.Version < K12SourceSectionSystemOrderV68.Version {
			beforeV68 = append(beforeV68, migration)
		}
	}
	if err := Run(ctx, db, beforeV68); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES ('legacy-owner')`); err != nil {
		t.Fatalf("insert legacy agent: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_problems (
		problem_id,agent_name,submission_id,page_asset_id,ordinal,problem_kind,
		parent_problem_id,subproblem_no,source_number_path_json,display_label,
		stem_raw,stem_markdown,canonical_version,created_at,updated_at
	) VALUES ('legacy-problem','legacy-owner','legacy-submission','legacy-page',0,'standalone',
		NULL,'','[]','','legacy raw','legacy markdown',1,1,1)`); err != nil {
		t.Fatalf("insert legacy problem: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_problem_structure_snapshots (
		agent_name,submission_id,structure_version,structure_digest,mapping_state,
		current_disposition,created_at,updated_at
	) VALUES ('legacy-owner','legacy-submission',1,'legacy-structure','resolved','current',1,1)`); err != nil {
		t.Fatalf("insert legacy structure snapshot: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_problem_structure_members (
		agent_name,submission_id,structure_version,problem_id,ordinal,problem_kind,
		parent_problem_id,subproblem_no,source_number_path_json,display_label,
		dependency_group_id,input_revision
	) VALUES ('legacy-owner','legacy-submission',1,'legacy-problem',0,'standalone',
		'','','[]','','problem:legacy-problem',1)`); err != nil {
		t.Fatalf("insert legacy structure member: %v", err)
	}
	if err := Run(ctx, db, []Migration{K12SourceSectionSystemOrderV68}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"k12_problems", "k12_problem_structure_members"} {
		for _, column := range []string{
			"source_section_path_json", "source_section_label",
			"system_section_ordinal", "system_display_label",
		} {
			has, err := columnExists(ctx, db, table, column)
			if err != nil || !has {
				t.Fatalf("%s.%s missing: has=%v err=%v", table, column, has, err)
			}
		}
	}
	for _, table := range []string{"k12_problems", "k12_problem_structure_members"} {
		var legacyPath, legacyLabel, systemLabel string
		var systemOrdinal int
		if err := db.QueryRowContext(ctx, `SELECT source_section_path_json,source_section_label,
			system_section_ordinal,system_display_label FROM `+table+` LIMIT 1`).Scan(
			&legacyPath, &legacyLabel, &systemOrdinal, &systemLabel,
		); err != nil {
			t.Fatalf("read migrated %s legacy defaults: %v", table, err)
		}
		if legacyPath != "[]" || legacyLabel != "" || systemOrdinal != 0 || systemLabel != "" {
			t.Fatalf("%s must preserve legacy facts as explicit blanks, got path=%q label=%q ordinal=%d system=%q", table, legacyPath, legacyLabel, systemOrdinal, systemLabel)
		}
	}
	if err := Run(ctx, db, []Migration{K12SourceSectionSystemOrderV68}); err != nil {
		t.Fatalf("v68 rerun must be idempotent: %v", err)
	}
}
