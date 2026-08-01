package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBug20260801_BaiduLiveTestRequiresExplicitOptInBeforeEngineExecution(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位当前回归测试文件")
	}
	sourcePath := filepath.Join(filepath.Dir(thisFile), "bug_20260704_no_items_selfheal_convergence_test.go")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("读取真实百度 E2E 用例失败: %v", err)
	}

	const function = "func TestBug20260704_BaiduCollectTemplate_LiveTop20(t *testing.T)"
	liveTest := string(source)
	start := strings.Index(liveTest, function)
	if start < 0 {
		t.Fatalf("未找到真实百度 E2E 用例 %q", function)
	}
	liveTest = liveTest[start:]

	const explicitGate = "os.Getenv(\"HEX_LIVE_BAIDU_COLLECT_E2E\") != \"1\""
	gateAt := strings.Index(liveTest, explicitGate)
	executeAt := strings.Index(liveTest, "NewStarlarkEngine().Execute")
	if gateAt < 0 {
		t.Fatalf("真实百度 E2E 必须先检查显式开关 %q，避免普通 go test 隐式联网", explicitGate)
	}
	if executeAt < 0 {
		t.Fatal("未找到真实百度 E2E 的引擎执行点")
	}
	if gateAt >= executeAt {
		t.Fatal("真实百度 E2E 的显式开关必须位于引擎执行前")
	}
	if skipAt := strings.Index(liveTest[gateAt:executeAt], "t.Skip("); skipAt < 0 {
		t.Fatal("未显式授权时真实百度 E2E 必须在引擎执行前 SKIP")
	}
}
