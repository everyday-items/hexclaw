package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// BUG-20260718 真机素材验证发现：作品去重键（类型+题名+任务）与状态无关，归档不释放
// 唯一键。复现链（真实 HTTP 实录）：
//  1. 家长误建了只挂照片没有文字的写作作品（draft）；
//  2. draft 不能交修改稿（生命周期正确拒绝）→ 家长只能归档；
//  3. 归档后按同一题名重建 → 去重命中已归档记录（created=false，返回归档件 ID），
//     新内容静默丢弃——同题名作品从此永远建不回来（死路闭环）。
//
// 期望：归档=退出活跃空间，应释放去重键；同题名重建必须 created=true 且是新记录。
func TestBug20260718_ArchivedWorkBlocksSameTitleRecreate(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()

	f := k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "我的好爸爸", Task: "写一篇写人记叙文",
		Versions: []k12.CreativeWorkVersion{{SourceAssetID: "data:image/png;base64,xxxx"}},
	}
	id1, created, err := d.CreateCreativeWork(ctx, "xiaoming", "s", f)
	if err != nil || !created {
		t.Fatalf("首次创建: created=%v err=%v", created, err)
	}
	if err := d.ArchiveCreativeWork(ctx, "xiaoming", id1); err != nil {
		t.Fatalf("归档: %v", err)
	}

	// 归档后同题名重建：必须成功产出新记录，而不是去重命中归档件。
	f2 := k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "我的好爸爸", Task: "写一篇写人记叙文",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "我的爸爸是一名程序员……"}},
	}
	id2, created2, err := d.CreateCreativeWork(ctx, "xiaoming", "s", f2)
	if err != nil {
		t.Fatalf("归档后重建: %v", err)
	}
	if !created2 {
		t.Fatalf("归档后同题名重建应 created=true（归档应释放去重键），got created=false（命中 %s）", id2)
	}
	if id2 == id1 {
		t.Fatalf("重建应产出新记录，got 归档件本身 %s", id1)
	}
	v2, err := d.GetCreativeWork(ctx, "xiaoming", id2)
	if err != nil {
		t.Fatalf("取新作品: %v", err)
	}
	if v2.Record.Status != k12.WorkStatusDraft || v2.Fields.Versions[0].ContentMarkdown == "" {
		t.Fatalf("新作品应为 draft 且带文字内容，got status=%s", v2.Record.Status)
	}

	// 活跃期语义不回归：新作品在（非归档）活跃期内，同题名再建仍去重命中。
	id3, created3, err := d.CreateCreativeWork(ctx, "xiaoming", "s", f2)
	if err != nil {
		t.Fatalf("活跃期重复创建: %v", err)
	}
	if created3 || id3 != id2 {
		t.Fatalf("活跃期同题名应保持幂等去重，got created=%v id=%s（期望命中 %s）", created3, id3, id2)
	}

	// 二次归档-重建循环也必须可行（tombstone 键不得相互碰撞）。
	if err := d.ArchiveCreativeWork(ctx, "xiaoming", id2); err != nil {
		t.Fatalf("二次归档: %v", err)
	}
	_, created4, err := d.CreateCreativeWork(ctx, "xiaoming", "s", f2)
	if err != nil || !created4 {
		t.Fatalf("二次归档后重建: created=%v err=%v", created4, err)
	}
}
