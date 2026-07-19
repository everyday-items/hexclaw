package migrate_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

func TestV12_PracticeReturnAssetsAndGenerationJobsUpgrade(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := migrate.Run(ctx, db, migrate.All[:11]); err != nil {
		t.Fatalf("迁到 V11: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('xiaoming')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_practice_sets
        (record_id,agent_name,status,source_kind,title,delivery_status,dedupe_key,tags_json,source_session_id,created_at,updated_at)
        VALUES('legacy-set','xiaoming','draft','custom','旧卷','not_sent','legacy-key','[]','s',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_practice_set_items
        (set_record_id,item_index,item_id,question_markdown,verification_status)
        VALUES('legacy-set',0,'q1','1+1=?','pending')`); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Run(ctx, db, migrate.All[:12]); err != nil {
		t.Fatalf("升级 V12: %v", err)
	}
	for _, tc := range []struct{ table, col string }{
		{"k12_practice_set_items", "generation_job_id"},
		{"k12_practice_set_items", "actual_difficulty"},
		{"k12_practice_return_assets", "return_id"},
		{"k12_practice_generation_jobs", "request_digest"},
		{"k12_practice_generation_jobs", "deduplicated_count"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, tc.table, tc.col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("V12 缺 schema %s.%s", tc.table, tc.col)
		}
	}
	var generationID, difficulty string
	if err := db.QueryRow(`SELECT generation_job_id, actual_difficulty FROM k12_practice_set_items
        WHERE set_record_id='legacy-set' AND item_index=0`).Scan(&generationID, &difficulty); err != nil {
		t.Fatalf("旧练习项丢失: %v", err)
	}
	if generationID != "" || difficulty != "" {
		t.Fatalf("旧数据新增列应为空默认值: %q/%q", generationID, difficulty)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 12 {
		t.Fatalf("schema_migrations 应到 V12: version=%d err=%v", version, err)
	}
	var v12 migrate.Migration
	for _, m := range migrate.All {
		if m.Version == 12 {
			v12 = m
			break
		}
	}
	if v12.Func == nil {
		t.Fatal("V12 应使用列探测 Func，支持半迁移库安全重入")
	}
	if err := v12.Func(ctx, db); err != nil {
		t.Fatalf("V12 schema 重入必须幂等: %v", err)
	}
}
