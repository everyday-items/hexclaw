package usecase_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 当前产品把每次上传或保存都视为独立作品；历史 revision 只允许读取，不再接受任何新写。
func TestSubmitRevision_CurrentContractRejectsAllRevisionWrites(t *testing.T) {
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

	before, err := d.GetCreativeWork(ctx, "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	wantVersions := append([]k12.CreativeWorkVersion(nil), before.Fields.Versions...)
	wantStatus := before.Record.Status
	wantRecordVersion := before.Record.Version

	assertUnchanged := func(t *testing.T) {
		t.Helper()
		after, err := d.GetCreativeWork(ctx, "xiaoming", id)
		if err != nil {
			t.Fatal(err)
		}
		if after.Record.Status != wantStatus || after.Record.Version != wantRecordVersion {
			t.Fatalf(
				"revision 拒绝后根状态发生变化: status/version=%s/%d, want %s/%d",
				after.Record.Status, after.Record.Version, wantStatus, wantRecordVersion,
			)
		}
		if !reflect.DeepEqual(after.Fields.Versions, wantVersions) {
			t.Fatalf("revision 拒绝后 legacy versions 发生变化: got=%+v want=%+v",
				after.Fields.Versions, wantVersions)
		}
	}

	for _, tc := range []struct {
		name, contentMarkdown, sourceAssetID string
	}{
		{name: "空输入"},
		{name: "照片输入", sourceAssetID: "asset-photo-1"},
		{name: "文本输入", contentMarkdown: "孩子粘贴的修改稿"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := d.SubmitRevision(
				ctx, "xiaoming", id, tc.contentMarkdown, tc.sourceAssetID,
			); err == nil {
				t.Fatal("已退役 revision 写入应被拒绝")
			} else if !errors.Is(err, usecase.ErrInvalidInput) {
				t.Fatalf("已退役 revision 写入应返回 ErrInvalidInput, got %v", err)
			}
			assertUnchanged(t)
		})
	}
}
