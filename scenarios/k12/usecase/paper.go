package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// PracticePaperView 练习卷渲染结果（§4.13 呈现物）。
type PracticePaperView struct {
	Kind     string // question | answer
	Title    string // 卷面标题（未截断原值；截断只发生在渲染的页眉）
	PaperNo  string // 卷面号；预览为空（固化才分配）
	Markdown string // 卷面 Markdown（§4.13 版面）
	Preview  bool   // 固化前预览（draft）：与正卷同渲染器同口径，无卷面号
}

// RenderPracticePaper 渲染题目卷/答案卷（2026-07-18 呈现物真实渲染批）：
//   - 固化后（有卷面号）为正卷：日期 = 固化日，页眉/页脚印卷面号；
//   - draft 为预览：走同一渲染器（预览口径 = 固化产物口径），临时编号不落库、无卷面号；
//   - 没有已验证题目时拒绝（与固化门同口径，家长向文案）；
//   - 学期取自孩子档案（可空不阻塞）。
func (d Deps) RenderPracticePaper(ctx context.Context, agentName, recordID, kind string) (PracticePaperView, error) {
	switch kind {
	case "", k12.PaperKindQuestion:
		kind = k12.PaperKindQuestion
	case k12.PaperKindAnswer:
	default:
		return PracticePaperView{}, fmt.Errorf("%w: 卷面种类非法 %q（question/answer）", ErrInvalidInput, kind)
	}
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return PracticePaperView{}, err
	}
	if v.Record.Status == k12.PracticeStatusCancelled {
		return PracticePaperView{}, fmt.Errorf("usecase: 已取消的练习集没有可打印的卷")
	}
	if !k12.PracticeSetPublishable(v.Fields) {
		// §4.11 家长向术语：不出现「验证器/质量门/篮子」。
		return PracticePaperView{}, fmt.Errorf("%w: 还没有已验证的题，暂时出不了卷", ErrInvalidInput)
	}
	preview := v.Fields.PaperNo == ""
	date := time.Unix(d.now(), 0)
	if !preview && v.Fields.FinalizedAt > 0 {
		date = time.Unix(v.Fields.FinalizedAt, 0)
	}
	term := ""
	if d.Profiles != nil {
		if p, perr := d.GetProfile(ctx, agentName); perr == nil {
			term = p.GradeTerm
		}
	}
	md := k12.RenderPaperMarkdown(v.Fields, kind, k12.PaperMeta{Term: term, Date: date, Preview: preview})
	return PracticePaperView{
		Kind: kind, Title: v.Fields.Title, PaperNo: v.Fields.PaperNo,
		Markdown: md, Preview: preview,
	}, nil
}
