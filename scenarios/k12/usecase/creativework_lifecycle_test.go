package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// TestCreativeWorkWritingLifecycle 语文写作：draft→feedback_ready→revised→再点评。
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

	rev, err := d.SubmitRevision(ctx, "xiaoming", id, "柳枝像绿色的丝带，风一吹就沙沙响。", "")
	if err != nil {
		t.Fatalf("提交修改稿: %v", err)
	}
	if rev.Record.Status != k12.WorkStatusRevised {
		t.Fatalf("修改后应为 revised，got %s", rev.Record.Status)
	}
	if len(rev.Fields.Versions) != 2 || rev.Fields.Versions[1].VersionID != "v2" {
		t.Fatalf("应形成 v2，got %d 版", len(rev.Fields.Versions))
	}

	// revised 可再点评回 feedback_ready。
	fb2 := generateCreativeWorkFeedbackForTest(t, &d, id, "加了听觉细节，更生动了；建议保留这个细节。")
	if fb2.Record.Status != k12.WorkStatusFeedbackReady {
		t.Fatalf("二次点评后应为 feedback_ready")
	}
	if fb2.Fields.Versions[1].Feedback == "" {
		t.Fatal("二次点评应落在 v2")
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
