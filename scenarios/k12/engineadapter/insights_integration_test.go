package engineadapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/memory"
)

// TestInsightsAdapter_RealFileMemory 集成验证：adapter 对接**真的 FileMemory**，
// 学情信号真的落盘、且按 agentName(role) 隔离到对应子目录。
func TestInsightsAdapter_RealFileMemory(t *testing.T) {
	dir := t.TempDir()
	fm, err := memory.New(memory.Options{Dir: dir})
	if err != nil {
		t.Fatalf("建 FileMemory: %v", err)
	}
	// *memory.FileMemory 满足 memoryWriter（编译期即证 adapter 接口对齐）
	a := NewInsightsAdapter(fm)

	if err := a.WriteWeakness(context.Background(), "mingming", "小数乘法", "在「小数乘法」出错：计算失误"); err != nil {
		t.Fatalf("WriteWeakness: %v", err)
	}

	// 真的落盘了：临时目录下某文件含 [学情] + note，且在 mingming 子目录（隔离）
	found, inMingmingDir := false, false
	filepath.Walk(dir, func(path string, info os.FileInfo, _ error) error {
		if info == nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "[学情]") && strings.Contains(string(b), "计算失误") {
			found = true
			if strings.Contains(path, "mingming") {
				inMingmingDir = true
			}
		}
		return nil
	})
	if !found {
		t.Error("学情信号应真的落盘（含 [学情] + 错因）")
	}
	if !inMingmingDir {
		t.Error("学情信号应落在 mingming 子目录（多孩隔离，role=agentName）")
	}
}
