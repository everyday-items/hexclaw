package api

import "testing"

func TestLogCollector_QueryWithDomain(t *testing.T) {
	c := NewLogCollector(16)
	c.Info("chat", "chat message")
	c.Info("telegram", "integration message")
	c.Info("cron", "automation message")

	entries, total := c.QueryWithDomain("", "", "integration", "", 10, 0)
	if total != 1 || len(entries) != 1 {
		t.Fatalf("期望 integration 命中 1 条，实际 total=%d len=%d", total, len(entries))
	}
	if entries[0].Domain != "integration" || entries[0].Source != "telegram" {
		t.Fatalf("domain/source 不正确: %+v", entries[0])
	}
}
