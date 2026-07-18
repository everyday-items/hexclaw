package usecase_test

// 作品照片资产归属契约（任务1 归属隔离）：asset:// 载体的 owner ≠ 作品实例时，
// 创建作品 / 提交修改稿在入口拒绝——跨孩/跨实例照片引用不落库。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const foreignAsset = "asset://honghong/" +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png"

func TestCreativeWork_RejectsForeignAsset(t *testing.T) {
	d := newDataDeps(t)
	_, _, err := d.CreateCreativeWork(context.Background(), "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "画", Task: "写生",
		Versions: []k12.CreativeWorkVersion{{SourceAssetID: foreignAsset}},
	})
	if err == nil || !strings.Contains(err.Error(), "不属于该实例") {
		t.Fatalf("跨实例资产应在创建入口拒绝, got %v", err)
	}

	// 本实例资产 ID（格式合法即可，落库不要求文件已存在——点评时才读盘）可创建；
	// 修改稿引用他人资产同样拒绝。
	id, _, err := d.CreateCreativeWork(context.Background(), "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "画", Task: "写生",
		Versions: []k12.CreativeWorkVersion{{SourceAssetID: strings.Replace(foreignAsset, "honghong", "xiaoming", 1)}},
	})
	if err != nil {
		t.Fatalf("本实例资产应可创建: %v", err)
	}
	if _, err := d.AttachFeedback(context.Background(), "xiaoming", id, "构图完整。"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SubmitRevision(context.Background(), "xiaoming", id, "", foreignAsset); err == nil {
		t.Fatal("修改稿引用他人资产应拒绝")
	}
}
