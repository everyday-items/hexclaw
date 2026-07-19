package migrate_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// TestV11_K12DedupeReleaseBackfill BUG-20260718 存量回填：V10 库中已退出活跃空间但
// 没有墓碑键的 K12 行（作品 archived / 练习集 assigned 等），V11 按运行态同款规则
// `dedupe_key || '#released#' || record_id` 回填——否则升级后存量已固化卷仍会永久截胡
// 相同首题组合的新篮（运行态修复只覆盖迁移后的状态变更）。
// 幂等可重跑，且必须容忍已手工 SQL 修过的行（本机 data.db 作品行）——绝不二次叠加。
func TestV11_K12DedupeReleaseBackfill(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()

	// 1) 只迁到 V10，构造升级前的存量库。
	if err := migrate.Run(ctx, db, migrate.All[:10]); err != nil {
		t.Fatalf("migrate 到 V10: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES('xiaoming')`); err != nil {
		t.Fatal(err)
	}

	// 存量已固化卷：dedupe_key 是装篮创建时算出的活跃键（source_kind|title|题目摘要），
	// 固化转 assigned 后未释放——与线上 bug 现场一致。键值用 schema 同款函数派生，
	// 保证与之后 AddToBasket 新建篮的键**逐字节相同**（同键冲突场景）。
	item := k12.PracticeItem{
		SourceProblemID: "mist-1", Subject: "数学", AddedVia: k12.PracticeAddedViaWeekly,
		QuestionMarkdown: "2.8 × 0.65 = ?", ExpectedAnswerMarkdown: "1.82",
		VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "原题重现·已带批改答案",
	}
	// "待打印篮" = usecase 装篮建篮的固定标题（basketTitle）。
	basketRec, err := k12.NewPracticeSetRecord("xiaoming", "s", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceMixed, Title: "待打印篮", Items: []k12.PracticeItem{item},
	})
	if err != nil {
		t.Fatal(err)
	}
	activeKey := k12.PracticeSetSchema().DedupeKey(basketRec)

	if _, err := db.ExecContext(ctx, `INSERT INTO k12_practice_sets
		(record_id, agent_name, schema_version, status, source_kind, title, paper_no,
		 delivery_status, dedupe_key, tags_json, source_session_id, version, created_at, updated_at)
		VALUES ('old-paper','xiaoming',1,'assigned','weekly','本周复习卷 · 07/18','P-2629-01',
		 'not_sent',?, '[]','s',3,1000,1000)`, activeKey); err != nil {
		t.Fatalf("插入存量固化卷: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_practice_set_items
		(set_record_id, item_index, item_id, source_problem_id, subject, added_via,
		 question_markdown, expected_answer_markdown, verification_status, verification_evidence, paper_seq)
		VALUES ('old-paper',0,'item-1','mist-1','数学','weekly','2.8 × 0.65 = ?','1.82','verified','原题重现·已带批改答案',1)`); err != nil {
		t.Fatalf("插入存量卷题目: %v", err)
	}
	// 存量归档作品（未修）+ 已手工 SQL 修过的归档作品（V11 必须原样跳过，不二次叠加）。
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_creative_works
		(record_id, agent_name, schema_version, status, work_type, title, task,
		 dedupe_key, tags_json, source_session_id, version, created_at, updated_at)
		VALUES ('old-work','xiaoming',1,'archived','writing','我的好爸爸','写一篇写人记叙文',
		 'workkey-1','[]','s',1,1000,1000),
		       ('fixed-work','xiaoming',1,'archived','writing','我的好妈妈','写一篇写人记叙文',
		 'workkey-2#released#fixed-work','[]','s',1,1000,1000)`); err != nil {
		t.Fatalf("插入存量归档作品: %v", err)
	}
	// 活跃对照行：draft 篮空间之外不许被误动。
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_practice_sets
		(record_id, agent_name, schema_version, status, source_kind, title,
		 delivery_status, dedupe_key, tags_json, source_session_id, version, created_at, updated_at)
		VALUES ('live-draft','xiaoming',1,'draft','custom','自定义卷',
		 'not_sent','draftkey-1','[]','s',0,1000,1000)`); err != nil {
		t.Fatalf("插入活跃 draft 对照行: %v", err)
	}

	// 2) 升级到 V11。
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatalf("migrate 到 V11: %v", err)
	}

	keyOf := func(table, id string) string {
		t.Helper()
		var k string
		if err := db.QueryRowContext(ctx,
			`SELECT dedupe_key FROM `+table+` WHERE record_id = ?`, id).Scan(&k); err != nil {
			t.Fatalf("查 %s.%s dedupe_key: %v", table, id, err)
		}
		return k
	}
	if got, want := keyOf("k12_practice_sets", "old-paper"), activeKey+"#released#old-paper"; got != want {
		t.Fatalf("存量固化卷应回填墓碑键:\n got %q\nwant %q", got, want)
	}
	if got, want := keyOf("k12_creative_works", "old-work"), "workkey-1#released#old-work"; got != want {
		t.Fatalf("存量归档作品应回填墓碑键: got %q want %q", got, want)
	}
	if got := keyOf("k12_creative_works", "fixed-work"); got != "workkey-2#released#fixed-work" {
		t.Fatalf("已手工修过的行必须原样保留（不得二次叠加）: got %q", got)
	}
	if got := keyOf("k12_practice_sets", "live-draft"); got != "draftkey-1" {
		t.Fatalf("draft 活跃行不得被回填: got %q", got)
	}

	// 3) 幂等可重跑：整段 V11 SQL 再执行一次，所有键保持不变。
	var v11 migrate.Migration
	for _, m := range migrate.All {
		if m.Version == 11 {
			v11 = m
			break
		}
	}
	if v11.Version != 11 {
		t.Fatalf("迁移清单应包含 V11, got v%d", v11.Version)
	}
	if _, err := db.ExecContext(ctx, v11.SQL); err != nil {
		t.Fatalf("V11 重跑: %v", err)
	}
	if got := keyOf("k12_practice_sets", "old-paper"); strings.Count(got, "#released#") != 1 {
		t.Fatalf("重跑后墓碑键不得叠加: got %q", got)
	}

	// 4) 端到端收口：回填后，相同首题组合的新篮真的能建出来（升级前会被 old-paper 截胡）。
	cur := curriculum.New()
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(cur)); err != nil {
		t.Fatal(err)
	}
	d := usecase.Deps{Records: k12storage.NewStore(db, reg.Records), Constraint: cur, Now: func() int64 { return 2000 }}
	// live-draft 是既有篮（单 Learner 单篮），先取消腾空，逼出「同键新建」路径。
	if err := d.CancelPracticeSet(ctx, "xiaoming", "live-draft"); err != nil {
		t.Fatalf("取消对照篮: %v", err)
	}
	id, added, err := d.AddToBasket(ctx, "xiaoming", "s", item)
	if err != nil {
		t.Fatalf("V11 后装篮: %v", err)
	}
	if !added || id == "old-paper" {
		t.Fatalf("V11 后新建同题篮应成功且不命中旧卷, got added=%v id=%s", added, id)
	}
	v, err := d.GetPracticeSet(ctx, "xiaoming", id)
	if err != nil {
		t.Fatalf("取新篮: %v", err)
	}
	if v.Record.Status != k12.PracticeStatusDraft || len(v.Fields.Items) != 1 {
		t.Fatalf("新篮应为 draft 且题目真实存在, got status=%s items=%d", v.Record.Status, len(v.Fields.Items))
	}
}
