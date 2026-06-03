// bug_deliver_roundtrip_test 守卫 D4.2 多 deliver 桥接：
//   - AddJobRequest.Deliver 透传到 Job.Deliver
//   - meta JSON 列正确序列化 / 反序列化
//   - EffectiveDeliver 在空 deliver 时回退 ["chat"]
package cron

import (
	"strings"
	"testing"
)

func TestEffectiveDeliver_DefaultChat(t *testing.T) {
	j := &Job{}
	got := EffectiveDeliver(j)
	if len(got) != 1 || got[0] != "chat" {
		t.Errorf("空 deliver 应回退 [chat]，实际 %v", got)
	}
}

func TestEffectiveDeliver_NilSafe(t *testing.T) {
	got := EffectiveDeliver(nil)
	if len(got) != 1 || got[0] != "chat" {
		t.Errorf("nil job 应回退 [chat]，实际 %v", got)
	}
}

func TestEffectiveDeliver_MultiChannel(t *testing.T) {
	j := &Job{Deliver: []string{"chat", "feishu", "wechat"}}
	got := EffectiveDeliver(j)
	if len(got) != 3 {
		t.Fatalf("应保留全部 3 渠道，实际 %d", len(got))
	}
}

func TestSerializeJobMeta_RoundTrip(t *testing.T) {
	in := &Job{Deliver: []string{"chat", "discord"}}
	metaJSON := serializeJobMeta(in)
	if !strings.Contains(metaJSON, "discord") {
		t.Errorf("meta JSON 缺 discord，实际 %s", metaJSON)
	}

	out := &Job{}
	parseJobMeta(out, metaJSON)
	if len(out.Deliver) != 2 || out.Deliver[1] != "discord" {
		t.Errorf("反序列化 deliver 失败，实际 %v", out.Deliver)
	}
}

func TestSerializeJobMeta_EmptyDeliverIsCompact(t *testing.T) {
	in := &Job{}
	metaJSON := serializeJobMeta(in)
	if metaJSON != "{}" {
		t.Errorf("空 deliver 应序列化为 {}，实际 %q", metaJSON)
	}
}

func TestParseJobMeta_NilJobSafe(t *testing.T) {
	// 不应 panic
	parseJobMeta(nil, `{"deliver":["chat"]}`)
}

func TestParseJobMeta_EmptyStringSafe(t *testing.T) {
	j := &Job{}
	parseJobMeta(j, "")
	if len(j.Deliver) != 0 {
		t.Error("空 meta 字符串不应改写 job.Deliver")
	}
}

func TestParseJobMeta_MalformedSafe(t *testing.T) {
	j := &Job{Deliver: []string{"chat"}}
	parseJobMeta(j, `not json`)
	// 不应破坏原 Deliver（保守降级）
	if len(j.Deliver) != 1 {
		t.Errorf("malformed meta 不应改写既有 deliver，实际 %v", j.Deliver)
	}
}
