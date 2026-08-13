package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 持久化 PermissionHub 是生产环境唯一的审批权威。保留源码级守卫，
// 防止已淘汰的内存态 ToolApprovalGate 回归并形成第二条发布路径。
func TestProductionApprovalAuthorityHasNoLegacyToolApprovalGate(t *testing.T) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve engine source directory")
	}

	entries, err := os.ReadDir(filepath.Dir(currentFile))
	if err != nil {
		t.Fatalf("read engine source directory: %v", err)
	}
	legacyMarkers := []string{
		"type ToolApprovalGate struct",
		"func NewToolApprovalGate(",
		"alwaysAllowed map[string]map[string]bool",
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(filepath.Dir(currentFile), entry.Name())
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read production source %s: %v", entry.Name(), readErr)
		}
		for _, marker := range legacyMarkers {
			if strings.Contains(string(content), marker) {
				t.Fatalf("retired legacy approval authority remains in production source %s: %q", entry.Name(), marker)
			}
		}
	}
}
