package eval

import "github.com/hexagon-codes/hexclaw/memory/recall"

// Scenarios 返回 hexclaw 记忆召回评测集（方案 §G1）。
//
// 覆盖 LongMemEval 6 类（单会话 用户/助手/偏好 召回 + 知识更新 + 时序推理 + 多会话）
// 与 LoCoMo（不串场 + 误召防护）。全部跑在降级纯 BM25 路径（hexclaw 桌面默认）。
func Scenarios() []Scenario {
	now := EvalClock()
	yesterday := now.AddDate(0, 0, -1)
	nextMonth := now.AddDate(0, 0, 30)

	return []Scenario{
		{
			Name: "单会话-用户信息召回", Class: "longmemeval/single-session-user",
			UserID: "u1", Query: "用户的名字是什么", MinScore: 0.03,
			Seed: []recall.Entry{
				{ID: "name", UserID: "u1", Type: recall.TypeIdentity, Content: "用户的名字叫小明", AccessedAt: now},
				{ID: "weather", UserID: "u1", Type: recall.TypeFact, Content: "今天聊了聊天气和股市", AccessedAt: now},
			},
			WantIDs: []string{"name"}, MustNotIDs: []string{"weather"},
		},
		{
			Name: "单会话-偏好召回", Class: "longmemeval/single-session-preference",
			UserID: "u1", Query: "界面主题偏好", MinScore: 0.03,
			Seed: []recall.Entry{
				{ID: "theme", UserID: "u1", Type: recall.TypePreference, Content: "用户喜欢深色主题界面", AccessedAt: now},
				{ID: "noise", UserID: "u1", Type: recall.TypeFact, Content: "随便闲聊了几句", AccessedAt: now},
			},
			WantIDs: []string{"theme"}, MustNotIDs: []string{"noise"},
		},
		{
			Name: "单会话-助手承诺召回", Class: "longmemeval/single-session-assistant",
			UserID: "u1", Query: "助手承诺了什么", MinScore: 0.03,
			Seed: []recall.Entry{
				{ID: "promise", UserID: "u1", Type: recall.TypeInstruction, Content: "助手承诺每次回复都附上代码示例", AccessedAt: now},
				{ID: "noise", UserID: "u1", Type: recall.TypeFact, Content: "用户问了一个数学题", AccessedAt: now},
			},
			WantIDs: []string{"promise"}, MustNotIDs: []string{"noise"},
		},
		{
			Name: "知识更新-时序取代", Class: "longmemeval/knowledge-update",
			UserID: "u1", Query: "用户在哪里工作", MinScore: 0.03,
			Seed: []recall.Entry{
				{ID: "old_job", UserID: "u1", Type: recall.TypeFact, Subject: "工作地", Content: "用户在北京工作", AccessedAt: yesterday, ValidTo: &yesterday},
				{ID: "new_job", UserID: "u1", Type: recall.TypeFact, Subject: "工作地", Content: "用户现在在上海工作", AccessedAt: now, ValidFrom: yesterday, Supersedes: "old_job"},
			},
			WantIDs: []string{"new_job"}, MustNotIDs: []string{"old_job"},
		},
		{
			Name: "时序推理-未来生效", Class: "longmemeval/temporal-reasoning",
			UserID: "u1", Query: "用户现在住哪", MinScore: 0.03,
			Seed: []recall.Entry{
				{ID: "current", UserID: "u1", Type: recall.TypeFact, Subject: "居住地", Content: "用户目前住在广州", AccessedAt: now},
				{ID: "future", UserID: "u1", Type: recall.TypeFact, Subject: "居住地", Content: "用户下月起搬到深圳", AccessedAt: now, ValidFrom: nextMonth},
			},
			WantIDs: []string{"current"}, MustNotIDs: []string{"future"},
		},
		{
			Name: "多会话-相关挑选", Class: "longmemeval/multi-session",
			UserID: "u1", Query: "项目用什么技术栈", MinScore: 0.03,
			Seed: []recall.Entry{
				{ID: "stack", UserID: "u1", Type: recall.TypeContext, Content: "用户的项目用 Go 和 Vue 技术栈", AccessedAt: yesterday},
				{ID: "hobby", UserID: "u1", Type: recall.TypeFact, Content: "用户周末喜欢爬山", AccessedAt: now},
			},
			WantIDs: []string{"stack"}, MustNotIDs: []string{"hobby"},
		},
		{
			Name: "不串场-多租户隔离", Class: "locomo/multi-tenant",
			UserID: "u1", Query: "项目代号是什么", MinScore: 0.03,
			Seed: []recall.Entry{
				{ID: "falcon", UserID: "u1", Type: recall.TypeContext, Content: "内部项目代号 Falcon", AccessedAt: now},
				{ID: "eagle", UserID: "u2", Type: recall.TypeContext, Content: "内部项目代号 Eagle", AccessedAt: now},
			},
			WantIDs: []string{"falcon"}, MustNotIDs: []string{"eagle"},
		},
		{
			Name: "query改写-同义扩展救漏召", Class: "recall/query-rewrite",
			UserID: "u1", Query: "主题偏好", MinScore: 0.03,
			Expander: recall.SynonymExpander{Synonyms: recall.DefaultSynonyms()},
			Seed: []recall.Entry{
				// 记忆措辞「外观」与 query「主题」不一致：无扩展则漏召；同义扩展后命中。
				{ID: "theme", UserID: "u1", Type: recall.TypePreference, Content: "用户喜欢深色外观界面", AccessedAt: now},
				{ID: "noise", UserID: "u1", Type: recall.TypeFact, Content: "用户养了一只橘猫", AccessedAt: now},
			},
			WantIDs: []string{"theme"}, MustNotIDs: []string{"noise"},
		},
		{
			Name: "误召防护-噪声砍除", Class: "locomo/noise-rejection",
			UserID: "u1", Query: "生产用什么数据库", MinScore: 0.03,
			Seed: []recall.Entry{
				{ID: "db", UserID: "u1", Type: recall.TypeFact, Content: "用户生产环境的数据库是 PostgreSQL", AccessedAt: now},
				{ID: "n1", UserID: "u1", Type: recall.TypeFact, Content: "昨天看了场电影", AccessedAt: now},
				{ID: "n2", UserID: "u1", Type: recall.TypeFact, Content: "喝了杯咖啡", AccessedAt: now},
			},
			WantIDs: []string{"db"}, MustNotIDs: []string{"n1", "n2"},
		},
	}
}
