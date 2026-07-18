package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// TestSubmitRevision_RequiresRealUpload 修改稿必须来自真实上传（§3.10，2026-07-18 裁决：
// 照片或粘贴文本，禁止凭空 +1 版本）——content 与 asset 至少一项非空。
func TestSubmitRevision_RequiresRealUpload(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "《春天的校园》", Task: "观察春景写一段",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "柳枝像绿色的丝带……"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachFeedback(ctx, "xiaoming", id, "结构清晰，可加感官细节。"); err != nil {
		t.Fatal(err)
	}

	// 空修改稿（无文本无照片）→ 拒绝，版本数不变。
	if _, err := d.SubmitRevision(ctx, "xiaoming", id, "", ""); err == nil {
		t.Fatal("空修改稿应被拒（禁止凭空 +1 版本）")
	} else if !errors.Is(err, usecase.ErrInvalidInput) {
		t.Errorf("应为 ErrInvalidInput（HTTP 400），got %v", err)
	}
	v, _ := d.GetCreativeWork(ctx, "xiaoming", id)
	if len(v.Fields.Versions) != 1 {
		t.Fatalf("拒绝后版本数应仍为 1，got %d", len(v.Fields.Versions))
	}

	// 只有照片 asset（无 OCR 文本）也是真实上传 → 允许。
	rev, err := d.SubmitRevision(ctx, "xiaoming", id, "", "asset-photo-1")
	if err != nil {
		t.Fatalf("仅照片上传应允许: %v", err)
	}
	if len(rev.Fields.Versions) != 2 {
		t.Fatalf("应形成 v2，got %d", len(rev.Fields.Versions))
	}
}
