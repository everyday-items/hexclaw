package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// TestSubmitRevision_RequiresRealUpload 修改稿必须来自真实上传（§3.10，2026-07-18 裁决：
// 照片或粘贴文本，禁止凭空 +1 版本）。DD-013 后写作照片还必须携带 OCR 确认版本。
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
	generateCreativeWorkFeedbackForTest(t, &d, id, "结构清晰；建议加一个感官细节。")

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

	// 写作只有照片但没有 OCR 确认证据 → 拒绝，不能把上传成功冒充正文已确认。
	if _, err := d.SubmitRevision(ctx, "xiaoming", id, "", "asset-photo-1"); err == nil {
		t.Fatal("写作照片缺 OCR 确认应拒绝")
	} else if !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("写作照片确认门应为 ErrInvalidInput，got %v", err)
	}

	// 真实粘贴文本仍是合法修改稿降级路径。
	rev, err := d.SubmitRevision(ctx, "xiaoming", id, "孩子粘贴的修改稿", "")
	if err != nil {
		t.Fatalf("纯文本修改稿应允许: %v", err)
	}
	if len(rev.Fields.Versions) != 2 {
		t.Fatalf("应形成 v2，got %d", len(rev.Fields.Versions))
	}
}
