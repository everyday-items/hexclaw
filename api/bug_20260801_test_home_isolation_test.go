package api

import (
	"os"
	"strings"
	"testing"
)

func TestBug20260801_APITestsUseIsolatedHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("读取测试 HOME 失败: %v", err)
	}
	if !strings.Contains(home, "hexclaw-test-home-") {
		t.Fatalf("api 测试不得继承用户 HOME，got %q", home)
	}
}
