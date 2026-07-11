package usecase

import (
	"context"
	"errors"
	"testing"
)

// 回归锁（M4-1 补债）：Restore 拒绝高于当前支持版本的归档（不部分导入），低版本正常读。
func TestRestore_VersionGate(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()

	future := &Hexbak{Version: HexbakVersion + 1, AgentName: "mingming", Records: nil}
	sum, _ := checksumRecords(future.Records)
	future.Checksum = sum
	if _, err := d.Restore(ctx, future); !errors.Is(err, ErrVersionUnsupported) {
		t.Errorf("高版本归档应拒绝 ErrVersionUnsupported, got %v", err)
	}

	// 当前版本空档案正常（round-trip 0 条）。
	cur := &Hexbak{Version: HexbakVersion, AgentName: "mingming", Records: nil}
	cur.Checksum, _ = checksumHexbak(cur)
	if _, err := d.Restore(ctx, cur); err != nil {
		t.Errorf("当前版本应正常恢复, got %v", err)
	}
}
