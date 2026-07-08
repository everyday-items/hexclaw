package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// MistakeSheetMarkdown 生成「错题卷」：把到期该练的错题排成可打印卷子（只出题、不给答案，留重做空间）。
//
// 这是 M2-2 周五错题卷 / 「一键出错题卷」的产物内容；由 cron 投递或前端打印（§3.5.4）。
func (d Deps) MistakeSheetMarkdown(ctx context.Context, agentName string) (string, error) {
	items, err := d.ReviewQueue(ctx, agentName)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 本周错题卷\n\n共 %d 题，做完对照错题本订正。\n\n", len(items))
	for i, it := range items {
		fmt.Fprintf(&b, "**%d.** %s\n\n（　　）\n\n", i+1, it.Fields.Question)
	}
	if len(items) == 0 {
		b.WriteString("本周没有到期该练的错题，继续保持。\n")
	}
	return b.String(), nil
}

// ExportMistakesMarkdown 把某实例的错题本导出为 Markdown 文本。
//
// 只产内容（结构化 Markdown）；PDF/Word 渲染由平台 render 服务承接（§3.8：PDF 复用错题卷版式 / Word 供老师批注）。
func (d Deps) ExportMistakesMarkdown(ctx context.Context, agentName string) (string, error) {
	if agentName == "" {
		return "", fmt.Errorf("usecase: agentName 不可空")
	}
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, "")
	if err != nil {
		return "", fmt.Errorf("usecase: 导出错题本: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 错题本导出\n\n共 %d 题\n\n", len(recs))
	for i, r := range recs {
		f, _ := k12.ParseMistakeFields(r.Fields)
		fmt.Fprintf(&b, "## %d. %s\n\n", i+1, f.Question)
		if f.KnowledgePoint != "" {
			fmt.Fprintf(&b, "- 知识点：%s\n", f.KnowledgePoint)
		}
		if f.ErrorCause != "" {
			fmt.Fprintf(&b, "- 错因：%s\n", f.ErrorCause)
		}
		if f.WrongProcess != "" {
			fmt.Fprintf(&b, "- 错误过程：%s\n", f.WrongProcess)
		}
		fmt.Fprintf(&b, "- 状态：%s\n\n", r.Status)
	}
	return b.String(), nil
}
