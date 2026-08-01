package api

import (
	"fmt"
	"os"
	"testing"

	"github.com/hexagon-codes/hexclaw/internal/testutil/testhome"
)

func TestMain(m *testing.M) {
	cleanup, err := testhome.Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "隔离 api 测试 HOME 失败:", err)
		os.Exit(2)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
