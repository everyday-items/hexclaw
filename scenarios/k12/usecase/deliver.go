package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 自动化沉淀（PRD §3.6）的**投递内容层**：把 generator 结构化产出渲染成一段可直接
// 投递到 IM 群 / 桌面会话的文本。核心纪律：**无内容静默跳过**（skip=true）——不发空
// 内容打扰家长（§3.6.3）。调度与投递复用平台 cron（Starlark 抓端点 → emit → Deliverer），
// 本层只负责"生成什么文本、要不要发"，与 cron/adapter 解耦（AP-1）。

// DailyReminderText 生成「每日复习提醒」（§3.6.4-2）：待复习错题数 >0 时推一句话
// （含数量与最薄弱知识点）；=0 时 skip=true 静默跳过。
func DailyReminderText(items []ReviewItem) (text string, skip bool) {
	if len(items) == 0 {
		return "", true
	}
	// 最薄弱知识点 = 到期队列里出现次数最多的知识点（并列取先到期者，队列已按到期升序）。
	weakest := mostFrequentKP(items)
	if weakest != "" {
		return fmt.Sprintf("今天有 %d 道错题待复习，最薄弱的是「%s」，抽空陪孩子过一遍。", len(items), weakest), false
	}
	return fmt.Sprintf("今天有 %d 道错题待复习，抽空陪孩子过一遍。", len(items)), false
}

// RenderReportMarkdown 把学情报告渲染成四段式 Markdown（§3.6.4-3：总览/强弱项/复习完成/下月建议）。
// 无任何错题记录（Trend.Total==0）时 skip=true（不发空报告）。
// 标题口径（§3.11 命名定案，与前端 K12InsightPanel titleWithGrade 一致）：
// 「{年级}学习概览」，gradeTerm 取 Learner profile 的 grade_term；空 → 通用「学习概览」。
func RenderReportMarkdown(rep InsightReport, gradeTerm string) (md string, skip bool) {
	if rep.Trend.Total == 0 {
		return "", true
	}
	var b strings.Builder
	b.WriteString("# " + gradeTerm + "学习概览\n\n")

	// ① 总览（进步趋势）
	b.WriteString("## 总览\n\n")
	trend := "→"
	if rep.Trend.Mastered > rep.Trend.Reviewing {
		trend = "↑"
	}
	fmt.Fprintf(&b, "已掌握 %d · 待复习 %d · 已重做 %d · 趋势 %s\n\n",
		rep.Trend.Mastered, rep.Trend.Reviewing, rep.Trend.Retried, trend)
	fmt.Fprintf(&b, "本月新增错题 %d 道。\n\n", rep.MonthNewMistakes)

	// ② 强弱项
	b.WriteString("## 强弱项\n\n")
	if len(rep.WeakTop3) == 0 {
		b.WriteString("本月没有新增薄弱知识点，保持得不错。\n\n")
	} else {
		b.WriteString("本月薄弱知识点（按错题数）：\n\n")
		for _, w := range rep.WeakTop3 {
			fmt.Fprintf(&b, "- %s（%d 道）\n", w.KnowledgePoint, w.Count)
		}
		b.WriteString("\n")
	}
	if len(rep.ConsecutiveFailKPs) > 0 {
		// §3.6.4-3 连续挫败提示（文案守 §5.4.3 不制造焦虑）。
		fmt.Fprintf(&b, "「%s」连续几次没过——建议先降低难度或回顾前置知识点，别急，慢慢来。\n\n",
			rep.ConsecutiveFailKPs[0])
	}

	// ③ 当前复习周完成
	b.WriteString("## 复习完成\n\n")
	if rep.ReviewCompletionRate < 0 {
		b.WriteString("本周还没有已固化的练习卷。\n\n")
	} else {
		fmt.Fprintf(&b, "本周复习完成率 %.0f%%。\n\n", rep.ReviewCompletionRate*100)
	}

	// ④ 下月建议
	b.WriteString("## 下月建议\n\n")
	b.WriteString(rep.Suggestion)
	b.WriteString("\n")
	return b.String(), false
}

// SemesterCheckText 生成学期确认提醒（§3.13 学期确认，3.1/9.1 触发）。
// 按当前档案推算下一学期，请家长确认（**绝不自动推进档案**）。
// 档案无年级 / 已是六年级下（小学封顶，冻结#2：不产生升初中建议）→ skip=true。
func SemesterCheckText(p k12.ChildProfile) (text string, next string, skip bool) {
	nextTerm, ok := k12.NextGradeTerm(p.GradeTerm)
	if !ok {
		return "", "", true
	}
	name := p.ChildName
	if name == "" {
		name = "孩子"
	}
	return fmt.Sprintf(
		"%s现在是%s了吗？回复 是 我就把讲题范围更新到%s；如果是别的年级学期，直接回复该年级。",
		name, nextTerm, nextTerm), nextTerm, false
}

// mostFrequentKP 返回复习队列里出现最多的知识点（空知识点忽略；无则空串）。
func mostFrequentKP(items []ReviewItem) string {
	count := map[string]int{}
	best, bestN := "", 0
	for _, it := range items {
		kp := it.Fields.KnowledgePoint
		if kp == "" {
			continue
		}
		count[kp]++
		if count[kp] > bestN {
			best, bestN = kp, count[kp]
		}
	}
	return best
}

// constraintFor 从 ConstraintProvider 取 grade 的已学方法白名单，拼成注入 solver 的约束串。
// 无 provider / grade 未命中课标 → 空串（solve 侧退化为"仅 grade 阶段提示、不列白名单"，fail-open 可见）。
func (d Deps) constraintFor(ctx context.Context, grade string) string {
	if d.Constraint == nil || grade == "" {
		return ""
	}
	allowed, err := d.Constraint.Allowed(ctx, grade)
	if err != nil || len(allowed) == 0 {
		return ""
	}
	return strings.Join(allowed, "、")
}

// --- 投递入口（Deps 方法，供 HTTP 投递端点调用）---

// DailyReminder 取复习队列 → 一句话提醒（skip=无待复习）。
//
// v0.5.0 冻结（架构设计-v0.5.0 §3.7 明令：「未掌握题不因长时间未练习自动隐藏；
// 只按学期策略或家长显式操作归档」）：原 30 天静默归档（ArchiveStaleMistakes）与
// PRD 冲突，已整体删除；归档只剩 学期策略 / 家长手动 / 证据已掌握（mastered）三条路径。
func (d Deps) DailyReminder(ctx context.Context, agentName string) (text string, skip bool, err error) {
	items, err := d.ReviewQueue(ctx, agentName)
	if err != nil {
		return "", false, err
	}
	text, skip = DailyReminderText(items)
	return text, skip, nil
}

// MonthlyReportMarkdown 聚合学情 → 四段式 Markdown（skip=无错题记录）。
// §3.13（2026-07-18 裁决）不含月报工作流：内容端点保留供家长手动查看，不再默认注册 cron。
func (d Deps) MonthlyReportMarkdown(ctx context.Context, agentName string) (md string, skip bool, err error) {
	rep, err := d.InsightReport(ctx, agentName)
	if err != nil {
		return "", false, err
	}
	// 标题年级取 Learner profile（未建档/无年级 → 通用「学习概览」，不阻断报告）。
	grade := ""
	if d.Profiles != nil {
		if p, perr := d.GetProfile(ctx, agentName); perr == nil {
			grade = p.GradeTerm
		}
	}
	md, skip = RenderReportMarkdown(rep, grade)
	return md, skip, nil
}

// YearArchiveText 学年归档建议文案（学年 6 月底备份建议）。
// §3.13（2026-07-18 裁决）不含学年归档工作流：内容端点保留，不再默认注册 cron。
// 注意这只是"导出备份"建议文案，不改任何错题状态（§3.7：归档只走学期策略/家长手动/mastered）。
// 无任何记录 → skip（新账号不打扰）。
func YearArchiveText(name string, recordCount int) (text string, skip bool) {
	if recordCount <= 0 {
		return "", true
	}
	if name == "" {
		name = "孩子"
	}
	return fmt.Sprintf(
		"这一学年%s累计了 %d 条错题记录。学年结束了，建议现在导出一次「家庭学习档案」备份留存（设置→备份与恢复），换电脑/重装不丢这一年的积累。",
		name, recordCount), false
}

// YearArchiveSuggestion 取记录总数 → 归档建议（skip=无记录）。
func (d Deps) YearArchiveSuggestion(ctx context.Context, agentName string) (text string, skip bool, err error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, "")
	if err != nil {
		return "", false, err
	}
	name := ""
	if d.Profiles != nil {
		if p, perr := d.GetProfile(ctx, agentName); perr == nil {
			name = p.ChildName
		}
	}
	text, skip = YearArchiveText(name, len(recs))
	return text, skip, nil
}
