package cron

// hex-test 审计 · R2：cron 幂等键改名（Name 追加 ·agentName + 引入 SourceKey）击穿迁移兜底。
// 存量旧格式 job（SourceKey="", Name 裸名）在升级后 req.Name 带后缀 → existing.Name==req.Name
// 永不成立 → 兜底失效 → 老 job 成孤儿、新 job 并存 → 升级后双投递（家庭群重复轰炸）。
// RED：迁移场景不匹配 → FAIL；GREEN：补「存量旧格式 + 新名以裸名+分隔符开头」迁移分支。

import "testing"

func TestJobMatchesIdempotencyKey_MigratesLegacyBareName_R2(t *testing.T) {
	// 核心迁移：存量旧格式（SourceKey="", 裸名）↔ 新格式（SourceKey!="", 裸名·agentName）
	if !jobMatchesIdempotencyKey("", "错题卷（每周五）", "张三/错题卷", "错题卷（每周五）·张三") {
		t.Fatal("R2: 迁移应匹配存量裸名 job（否则升级后老新并存双投递）")
	}
	// 回归：同 SourceKey 匹配
	if !jobMatchesIdempotencyKey("k1", "any", "k1", "other") {
		t.Fatal("同 SourceKey 应匹配")
	}
	// 回归：旧格式同名匹配（迁移前的原兜底）
	if !jobMatchesIdempotencyKey("", "日报", "", "日报") {
		t.Fatal("旧格式同名应匹配")
	}
	// 边界：无分隔符的前缀不误配（AP-158 教训）——「错题卷」不应吞「错题卷精选·张三」
	if jobMatchesIdempotencyKey("", "错题卷", "k", "错题卷精选·张三") {
		t.Fatal("无分隔符边界的前缀不应误配")
	}
	// 安全：已被其他非空 SourceKey 拥有的 job 不被展示名偷走
	if jobMatchesIdempotencyKey("otherkey", "错题卷（每周五）", "张三/错题卷", "错题卷（每周五）·张三") {
		t.Fatal("已被其他 SourceKey 拥有的 job 不应被展示名偷走")
	}
}
