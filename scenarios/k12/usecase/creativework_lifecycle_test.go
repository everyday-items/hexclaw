package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// TestCreativeWorkWritingLifecycle 语文写作：draft→feedback_ready，修改稿拒绝后独立新建并再点评。
func TestCreativeWorkWritingLifecycle(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "《春天的校园》", Task: "观察春景写一段",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "柳枝像绿色的丝带……"}},
	}
	id, created, err := d.CreateCreativeWork(ctx, "xiaoming", "s", f)
	if err != nil || !created {
		t.Fatalf("创建作品: created=%v err=%v", created, err)
	}
	v, _ := d.GetCreativeWork(ctx, "xiaoming", id)
	if v.Record.Status != k12.WorkStatusDraft {
		t.Fatalf("初始应为 draft，got %s", v.Record.Status)
	}
	if k12.CreativeWorkLabel(v.Record.Status) != "待点评" {
		t.Fatalf("draft 译名应为 待点评")
	}
	if v.Fields.Versions[0].VersionID != "v1" {
		t.Fatalf("首版应为 v1，got %s", v.Fields.Versions[0].VersionID)
	}

	fb := generateCreativeWorkFeedbackForTest(
		t, &d, id, "切题；结构三段清晰；「像绿色的丝带」比喻好；建议再加一个感官细节。",
	)
	if fb.Record.Status != k12.WorkStatusFeedbackReady {
		t.Fatalf("点评后应为 feedback_ready，got %s", fb.Record.Status)
	}
	if fb.Fields.Versions[0].Feedback == "" {
		t.Fatal("点评应写入最新版本")
	}

	var rootsBefore, versionRowsBefore int
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_creative_works
		WHERE agent_name='xiaoming'`).Scan(&rootsBefore); err != nil {
		t.Fatal(err)
	}
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_creative_work_versions
		WHERE work_record_id=?`, id).Scan(&versionRowsBefore); err != nil {
		t.Fatal(err)
	}
	if rootsBefore != 1 || versionRowsBefore != 1 {
		t.Fatalf("历史基线漂移: roots=%d version_rows=%d", rootsBefore, versionRowsBefore)
	}

	if _, err := d.SubmitRevision(
		ctx, "xiaoming", id, "柳枝像绿色的丝带，风一吹就沙沙响。", "",
	); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("修改稿应在写入前拒绝: %v", err)
	}
	unchanged, err := d.GetCreativeWork(ctx, "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Record.RecordID != fb.Record.RecordID ||
		unchanged.Record.Status != fb.Record.Status ||
		unchanged.Record.Version != fb.Record.Version ||
		len(unchanged.Fields.Versions) != 1 ||
		unchanged.Fields.Versions[0].VersionID != "v1" ||
		unchanged.Fields.Versions[0].Feedback != fb.Fields.Versions[0].Feedback {
		t.Fatalf("修改稿拒绝后改写了根作品或历史版本: before=%+v after=%+v", fb, unchanged)
	}
	var rootsAfterReject, versionRowsAfterReject int
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_creative_works
		WHERE agent_name='xiaoming'`).Scan(&rootsAfterReject); err != nil {
		t.Fatal(err)
	}
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_creative_work_versions
		WHERE work_record_id=?`, id).Scan(&versionRowsAfterReject); err != nil {
		t.Fatal(err)
	}
	if rootsAfterReject != rootsBefore || versionRowsAfterReject != versionRowsBefore {
		t.Fatalf("修改稿拒绝泄漏了写入: roots=%d/%d version_rows=%d/%d",
			rootsAfterReject, rootsBefore, versionRowsAfterReject, versionRowsBefore)
	}

	newContent := "柳枝像绿色的丝带，风一吹就沙沙响。"
	newID, generationID, created, err := d.CreateCurrentTextWork(
		ctx, "xiaoming", newContent, "creative-lifecycle-independent-work",
	)
	if err != nil || !created || newID == "" || generationID == "" || newID == id {
		t.Fatalf("独立新作品创建: id=%q old=%q generation=%q created=%v err=%v",
			newID, id, generationID, created, err)
	}
	independent, err := d.GetCreativeWork(ctx, "xiaoming", newID)
	if err != nil {
		t.Fatal(err)
	}
	if len(independent.Fields.Versions) != 0 ||
		independent.GenerationState.Initial == nil ||
		independent.GenerationState.Initial.GenerationID != generationID ||
		independent.GenerationState.Initial.WorkID != newID ||
		independent.GenerationState.Initial.Source.ContentMarkdown != newContent {
		t.Fatalf("独立作品身份/冻结证据不一致: %+v", independent)
	}
	var rootsAfterCreate, oldVersionRows, newVersionRows int
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_creative_works
		WHERE agent_name='xiaoming'`).Scan(&rootsAfterCreate); err != nil {
		t.Fatal(err)
	}
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_creative_work_versions
		WHERE work_record_id=?`, id).Scan(&oldVersionRows); err != nil {
		t.Fatal(err)
	}
	if err := d.Records.DB().QueryRow(`SELECT count(*) FROM k12_creative_work_versions
		WHERE work_record_id=?`, newID).Scan(&newVersionRows); err != nil {
		t.Fatal(err)
	}
	if rootsAfterCreate != rootsBefore+1 || oldVersionRows != versionRowsBefore || newVersionRows != 0 {
		t.Fatalf("独立新建数量不变量失效: roots=%d old_versions=%d new_versions=%d",
			rootsAfterCreate, oldVersionRows, newVersionRows)
	}

	// 独立新作品可从同一首轮 generation 完成点评。
	fb2 := generateCreativeWorkFeedbackForTest(t, &d, newID, "加了听觉细节，更生动了；建议保留这个细节。")
	if fb2.Record.Status != k12.WorkStatusFeedbackReady {
		t.Fatalf("独立新作品点评后应为 feedback_ready")
	}
	if fb2.GenerationState.Initial == nil || fb2.GenerationState.Latest == nil ||
		fb2.GenerationState.Initial.GenerationID != generationID ||
		fb2.GenerationState.Latest.GenerationID != generationID ||
		fb2.GenerationState.Latest.Feedback == nil {
		t.Fatal("独立新作品点评应收敛在同一首轮 generation")
	}
}

// TestCreativeWorkArtLifecycle 美术作品：意图 + 点评，不打分不代写（INV-011）。
func TestCreativeWorkArtLifecycle(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "《雨后的校园》", Task: "画一处雨后场景",
		Intent:   "想画出雨后安静的感觉",
		Versions: []k12.CreativeWorkVersion{{SourceAssetID: "asset-1"}},
	}
	id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", f)
	if err != nil {
		t.Fatal(err)
	}
	fb := generateCreativeWorkFeedbackForTest(
		t, &d, id, "构图主体偏右，天空留白呼应了安静；建议让地面倒影再明显一点。",
	)
	if fb.Fields.Intent != "想画出雨后安静的感觉" {
		t.Fatal("创作意图应保留")
	}
}

// TestCreativeWorkTypeFilter 按类型过滤。
func TestCreativeWorkTypeFilter(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "作文A", Task: "任务A",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "x"}}})
	d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "画作B", Task: "任务B",
		Versions: []k12.CreativeWorkVersion{{SourceAssetID: "a"}}})

	writings, _ := d.ListCreativeWorks(ctx, "xiaoming", k12.WorkTypeWriting)
	arts, _ := d.ListCreativeWorks(ctx, "xiaoming", k12.WorkTypeArt)
	all, _ := d.ListCreativeWorks(ctx, "xiaoming", "")
	if len(writings) != 1 || len(arts) != 1 || len(all) != 2 {
		t.Fatalf("类型过滤错误: writing=%d art=%d all=%d", len(writings), len(arts), len(all))
	}
}

// TestCreativeWorkInvalidType 非法类型被拒。
func TestCreativeWorkInvalidType(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	_, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: "math", Title: "错类型", Task: "t",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "x"}}})
	if err == nil {
		t.Fatal("非法作品类型应被拒")
	}
}

// TestCreativeWorkOwnerIsolation 跨实例隔离。
func TestCreativeWorkOwnerIsolation(t *testing.T) {
	d := newDataDeps(t, "xiaoming", "xiaohong")
	ctx := context.Background()
	id, _, _ := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "小明作文", Task: "t",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "x"}}})
	if _, err := d.GetCreativeWork(ctx, "xiaohong", id); err == nil {
		t.Fatal("小红不应读到小明的作品")
	}
}
