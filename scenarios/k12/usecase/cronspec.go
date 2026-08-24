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
	Key      string      // 稳定幂等键：agentName/kind（不依赖可变展示名）
	Kind     K12CronKind // 任务类别（描述符自带，注册方以此为 kind——单一事实源，防错位）
	Name     string      // 任务名（对家长可见）
	Schedule string      // 标准 5 段 cron 表达式
	Runtime  string      // 执行运行时；空 → 平台默认 starlark
	Script   string      // 脚本体
	Deliver  []string    // 投递目标（"chat" 桌面 / "feishu"|"dingtalk"|… IM）
}

// K12CronKind 标识默认任务类别（供 composition/前端做启停与去重）。
//
// 类别集对齐架构设计-v0.5.0 §3.13（2026-07-18 裁决）四类工作流：每周复习 / 回传提醒 /
// 学期确认 / 学情刷新（事件触发，不进 cron）。原 monthly-report / year-archive 是 §3.13
// 之外的超集，描述符已撤下（内容端点保留，家长可手动查看）。
type K12CronKind string

const (
	KindWeeklySheet    K12CronKind = "weekly-sheet"    // §3.13 每周复习：周五错题卷
	KindReturnReminder K12CronKind = "return-reminder" // §3.13 回传提醒：卷子固化后 T+1 未回传提醒
	KindSemesterSpring K12CronKind = "semester-spring" // §3.13 学期确认：3/1
	KindSemesterFall   K12CronKind = "semester-fall"   // §3.13 学期确认：9/1
)

// 2026-07-18 数字口径对齐（账本唯一化裁决）：cron 注册数以 §3.13 工作流表为唯一口径——
// 每周复习 1 + 回传提醒 1 + 学期确认 2（3/1、9/1）= 4 个 job；学情刷新是事件触发不进 cron。
// daily-reminder 不在 §3.13 工作流表内（与 monthly-report / year-archive 同为历史超集），
// 描述符已撤下；内容端点 /cron/daily-reminder 保留供手动查看。

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

// DefaultCronSpecs 返回某辅导实例的默认自动化任务集（建档完成时自动初始化，§3.13
// 2026-07-18 裁决："任务按 Learner 实例注册，没有实例就没有任务"）。
//
//	baseURL   本机 API 基址（如 http://127.0.0.1:8787），composition root 注入。
//	agentName 实例名（错题本/档案 scope 键）。
//	deliver   投递目标（如 ["dingtalk"] 或 ["chat"]）；空 → 平台默认桌面 chat。
//
// 任务集对齐 §3.13：每周复习（周五 19:00 卷）/ 每日复习提醒（20:00 轻提醒）/
// 回传提醒（T+1 20:00）/ 学期确认（3.1、9.1 09:00）。学情刷新是事件触发不进 cron；
// monthly-report / year-archive 为 §3.13 外超集，已撤下描述符（内容端点保留）。
func DefaultCronSpecs(baseURL, agentName string, deliver []string) []CronSpec {
	base := strings.TrimRight(baseURL, "/")
	q := "?agent=" + url.QueryEscape(agentName)
	ep := func(path string) string { return base + "/api/k12/cron/" + path + q }

	mk := func(kind K12CronKind, name, schedule, path string) CronSpec {
		return CronSpec{
			Key:  string(agentName) + "/" + string(kind),
			Kind: kind,
			// BUG-20260710-H3：Name 带 agent 作用域——它是 Register 幂等键
			// （agent+kind）的载体：同名覆盖/跳过、多孩互不挤占；对家长也可分辨
			// 是哪个孩子的任务。
			Name:     name + "·" + agentName,
			Schedule: schedule,
			Runtime:  "", // 平台默认 starlark
			Script:   deliverScript(ep(path)),
			Deliver:  deliver,
		}
	}
	// §3.13 回传提醒：卷子固化（打印/发送）后 T+1 20:00，未回传才提醒（已回传不发，
	// 每卷最多一次）。"T+1 事件"语义由内容端点按 finalized_at=昨日 筛选实现，cron 只是
	// 每日 20:00 的扫描节拍。内容端点 /api/k12/cron/return-reminder 已实现
	// （usecase/return_reminder.go，契约见 return_reminder_test.go）。
	rr := mk(KindReturnReminder, "回传提醒（每天）", "0 20 * * *", "return-reminder")
	rr.Script = returnReminderScript(ep("return-reminder"))

	// 每周复习只整理并投递到期错题；加入练习集只由家长逐题显式触发。
	ws := mk(KindWeeklySheet, "错题卷（每周五）", "0 19 * * 5", "mistake-sheet")

	return []CronSpec{
		ws,
		rr,
		// §3.13 学期确认：3/1、9/1 询问是否切换学期，只在家长确认后更新（六年级下封顶，冻结#2）。
		mk(KindSemesterSpring, "学期确认（3/1）", "0 9 1 3 *", "semester-check"),
		mk(KindSemesterFall, "学期确认（9/1）", "0 9 1 9 *", "semester-check"),
	}
}

// returnReminderScript 同 deliverScript，但把 404 当"本期无内容"静默跳过。
// 端点已实现（GET /cron/return-reminder），404 分支仅防回滚：旧二进制/端点被裁时
// 不产生每日误报，端点恢复后自动生效。
func returnReminderScript(url string) string {
	return fmt.Sprintf(`# K12 回传提醒投递（§3.13 · 平台 cron · Starlark · 零 LLM）
def run():
    resp = http_get('%s')
    if resp["status"] == 404:
        # 端点已实现；404 分支仅防回滚——视同本期无内容，静默跳过。
        return {"status": "success"}
    if resp["status"] < 200 or resp["status"] >= 300:
        return {"status": "error", "error": "K12 投递端点非 2xx: %%d" %% resp["status"]}
    body = resp["body"]
    if not body:
        # 昨日固化的卷都已回传/无固化卷——静默跳过，不打扰（每卷最多提醒一次）。
        return {"status": "success"}
    return {"status": "success", "data": {"message": body}}

emit(run())
`, url)
}
