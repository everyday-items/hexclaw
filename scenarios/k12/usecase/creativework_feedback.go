package usecase

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// WorkFeedbackRequest 是作品点评生成请求：只携带作品的可见证据（题目要求、原文/原图、
// 孩子意图），Skill 层据此生成，不发明输入（PRD §3.10）。
type WorkFeedbackRequest struct {
	WorkType        string // writing / art
	Title           string
	Task            string // 题目要求或创作任务
	Intent          string // 孩子想表达的内容（美术）
	ContentMarkdown string // 最新版本文字内容（写作）
	SourceAssetID   string // 最新版本原图（美术）
	Grade           string // 生效年级（约束点评口径）
}

// WorkFeedbackOutput 是作品点评生成结果：点评正文 + 方法论基座来源戳。
type WorkFeedbackOutput struct {
	Feedback string
	// SkillStamp 生成本条点评实际使用的方法论基座来源戳，随点评落库（feedback_skill 字段）
	// 供追溯：形如 "writing-feedback@1.0.0/disk"（盘上 marketplace 版本）、
	// "art-feedback@1.0.0/embedded"（发版内嵌快照）、"builtin"（硬编码红线兜底）。
	// 空值合法（生成器未申报来源），落库时按空处理，不猜。
	SkillStamp string
}

// WorkFeedbackGenerator 是作品点评生成的可选 Skill Executor 扩展 port（与 RetryGenerator/
// CauseSummarizer 同纪律：由 Solver 实现发现，未实现即诚实报错，不留假点评）。
// 输出契约（INV-011）：只点评不打分不代写——写作为好句摘出 + 一处具体建议，美术为
// 观察描述式点评；含分数/等第/代写成文的输出由用例层拒绝入库。
type WorkFeedbackGenerator interface {
	GenerateWorkFeedback(ctx context.Context, req WorkFeedbackRequest) (WorkFeedbackOutput, error)
}

// GenerateWorkFeedback 对 draft/revised 作品调 Skill Executor 生成证据化点评并落库
// （→ feedback_ready，来源标记 ai，与家长手写 parent 区分）。
// 失败路径全部诚实报错：未接线、生成失败、违反 INV-011 都不写库。
func (d Deps) GenerateWorkFeedback(ctx context.Context, agentName, recordID string) (CreativeWorkView, error) {
	v, err := d.GetCreativeWork(ctx, agentName, recordID) // 归属校验先行，未过不触发生成
	if err != nil {
		return CreativeWorkView{}, err
	}
	if v.Record.Status != k12.WorkStatusDraft && v.Record.Status != k12.WorkStatusRevised {
		return CreativeWorkView{}, fmt.Errorf("usecase: 只有待点评/已修改作品可生成点评，当前 %s", v.Record.Status)
	}
	if len(v.Fields.Versions) == 0 {
		return CreativeWorkView{}, fmt.Errorf("usecase: 作品无版本可点评")
	}
	gen, ok := d.Solver.(WorkFeedbackGenerator)
	if !ok {
		return CreativeWorkView{}, fmt.Errorf("%w: 未配置作品点评生成能力", ErrSolveFailed)
	}

	last := v.Fields.Versions[len(v.Fields.Versions)-1]
	req := WorkFeedbackRequest{
		WorkType:        v.Fields.WorkType,
		Title:           v.Fields.Title,
		Task:            v.Fields.Task,
		Intent:          v.Fields.Intent,
		ContentMarkdown: strings.TrimSpace(last.ContentMarkdown),
		SourceAssetID:   last.SourceAssetID,
	}
	// 无任何可见证据（无文字也无原图）不生成——点评必须有依据（INV-011 的另一半：不虚构）。
	if req.ContentMarkdown == "" && req.SourceAssetID == "" {
		return CreativeWorkView{}, fmt.Errorf("%w: 作品最新版本无原文/原图，无法生成证据化点评", ErrInvalidInput)
	}
	// 资产归属双保险（入口 CreateCreativeWork/SubmitRevision 已拦；此处防旧数据/旁路写入）：
	// asset:// 载体不属于本实例时拒绝送视觉模型。
	if err := validateWorkAssetOwner(agentName, req.SourceAssetID); err != nil {
		return CreativeWorkView{}, err
	}
	if d.Profiles != nil {
		if p, perr := d.GetProfile(ctx, agentName); perr == nil {
			req.Grade = p.GradeTerm
		}
	}

	out, err := gen.GenerateWorkFeedback(ctx, req)
	if err != nil {
		return CreativeWorkView{}, fmt.Errorf("%w: 作品点评生成失败: %v", ErrSolveFailed, err)
	}
	feedback := strings.TrimSpace(out.Feedback)
	if feedback == "" {
		return CreativeWorkView{}, fmt.Errorf("%w: 作品点评生成为空", ErrSolveFailed)
	}
	if reason := workFeedbackInvariantViolation(feedback); reason != "" {
		return CreativeWorkView{}, fmt.Errorf("%w: 生成点评违反 INV-011（%s），已拒绝入库", ErrSolveFailed, reason)
	}
	return d.attachFeedbackWithSource(ctx, agentName, recordID, feedback, k12.FeedbackSourceAI, strings.TrimSpace(out.SkillStamp))
}

// scorePattern 命中“数字+分”打分口径；“分钟/分钟数”另行放行（见下）。
var scorePattern = regexp.MustCompile(`[0-9０-９]+(\.[0-9]+)?\s*分`)

// scoreOutOfPattern 命中“85/100”类比值打分。
var scoreOutOfPattern = regexp.MustCompile(`[0-9]+\s*/\s*(10|100)\b`)

// 打分/等第/代写的关键词黑名单（INV-011 从紧口径：宁可错拒重生成，不放假点评入库）。
var (
	scoreWords = []string{"打分", "评分", "得分", "满分", "总分", "计分", "评级", "评等", "排名", "名次"}
	rankWords  = []string{"等第", "甲等", "乙等", "丙等", "优等", "次等", "不及格", "及格线"}
	ghostWords = []string{"范文", "代写", "帮你写", "替你写", "替他写", "改写全文", "重写全文", "全文如下", "修改后的作文", "示范作文", "我来重画", "替孩子重画"}
)

// WorkFeedbackRedlineViolation 是 workFeedbackInvariantViolation 的导出别名，
// 供 §5.7 第 6 套 eval（作品反馈·禁则违反率）以生产拦截器为唯一真相做确定性评测
// （scenarios/k12/eval），避免评测侧复刻黑名单形成两套真相。
func WorkFeedbackRedlineViolation(feedback string) string {
	return workFeedbackInvariantViolation(feedback)
}

// workFeedbackInvariantViolation 检查生成点评是否违反 INV-011（只点评不打分不代写）。
// 返回非空原因即违规。关键词/模式为确定性契约（契约测试钉死），不依赖模型自律。
func workFeedbackInvariantViolation(feedback string) string {
	for _, w := range scoreWords {
		if strings.Contains(feedback, w) {
			return "出现打分口径「" + w + "」"
		}
	}
	for _, w := range rankWords {
		if strings.Contains(feedback, w) {
			return "出现等第口径「" + w + "」"
		}
	}
	for _, w := range ghostWords {
		if strings.Contains(feedback, w) {
			return "出现代写口径「" + w + "」"
		}
	}
	// “数字+分”视为打分，但“10 分钟”这类时长表述放行（分后紧跟“钟”）。
	for _, loc := range scorePattern.FindAllStringIndex(feedback, -1) {
		rest := feedback[loc[1]:]
		if !strings.HasPrefix(rest, "钟") {
			return "出现分数「" + feedback[loc[0]:loc[1]] + "」"
		}
	}
	if m := scoreOutOfPattern.FindString(feedback); m != "" {
		return "出现比值打分「" + m + "」"
	}
	return ""
}
