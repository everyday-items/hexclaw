package assetstore_test

// BUG-20260718（测试验收清单 §15 / CRUD-002/010 / E2E-DELETE-001）：删除 Agent 时
// assetstore「明确无删除能力」，孩子的作品照片文件在档案删除后仍残留本机——与 §3.12
// 「照片仅本机、随档案抹除」承诺矛盾。此测试先钉死 DeleteAgent 的归属删除契约（RED
// 先行：DeleteAgent/SnapshotAgent 尚未存在），修复后必须转绿并长期防回归。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

func TestBug20260718_DeleteAgentRemovesResidualAssets(t *testing.T) {
	root := withRoot(t)
	if _, err := assetstore.Save("mingming", tinyPNG(t)); err != nil {
		t.Fatal(err)
	}
	// 另一个孩子的资产必须不受牵连（归属隔离硬边界）。
	if _, err := assetstore.Save("honghong", tinyPNG(t)); err != nil {
		t.Fatal(err)
	}

	n, err := assetstore.DeleteAgent("mingming")
	if err != nil {
		t.Fatalf("DeleteAgent 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应删除 1 个资产文件, got %d", n)
	}
	if _, err := os.Stat(filepath.Join(root, "mingming")); !os.IsNotExist(err) {
		t.Fatalf("删除后 agent 资产目录必须不存在, stat err=%v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "honghong")); len(entries) != 1 {
		t.Fatalf("跨 agent 资产不得被误删, honghong 剩 %d", len(entries))
	}

	// 幂等：再删（目录已不存在）不报错、返回 0。
	if n2, err := assetstore.DeleteAgent("mingming"); err != nil || n2 != 0 {
		t.Fatalf("重复删除应幂等 (n=%d err=%v)", n2, err)
	}
}

func TestBug20260718_DeleteAgentRejectsUnsafeAgent(t *testing.T) {
	withRoot(t)
	for _, agent := range []string{"", "..", "a/b", `a\b`, "a/../b"} {
		if _, err := assetstore.DeleteAgent(agent); err == nil {
			t.Fatalf("不安全 agent %q 必须拒绝（防穿越误删）", agent)
		}
	}
}

func TestBug20260718_SnapshotRestoreRoundtripForSaga(t *testing.T) {
	root := withRoot(t)
	id, err := assetstore.Save("mingming", tinyPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	_, file, _ := assetstore.Parse(id)

	// 注销 saga 补偿：删前留内存快照，归属删除失败时原样回填。
	snap, err := assetstore.SnapshotAgent("mingming")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assetstore.DeleteAgent("mingming"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "mingming", file)); !os.IsNotExist(err) {
		t.Fatal("删除后文件应消失")
	}
	if err := snap.Restore(); err != nil {
		t.Fatalf("回滚补偿 Restore 失败: %v", err)
	}
	data, _, err := assetstore.Read("mingming", file)
	if err != nil {
		t.Fatalf("回滚后应可读回原资产: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("回滚后资产为空")
	}
}
