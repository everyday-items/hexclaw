package usecase

import (
	"fmt"
	"net/url"
	"strings"
)

// 自动化沉淀的**调度描述符层**（PRD §3.6.2）。K12 不 import cron（AP-1）：这里只产出
// 纯声明式描述符（名字/schedule/Starlark 脚本/投递目标），由 composition root 翻译成
// 平台 cron.AddJobRequest 注册。脚本走平台惯用法——http_get 抓 K12 投递端点 → 有内容
// 才 emit（无内容 emit 空 status 让调度器静默跳过，§3.6.3）。
//
// 复用平台 cron 的红利：任务页可见启停/上次执行（§3.6.6）、失败进既有反馈、per-platform
// 限速、投递多路（IM + 桌面）全部现成，K12 零重复造。

// CronSpec 一个默认自动化任务的声明式描述符。
type CronSpec struct {
	Name     string   // 任务名（对家长可见）
	Schedule string   // 标准 5 段 cron 表达式
	Runtime  string   // 执行运行时；空 → 平台默认 starlark
	Script   string   // 脚本体
	Deliver  []string // 投递目标（"chat" 桌面 / "feishu"|"dingtalk"|… IM）
}

// K12CronKind 标识默认任务类别（供 composition/前端做启停与去重）。
type K12CronKind string

const (
	KindWeeklySheet    K12CronKind = "weekly-sheet"    // 周五错题卷
	KindDailyReminder  K12CronKind = "daily-reminder"  // 每日复习提醒
	KindMonthlyReport  K12CronKind = "monthly-report"  // 月度学情报告
	KindYearEndArchive K12CronKind = "year-archive"    // 学年 6 月底归档建议
	KindSemesterSpring K12CronKind = "semester-spring" // 3/1 学期确认
	KindSemesterFall   K12CronKind = "semester-fall"   // 9/1 学期确认
)

// deliverScript 生成"抓端点→有内容才投递"的 Starlark 脚本。
// endpoint 返回空 body = 本期无内容（§3.6.3 静默跳过）；非 2xx = 标记失败进任务反馈。
func deliverScript(url string) string {
	// 单引号在 Starlark 里是合法字符串定界符；url 由本函数拼装（agent 名已 URL 编码），不含引号注入面。
	return fmt.Sprintf(`# K12 自动化沉淀投递（平台 cron · Starlark · 零 LLM）
def run():
    resp = http_get('%s')
    if resp["status"] < 200 or resp["status"] >= 300:
        return {"status": "error", "error": "K12 投递端点非 2xx: %%d" %% resp["status"]}
    body = resp["body"]
    if not body:
        # 本期无内容（无到期错题 / 无记录 / 无从推进学期）——静默跳过，不打扰。
        return {"status": "success"}
    return {"status": "success", "data": {"message": body}}

emit(run())
`, url)
}

// DefaultCronSpecs 返回某辅导实例的默认自动化任务集（PRD §3.6.2 "随模板创建默认注册"）。
//
//	baseURL   本机 API 基址（如 http://127.0.0.1:8787），composition root 注入。
//	agentName 实例名（错题本/档案 scope 键）。
//	deliver   投递目标（如 ["dingtalk"] 或 ["chat"]）；空 → 平台默认桌面 chat。
//
// schedule 口径对齐 §3.6.2：周五 19:00 卷 / 每日 20:00 提醒 / 每月 1 日 09:00 报告 /
// 3.1 与 9.1 09:00 学期确认。
func DefaultCronSpecs(baseURL, agentName string, deliver []string) []CronSpec {
	base := strings.TrimRight(baseURL, "/")
	q := "?agent=" + url.QueryEscape(agentName)
	ep := func(path string) string { return base + "/api/k12/cron/" + path + q }

	mk := func(kind K12CronKind, name, schedule, path string) CronSpec {
		return CronSpec{
			Name:     name,
			Schedule: schedule,
			Runtime:  "", // 平台默认 starlark
			Script:   deliverScript(ep(path)),
			Deliver:  deliver,
		}
	}
	return []CronSpec{
		mk(KindWeeklySheet, "错题卷（每周五）", "0 19 * * 5", "mistake-sheet"),
		mk(KindDailyReminder, "复习提醒（每天）", "0 20 * * *", "daily-reminder"),
		mk(KindMonthlyReport, "学情报告（每月）", "0 9 1 * *", "monthly-report"),
		mk(KindYearEndArchive, "学年归档建议（6/25）", "0 9 25 6 *", "year-archive"),
		mk(KindSemesterSpring, "学期确认（3/1）", "0 9 1 3 *", "semester-check"),
		mk(KindSemesterFall, "学期确认（9/1）", "0 9 1 9 *", "semester-check"),
	}
}
