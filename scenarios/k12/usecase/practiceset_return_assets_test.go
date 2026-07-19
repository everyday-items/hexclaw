package usecase_test

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func saveReturnAsset(t *testing.T, agent string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	id, err := assetstore.Save(agent, raw)
	if err != nil {
		t.Fatalf("保存回传照片: %v", err)
	}
	return id
}

func finalizedReturnSet(t *testing.T, d usecase.Deps, agent string) string {
	t.Helper()
	id, _, err := d.CreatePracticeSet(context.Background(), agent, "s-return", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual,
		Title:      "回传契约卷",
		Items: []k12.PracticeItem{
			verifiedItem("q1", "1+1=?", "2"),
			verifiedItem("q2", "2+2=?", "4"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.FinalizeBasket(context.Background(), agent, id, "print", ""); err != nil {
		t.Fatal(err)
	}
	return id
}

func submitWholeSet(t *testing.T, d usecase.Deps, agent, setID string) usecase.PracticeSetView {
	t.Helper()
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	assetID := saveReturnAsset(t, agent)
	v, err := d.GetPracticeSet(context.Background(), agent, setID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(v.Fields.Items))
	for _, it := range v.Fields.Items {
		if k12.PracticeItemPublishable(it) {
			ids = append(ids, it.ItemID)
		}
	}
	v, err = d.SubmitReturn(context.Background(), agent, setID, "return-test-"+setID, assetID, ids)
	if err != nil {
		t.Fatalf("回传整卷测试证据: %v", err)
	}
	return v
}

func TestSubmitReturn_AppendsAssetEvidenceAndIsExactlyIdempotent(t *testing.T) {
	d := newDataDeps(t)
	id := finalizedReturnSet(t, d, "xiaoming")
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	assetID := saveReturnAsset(t, "xiaoming")

	v, err := d.SubmitReturn(context.Background(), "xiaoming", id, "return-1", assetID, []string{"q1"})
	if err != nil {
		t.Fatalf("首次回传: %v", err)
	}
	if v.Record.Status != k12.PracticeStatusSubmitted || len(v.Fields.ReturnAssets) != 1 {
		t.Fatalf("应追加一条回传证据并进入 submitted: status=%s assets=%+v", v.Record.Status, v.Fields.ReturnAssets)
	}
	got := v.Fields.ReturnAssets[0]
	if got.ReturnID != "return-1" || got.AssetID != assetID || got.ReturnedAt != 1000 || len(got.ItemIDs) != 1 || got.ItemIDs[0] != "q1" {
		t.Fatalf("回传证据未完整持久化: %+v", got)
	}
	if !v.Fields.Items[0].Returned || v.Fields.Items[1].Returned {
		t.Fatalf("题级 returned 映射错误: %+v", v.Fields.Items)
	}
	version := v.Record.Version

	// 完全相同的 return_id 重投只能返回既有结果，不追加、不推进 version。
	v, err = d.SubmitReturn(context.Background(), "xiaoming", id, "return-1", assetID, []string{"q1"})
	if err != nil {
		t.Fatalf("幂等重投: %v", err)
	}
	if len(v.Fields.ReturnAssets) != 1 || v.Record.Version != version {
		t.Fatalf("幂等重投产生了副作用: version %d→%d assets=%d", version, v.Record.Version, len(v.Fields.ReturnAssets))
	}

	// 同一题允许由下一批照片再次覆盖；旧证据不可覆盖。
	v, err = d.SubmitReturn(context.Background(), "xiaoming", id, "return-2", assetID, []string{"q1", "q2"})
	if err != nil {
		t.Fatalf("补传: %v", err)
	}
	if len(v.Fields.ReturnAssets) != 2 || !v.Fields.Items[0].Returned || !v.Fields.Items[1].Returned {
		t.Fatalf("补传应只追加且更新题级投影: %+v", v.Fields)
	}
}

func TestSubmitReturn_ConcurrentExactReplayAppendsOnce(t *testing.T) {
	d := newDataDeps(t)
	id := finalizedReturnSet(t, d, "xiaoming")
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	assetID := saveReturnAsset(t, "xiaoming")
	before, err := d.GetPracticeSet(context.Background(), "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = d.SubmitReturn(context.Background(), "xiaoming", id, "return-concurrent", assetID, []string{"q1", "q2"})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发幂等请求 #%d: %v", i, err)
		}
	}
	v, err := d.GetPracticeSet(context.Background(), "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Fields.ReturnAssets) != 1 || v.Fields.ReturnAssets[0].ReturnID != "return-concurrent" || v.Record.Version != before.Record.Version+1 {
		t.Fatalf("并发完全重放必须只追加一次且只推进一次版本: %+v", v)
	}
}

func TestSubmitReturn_RejectsMissingForeignOrConflictingEvidenceWithoutMutation(t *testing.T) {
	d := newDataDeps(t, "xiaoming", "lele")
	id := finalizedReturnSet(t, d, "xiaoming")
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	owned := saveReturnAsset(t, "xiaoming")
	foreign := saveReturnAsset(t, "lele")

	cases := []struct {
		name, returnID, assetID string
		items                   []string
	}{
		{name: "missing return id", assetID: owned, items: []string{"q1"}},
		{name: "missing asset", returnID: "r-missing", items: []string{"q1"}},
		{name: "missing items", returnID: "r-empty", assetID: owned},
		{name: "foreign asset", returnID: "r-foreign", assetID: foreign, items: []string{"q1"}},
		{name: "unknown item", returnID: "r-unknown", assetID: owned, items: []string{"ghost"}},
		{name: "duplicate item", returnID: "r-dup", assetID: owned, items: []string{"q1", "q1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := d.SubmitReturn(context.Background(), "xiaoming", id, tc.returnID, tc.assetID, tc.items); !errors.Is(err, usecase.ErrInvalidInput) {
				t.Fatalf("应拒绝且标记 ErrInvalidInput, got %v", err)
			}
			v, err := d.GetPracticeSet(context.Background(), "xiaoming", id)
			if err != nil {
				t.Fatal(err)
			}
			if v.Record.Status != k12.PracticeStatusAssigned || len(v.Fields.ReturnAssets) != 0 {
				t.Fatalf("失败不得产生任何回传写入: status=%s assets=%+v", v.Record.Status, v.Fields.ReturnAssets)
			}
		})
	}

	v, err := d.SubmitReturn(context.Background(), "xiaoming", id, "r-conflict", owned, []string{"q1"})
	if err != nil {
		t.Fatal(err)
	}
	before := v.Record.Version
	if _, err := d.SubmitReturn(context.Background(), "xiaoming", id, "r-conflict", owned, []string{"q2"}); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("同 return_id 不同载荷必须冲突, got %v", err)
	}
	v, _ = d.GetPracticeSet(context.Background(), "xiaoming", id)
	if v.Record.Version != before || len(v.Fields.ReturnAssets) != 1 || v.Fields.Items[1].Returned {
		t.Fatalf("冲突重投不得改写既有证据: %+v", v)
	}
}

func TestGradePracticeSetItems_RequiresReturnAssetCoverage(t *testing.T) {
	d := newDataDeps(t)
	id := finalizedReturnSet(t, d, "xiaoming")
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	assetID := saveReturnAsset(t, "xiaoming")
	if _, err := d.SubmitReturn(context.Background(), "xiaoming", id, "return-q1", assetID, []string{"q1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GradePracticeSetItems(context.Background(), "xiaoming", id,
		[]usecase.PracticeGradeResult{{ItemID: "q2", Correct: true}}); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("无照片覆盖的题不得被复批并反向伪造 returned, got %v", err)
	}
	v, _ := d.GetPracticeSet(context.Background(), "xiaoming", id)
	if v.Fields.Items[1].Returned || v.Fields.Items[1].ResultCorrect != nil {
		t.Fatalf("失败复批不得污染题级投影: %+v", v.Fields.Items[1])
	}
}
