package cron

// BUG-20260704 实机取证 harness（永久保留）：把实机 DB 导出的 spec_json 原样跑
// 真实百度，断言执行结果与预期一致。修复取证记录（2026-07-04）：
//   - 修复前（E2E 库 cron-zAHFWADE 的 LLM 盲猜脚本）：
//     BUG20260704_EXPECT=error → PASS（稳定复现 status=error /
//     「no items found in data structure」，1.07s，排除反爬与网络因素）
//   - 修复后（同一行 spec_json 换成 collect 模板）：
//     BUG20260704_EXPECT=success → PASS（status=success，TOP20 到手）
// 运行：
//   BUG20260704_SPEC=<spec_json文件> BUG20260704_EXPECT=success \
//     go test ./cron/ -run TestZZ_Bug20260704_LiveSpecReplay -v

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestZZ_Bug20260704_LiveSpecReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过真实网络复现")
	}
	specPath := os.Getenv("BUG20260704_SPEC")
	if specPath == "" {
		t.Skip("未设置 BUG20260704_SPEC（实机 spec_json 路径），跳过")
	}
	expect := os.Getenv("BUG20260704_EXPECT")
	if expect == "" {
		expect = "success"
	}
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("读实机 spec: %v", err)
	}
	var spec JobSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatalf("解析实机 spec: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	res, err := NewStarlarkEngine().Execute(ctx, &spec)
	if err != nil {
		t.Fatalf("引擎执行失败: %v", err)
	}
	t.Logf("实机脚本执行结果：status=%q error=%q", res.Status, res.Error)
	if res.Status != expect {
		t.Fatalf("预期 status=%q，got status=%q error=%q", expect, res.Status, res.Error)
	}
}
