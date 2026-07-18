package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// TestBackupCoversNewEntities 练习集与作品必须随 .hexbak 备份完整往返（PRD §3.15 逐对象一致）。
// records 底座的 ExportAgent 按实例导出全部 collection，新实体应自动被覆盖——本测试钉死该保证。
func TestBackupCoversNewEntities(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()

	psID, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly, Title: "备份卷",
		Items: []k12.PracticeItem{verifiedItem("q1", "备份题", "备份答")}})
	if err != nil {
		t.Fatal(err)
	}
	cwID, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "备份作文", Task: "任务",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "原稿内容"}}})
	if err != nil {
		t.Fatal(err)
	}

	bak, err := d.Backup(ctx, "xiaoming")
	if err != nil {
		t.Fatalf("导出备份: %v", err)
	}
	var hasPS, hasCW bool
	for _, r := range bak.Records {
		switch r.Collection {
		case k12.CollectionPracticeSet:
			hasPS = true
		case k12.CollectionCreativeWork:
			hasCW = true
		}
	}
	if !hasPS || !hasCW {
		t.Fatalf("备份未覆盖新实体: practiceSet=%v creativeWork=%v", hasPS, hasCW)
	}

	// 清库后恢复，逐对象验证练习集/作品回来且字段完整。
	if err := d.Records.Delete(ctx, "xiaoming", psID); err != nil {
		t.Fatal(err)
	}
	if err := d.Records.Delete(ctx, "xiaoming", cwID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Restore(ctx, bak); err != nil {
		t.Fatalf("恢复备份: %v", err)
	}
	sets, _ := d.ListPracticeSets(ctx, "xiaoming", "")
	works, _ := d.ListCreativeWorks(ctx, "xiaoming", "")
	if len(sets) != 1 || sets[0].Fields.Title != "备份卷" {
		t.Fatalf("练习集未完整恢复: %+v", sets)
	}
	if len(works) != 1 || works[0].Fields.Versions[0].ContentMarkdown != "原稿内容" {
		t.Fatalf("作品未完整恢复: %+v", works)
	}
}
