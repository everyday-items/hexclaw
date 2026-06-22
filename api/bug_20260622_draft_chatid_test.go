package api

import (
	"encoding/json"
	"testing"
)

// AUD-1 回归锁（2026-06-22 R3）：统一 cron 创建载荷的 draft.chat_id 必须解析到
// CronJobDraft.ChatID，cronActionCreate 再透传到 AddJobRequest.ChatID → job.ChatID。
// 桌面端 createCronJobJSON 对 IM/连接投递目标会带 chat_id；后端必须收得到。
func TestCronJobDraft_ParsesChatID(t *testing.T) {
	body := `{"name":"日报","schedule":"0 9 * * *","prompt":"汇总","deliver":["feishu"],"chat_id":"oc_test_123"}`
	var d CronJobDraft
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("unmarshal draft 失败: %v", err)
	}
	if d.ChatID != "oc_test_123" {
		t.Fatalf("draft.chat_id 未解析到 ChatID：want oc_test_123, got %q", d.ChatID)
	}
}
