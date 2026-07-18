package migrate

// V9 类型化存储迁移契约（架构设计-v0.5.0.md §6.9 / ADR-K12-006 / ADR-K12-013）：
// V8 老库（K12 五 collection 数据全在 agent_records.fields_json）升级到 V9 后——
//  1. 五张聚合根专表 + 三张子表 + outbox_events 建成；
//  2. agent_records 中 K12 collection 的每一行逐字段迁入对应专表（含近期新增
//     spot_check_state / canonical_answer / feedback_skill / paper_no 等字段）；
//  3. agent_records 中 K12 collection 行清空（一次切换，不留双轨；表本身保留他用）；
//  4. 迁移幂等可重跑（IF NOT EXISTS + INSERT OR IGNORE；重跑不重复不报错）。
//
// 注：本测试为 RED 先行的迁移一致性契约。夹具 JSON 手写（不 import scenarios/k12，
// 保持平台测试不依赖场景包），键序与领域结构体 json.Marshal 输出一致。

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// openV8 建一个只跑到 V8 的老库，并塞入 K12 五 collection 的代表性数据。
func openV8(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	var v8 []Migration
	for _, m := range All {
		if m.Version <= 8 {
			v8 = append(v8, m)
		}
	}
	if len(v8) < 8 {
		t.Fatalf("期望至少 8 个迁移版本，got %d", len(v8))
	}
	if err := Run(context.Background(), db, v8); err != nil {
		t.Fatalf("migrate to v8: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming'),('lele')`); err != nil {
		t.Fatalf("seed agents: %v", err)
	}
	return db
}

// insertRecord 按 V8 口径塞一行 agent_records。
func insertRecord(t *testing.T, db *sql.DB, id, agent, collection, status, fields, dedupe string, due *int64, version int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO agent_records
        (record_id, agent_name, collection, schema_version, status, fields_json,
         dedupe_key, tags_json, due_at, source_session_id, version, created_at, updated_at)
        VALUES (?, ?, ?, 1, ?, ?, ?, '[]', ?, 'sess-1', ?, 1000, 2000)`,
		id, agent, collection, status, fields, dedupe, due, version)
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func seedK12V8Data(t *testing.T, db *sql.DB) {
	t.Helper()
	due := int64(86400)
	// 错题（含近期新增 canonical_answer / spot_check_state）
	insertRecord(t, db, "m-1", "mingming", "错题本", "new",
		`{"subject":"数学","question":"3.8×3=?","knowledge_point":"小数乘法","error_cause":"计算失误","wrong_process":"误算 10.4","canonical_answer":"11.4","review_stage":2,"last_retried_at":900,"spot_check_state":"scheduled"}`,
		"dk-m1", &due, 3)
	// 错题（老记录：无新增字段，前向兼容）
	insertRecord(t, db, "m-2", "lele", "错题本", "mastered",
		`{"question":"7+8=?","knowledge_point":"进位加法","error_cause":"粗心","wrong_process":"","review_stage":0,"last_retried_at":0}`,
		"dk-m2", nil, 0)
	// 积累本
	insertRecord(t, db, "a-1", "mingming", "积累本", "待复习",
		`{"subject":"语文","entry_type":"默写错","content":"「藤」写错","source":"作业","review_stage":1,"last_retried_at":800}`,
		"dk-a1", &due, 1)
	// 练习集（含 items 子表数据 + paper_no/finalized 等近期字段）
	insertRecord(t, db, "p-1", "mingming", "练习集", "assigned",
		`{"source_kind":"weekly","title":"本周复习卷 · 07/17","paper_no":"P-2629-01","items":[{"item_id":"item-aa","source_problem_id":"sp-1","subject":"数学","added_via":"weekly","question_markdown":"3.8×3=?","expected_answer_markdown":"11.4","verification_status":"verified","verification_evidence":"独立验算","paper_seq":1,"practice_problem_id":"pp-1"},{"item_id":"item-bb","subject":"科学","added_via":"custom","question_markdown":"为什么下雨?","expected_answer_markdown":"水汽凝结","verification_status":"needs_review","blocked_reason":"暂不支持自动验证","returned":true,"result_correct":false}],"question_artifact_id":"art-q1","answer_artifact_id":"art-a1","skipped_blocked_count":1,"finalized_at":1500,"finalized_via":"print","reminder_sent_at":1600,"closed_reason":"","delivery_status":"not_sent"}`,
		"dk-p1", nil, 2)
	// 作品（含 versions 子表 + feedback_skill 近期字段）
	insertRecord(t, db, "w-1", "mingming", "作品", "feedback_ready",
		`{"work_type":"art","title":"我的太空画","task":"画一幅想象画","intent":"想画火箭","versions":[{"version_id":"v1","source_asset_id":"asset://mingming/abc.png","feedback":"构图完整。试试把火箭画大一点。","feedback_source":"ai","feedback_skill":"art-feedback@1.0.0/disk","practice_card_done_at":1700},{"version_id":"v2","content_markdown":"修改稿"}]}`,
		"dk-w1", nil, 1)
	// 批改任务
	insertRecord(t, db, "g-1", "mingming", "批改任务", "completed",
		`{"submission_id":"photo-abc","source_kind":"im_message","idempotency_key":"im|msg-1|v0","confirmed_version":0,"confirmation_state":"confirmed","anchor_state":"located","deadline":0,"model_snapshot":{"provider":"glm","model":"glm-4v"},"stage_checkpoints":[{"stage":"normalizing","artifact_digest":"d1","recorded_at":1100}],"attempt_count":1}`,
		"im|msg-1|v0", nil, 5)
	// 非 K12 collection 的记录不得被动（agent_records 保留他用）
	insertRecord(t, db, "x-1", "mingming", "其他场景记录集", "new", `{"k":"v"}`, "dk-x1", nil, 0)
}

func queryStr(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

func queryInt(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// TestV9_TypedTablesDataMigration V8 老库带各实体数据 → 升级 → 专表数据逐字段一致 + K12 行清空。
func TestV9_TypedTablesDataMigration(t *testing.T) {
	db := openV8(t)
	seedK12V8Data(t, db)
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}

	// —— 错题：逐字段一致（含通用基建列 + 近期新增字段）——
	row := db.QueryRow(`SELECT agent_name, status, subject, question, knowledge_point, error_cause,
        wrong_process, canonical_answer, review_stage, last_retried_at, spot_check_state,
        dedupe_key, due_at, source_session_id, version, created_at, updated_at
        FROM k12_mistakes WHERE record_id='m-1'`)
	var agent, status, subject, question, kp, cause, wrong, canonical, spot, dedupe, sess string
	var reviewStage, lastRetried, version, createdAt, updatedAt int64
	var dueAt *int64
	if err := row.Scan(&agent, &status, &subject, &question, &kp, &cause, &wrong, &canonical,
		&reviewStage, &lastRetried, &spot, &dedupe, &dueAt, &sess, &version, &createdAt, &updatedAt); err != nil {
		t.Fatalf("m-1 未迁入 k12_mistakes: %v", err)
	}
	if agent != "mingming" || status != "new" || subject != "数学" || question != "3.8×3=?" ||
		kp != "小数乘法" || cause != "计算失误" || wrong != "误算 10.4" || canonical != "11.4" ||
		reviewStage != 2 || lastRetried != 900 || spot != "scheduled" ||
		dedupe != "dk-m1" || dueAt == nil || *dueAt != 86400 || sess != "sess-1" ||
		version != 3 || createdAt != 1000 || updatedAt != 2000 {
		t.Fatalf("m-1 字段迁移不一致: agent=%s status=%s subject=%s q=%s kp=%s cause=%s wrong=%s canon=%s stage=%d last=%d spot=%s dk=%s due=%v sess=%s v=%d c=%d u=%d",
			agent, status, subject, question, kp, cause, wrong, canonical, reviewStage, lastRetried, spot, dedupe, dueAt, sess, version, createdAt, updatedAt)
	}
	// 老记录缺新增字段 → 默认值兜底
	if got := queryStr(t, db, `SELECT spot_check_state FROM k12_mistakes WHERE record_id='m-2'`); got != "" {
		t.Fatalf("老记录 spot_check_state 应为空串前向兼容, got %q", got)
	}
	if got := queryStr(t, db, `SELECT canonical_answer FROM k12_mistakes WHERE record_id='m-2'`); got != "" {
		t.Fatalf("老记录 canonical_answer 应为空串, got %q", got)
	}

	// —— 积累本 ——
	if got := queryStr(t, db, `SELECT entry_type FROM k12_accumulations WHERE record_id='a-1'`); got != "默写错" {
		t.Fatalf("a-1 entry_type=%q", got)
	}
	if got := queryInt(t, db, `SELECT review_stage FROM k12_accumulations WHERE record_id='a-1'`); got != 1 {
		t.Fatalf("a-1 review_stage=%d", got)
	}

	// —— 练习集：卷级字段 + items 子表 ——
	if got := queryStr(t, db, `SELECT paper_no FROM k12_practice_sets WHERE record_id='p-1'`); got != "P-2629-01" {
		t.Fatalf("p-1 paper_no=%q", got)
	}
	if got := queryInt(t, db, `SELECT finalized_at FROM k12_practice_sets WHERE record_id='p-1'`); got != 1500 {
		t.Fatalf("p-1 finalized_at=%d", got)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM k12_practice_set_items WHERE set_record_id='p-1'`); got != 2 {
		t.Fatalf("p-1 应有 2 个练习项, got %d", got)
	}
	if got := queryStr(t, db, `SELECT practice_problem_id FROM k12_practice_set_items WHERE set_record_id='p-1' AND item_index=0`); got != "pp-1" {
		t.Fatalf("item0 practice_problem_id=%q", got)
	}
	if got := queryInt(t, db, `SELECT returned FROM k12_practice_set_items WHERE set_record_id='p-1' AND item_index=1`); got != 1 {
		t.Fatalf("item1 returned=%d", got)
	}
	var rc *int64
	if err := db.QueryRow(`SELECT result_correct FROM k12_practice_set_items WHERE set_record_id='p-1' AND item_index=1`).Scan(&rc); err != nil {
		t.Fatalf("item1 result_correct: %v", err)
	}
	if rc == nil || *rc != 0 {
		t.Fatalf("item1 result_correct 应为 0(false), got %v", rc)
	}
	if err := db.QueryRow(`SELECT result_correct FROM k12_practice_set_items WHERE set_record_id='p-1' AND item_index=0`).Scan(&rc); err != nil {
		t.Fatalf("item0 result_correct: %v", err)
	}
	if rc != nil {
		t.Fatalf("item0 result_correct 应为 NULL(无结论), got %v", *rc)
	}

	// —— 作品：versions 子表 + feedback 子表（含 feedback_skill）——
	if got := queryInt(t, db, `SELECT COUNT(*) FROM k12_creative_work_versions WHERE work_record_id='w-1'`); got != 2 {
		t.Fatalf("w-1 应有 2 个版本, got %d", got)
	}
	if got := queryStr(t, db, `SELECT feedback_skill FROM k12_work_feedback WHERE work_record_id='w-1' AND version_index=0`); got != "art-feedback@1.0.0/disk" {
		t.Fatalf("w-1 v1 feedback_skill=%q", got)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM k12_work_feedback WHERE work_record_id='w-1' AND version_index=1`); got != 0 {
		t.Fatalf("无点评版本不应有 feedback 行, got %d", got)
	}
	if got := queryInt(t, db, `SELECT practice_card_done_at FROM k12_creative_work_versions WHERE work_record_id='w-1' AND version_index=0`); got != 1700 {
		t.Fatalf("w-1 v1 practice_card_done_at=%d", got)
	}

	// —— 批改任务 ——
	if got := queryStr(t, db, `SELECT idempotency_key FROM k12_grading_jobs WHERE record_id='g-1'`); got != "im|msg-1|v0" {
		t.Fatalf("g-1 idempotency_key=%q", got)
	}
	if got := queryStr(t, db, `SELECT confirmation_state FROM k12_grading_jobs WHERE record_id='g-1'`); got != "confirmed" {
		t.Fatalf("g-1 confirmation_state=%q", got)
	}
	snap := queryStr(t, db, `SELECT model_snapshot_json FROM k12_grading_jobs WHERE record_id='g-1'`)
	if snap == "" || snap == "{}" {
		t.Fatalf("g-1 model_snapshot_json 不应为空: %q", snap)
	}

	// —— 一次切换：agent_records 中 K12 collection 行清空；非 K12 行保留 ——
	if got := queryInt(t, db, `SELECT COUNT(*) FROM agent_records WHERE collection IN ('错题本','积累本','练习集','作品','批改任务')`); got != 0 {
		t.Fatalf("一次切换后 agent_records 不得残留 K12 行, got %d", got)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM agent_records WHERE record_id='x-1'`); got != 1 {
		t.Fatalf("非 K12 collection 记录应保留（表他用）")
	}

	// —— outbox_events 表就位 ——
	if got := queryInt(t, db, `SELECT COUNT(*) FROM outbox_events`); got != 0 {
		t.Fatalf("新建 outbox_events 应为空, got %d", got)
	}
}

// TestV9_Idempotent 迁移幂等可重跑：V9 的 SQL 重复执行不报错不重复。
func TestV9_Idempotent(t *testing.T) {
	db := openV8(t)
	seedK12V8Data(t, db)
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	before := queryInt(t, db, `SELECT COUNT(*) FROM k12_mistakes`)

	// 找到 V9 定义，二次执行其 SQL/Func（模拟中断后重跑）。
	var v9 *Migration
	for i := range All {
		if All[i].Version == 9 {
			v9 = &All[i]
		}
	}
	if v9 == nil {
		t.Fatal("缺少 V9 迁移定义")
	}
	if v9.Func != nil {
		if err := v9.Func(ctx, db); err != nil {
			t.Fatalf("V9 Func 重跑应幂等: %v", err)
		}
	} else {
		if _, err := db.Exec(v9.SQL); err != nil {
			t.Fatalf("V9 SQL 重跑应幂等: %v", err)
		}
	}
	after := queryInt(t, db, `SELECT COUNT(*) FROM k12_mistakes`)
	if before != after {
		t.Fatalf("重跑后行数变化 %d → %d（应幂等）", before, after)
	}
}
