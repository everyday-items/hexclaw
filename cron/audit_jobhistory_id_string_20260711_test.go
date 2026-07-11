package cron

// hex-test 审计 · 契约#7：CronJobRun.id 前端 string ↔ 后端 int64，wire 是 number。
// 前端 tasks.ts:380 interface CronJobRun { id: string }，后端 JobHistory.ID int64 json:"id"
// 序列化为裸 number → 运行时 number、TS 误判 string，.startsWith/编码等字符串操作出错。
// RED：ID 序列化为 number → FAIL；GREEN：json:"id,string" 序列化为 string 与前端契约对齐。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJobHistory_IDSerializesAsString_Contract7(t *testing.T) {
	b, err := json.Marshal(JobHistory{ID: 123456789})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"id":"123456789"`) {
		t.Fatalf("JobHistory.ID 应序列化为 JSON string(前端 CronJobRun.id: string 契约),实际 %s", b)
	}
}
