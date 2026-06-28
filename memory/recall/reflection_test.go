package recall

import (
	"testing"
	"time"
)

func hasOp(ops []Op, a OpAction, target string) bool {
	for _, o := range ops {
		if o.Action == a && o.TargetID == target {
			return true
		}
	}
	return false
}

func TestReflect_MechanicalIntegration(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{ID: "keep", Type: TypeFact, Content: "用户喜欢深色主题", RecallCount: 1, AccessedAt: now},
		{ID: "dup", Type: TypeFact, Content: "用户喜欢深色主题界面", RecallCount: 0, AccessedAt: now},
		{ID: "oldjob", Type: TypeFact, Subject: "工作地", Content: "用户在北京工作", CreatedAt: now.AddDate(0, 0, -10), AccessedAt: now},
		{ID: "newjob", Type: TypeFact, Subject: "工作地", Content: "用户在上海工作", CreatedAt: now, AccessedAt: now},
		{ID: "hotfact", Type: TypeFact, Content: "经常被召回的事实", RecallCount: 4, AccessedAt: now},
		{ID: "stale", Type: TypeFact, Content: "陈旧无关事实", AccessedAt: now.AddDate(0, 0, -200)},
	}
	ops := Reflect(entries, now, DefaultLifecyclePolicy(), DedupOptions{SimFn: LexicalSim, JaccardThreshold: 0.6})

	if !hasOp(ops, OpDedup, "dup") {
		t.Errorf("近重复 dup 应被去重（保留 keep）: %+v", ops)
	}
	if !hasOp(ops, OpSupersede, "oldjob") {
		t.Errorf("同主语旧条 oldjob 应被时序取代: %+v", ops)
	}
	if !hasOp(ops, OpPromote, "hotfact") {
		t.Errorf("高频 hotfact 应晋升: %+v", ops)
	}
	if !hasOp(ops, OpArchive, "stale") {
		t.Errorf("陈旧 stale 应归档: %+v", ops)
	}

	// 机械化硬约束（修正2）：每个 op 只引用真实条目 ID，绝不凭空造内容。
	valid := map[string]bool{"keep": true, "dup": true, "oldjob": true, "newjob": true, "hotfact": true, "stale": true}
	for _, o := range ops {
		if !valid[o.TargetID] {
			t.Errorf("op 引用了不存在的条目 %q（疑似凭空造）", o.TargetID)
		}
		if o.OtherID != "" && !valid[o.OtherID] {
			t.Errorf("op 的 OtherID 引用了不存在的条目 %q", o.OtherID)
		}
	}
}
