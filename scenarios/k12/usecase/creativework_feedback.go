package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
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

type WorkFeedbackRouteResolver func(
	context.Context,
	string,
) (k12.ImageTaskRouteSnapshot, error)

type workFeedbackRouteSnapshotContextKey struct{}

// withWorkFeedbackRouteSnapshot carries the owning ImageTask's frozen route
// into work promotion. It is deliberately separate from the provider-facing
// GradingModelSnapshot: this value is consumed while preparing the durable
// invocation, before any provider call can happen.
func withWorkFeedbackRouteSnapshot(
	ctx context.Context,
	snapshot k12.ImageTaskRouteSnapshot,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(
		ctx,
		workFeedbackRouteSnapshotContextKey{},
		k12.NormalizeImageTaskRouteSnapshot(snapshot),
	)
}

func workFeedbackRouteSnapshotFromContext(
	ctx context.Context,
	workType string,
) (k12.ImageTaskRouteSnapshot, bool) {
	if ctx == nil {
		return k12.ImageTaskRouteSnapshot{}, false
	}
	snapshot, ok := ctx.Value(workFeedbackRouteSnapshotContextKey{}).(k12.ImageTaskRouteSnapshot)
	if !ok {
		return k12.ImageTaskRouteSnapshot{}, false
	}
	snapshot = k12.NormalizeImageTaskRouteSnapshot(snapshot)
	if strings.TrimSpace(workType) == k12.WorkTypeArt {
		snapshot.Capability = "text+vision"
		snapshot.PromptVersion = "art-feedback-v1"
	} else {
		snapshot.Capability = "text"
		snapshot.PromptVersion = "writing-feedback-v1"
	}
	return snapshot, true
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
	if v.Fields.WorkType == k12.WorkTypeWriting && strings.TrimSpace(last.SourceAssetID) != "" {
		var validateErr error
		if strings.TrimSpace(v.Fields.SourceIntakeID) != "" {
			validateErr = d.validatePromotedWritingFeedbackSnapshot(ctx, v, last)
		} else {
			validateErr = validateConfirmedWritingFeedbackSnapshot(last)
		}
		if validateErr != nil {
			return CreativeWorkView{}, validateErr
		}
	}
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

	invocation, replay, err := d.prepareWorkFeedbackInvocation(ctx, v, last, req)
	if err != nil {
		return CreativeWorkView{}, err
	}
	var out WorkFeedbackOutput
	if replay != nil {
		out = *replay
	} else {
		providerCtx := ctx
		if invocation != nil {
			if _, err := d.Records.MarkImageTaskInvocationSent(
				ctx, agentName, invocation.InvocationID,
				"creative-work:"+recordID+":feedback",
			); err != nil {
				return CreativeWorkView{}, err
			}
			var cancelProvider context.CancelFunc
			providerCtx, cancelProvider = imageTaskProviderContext(
				ctx,
				invocation.RouteSnapshot,
			)
			defer cancelProvider()
		}
		out, err = gen.GenerateWorkFeedback(providerCtx, req)
		if err != nil {
			if invocation != nil {
				unknown := sentProviderOutcomeUnknown(err, providerCtx.Err())
				_ = d.Records.FailWorkFeedbackInvocation(
					context.WithoutCancel(ctx), agentName, invocation.InvocationID,
					"work_feedback_provider_failed", unknown, !unknown,
				)
			}
			return CreativeWorkView{}, fmt.Errorf("%w: 作品点评生成失败: %v", ErrSolveFailed, err)
		}
	}
	feedback := strings.TrimSpace(out.Feedback)
	if feedback == "" {
		if invocation != nil && replay == nil {
			_ = d.Records.FailWorkFeedbackInvocation(
				context.WithoutCancel(ctx), agentName, invocation.InvocationID,
				"work_feedback_empty", false, true,
			)
		}
		return CreativeWorkView{}, fmt.Errorf("%w: 作品点评生成为空", ErrSolveFailed)
	}
	if reason := workFeedbackInvariantViolation(feedback); reason != "" {
		if invocation != nil && replay == nil {
			_ = d.Records.FailWorkFeedbackInvocation(
				context.WithoutCancel(ctx), agentName, invocation.InvocationID,
				"work_feedback_contract_invalid", false, true,
			)
		}
		return CreativeWorkView{}, fmt.Errorf("%w: 生成点评违反 INV-011（%s），已拒绝入库", ErrSolveFailed, reason)
	}
	if invocation != nil && replay == nil {
		resultJSON, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			return CreativeWorkView{}, marshalErr
		}
		if err := d.Records.CompleteWorkFeedbackInvocation(
			ctx, agentName, invocation.InvocationID, digestJSON(out), string(resultJSON),
		); err != nil {
			_ = d.Records.FailWorkFeedbackInvocation(
				context.WithoutCancel(ctx), agentName, invocation.InvocationID,
				"work_feedback_receipt_commit_failed", true, false,
			)
			return CreativeWorkView{}, err
		}
	}
	return d.attachAIFeedback(ctx, agentName, recordID, feedback, strings.TrimSpace(out.SkillStamp))
}

func (d Deps) validatePromotedWritingFeedbackSnapshot(
	ctx context.Context,
	work CreativeWorkView,
	version k12.CreativeWorkVersion,
) error {
	if d.Records == nil || work.Record == nil {
		return fmt.Errorf("%w: 作文图片接入证据存储未配置", ErrInvalidInput)
	}
	intake, err := d.Records.GetCreativeWorkIntake(
		ctx, work.Record.AgentName, work.Fields.SourceIntakeID,
	)
	if err != nil {
		return err
	}
	if intake.AgentName != work.Record.AgentName ||
		intake.Status != k12.CreativeWorkIntakePromoted ||
		intake.PromotedWorkID != work.Record.RecordID ||
		intake.WorkType != k12.WorkTypeWriting ||
		intake.OCREvidence == nil ||
		len(intake.SourceAssetRefs) == 0 ||
		version.SourceAssetID != intake.SourceAssetRefs[0] ||
		strings.TrimSpace(version.ContentMarkdown) == "" ||
		version.ContentMarkdown != intake.OCREvidence.CanonicalContent ||
		version.OCRVersion != intake.OCREvidence.CanonicalVersion ||
		version.OCRConfirmedDigest != intake.OCREvidence.CanonicalDigest ||
		version.ContentConfirmedAt <= 0 ||
		intake.OCREvidence.FrozenAt <= 0 {
		return fmt.Errorf("%w: 作文图片接入的 canonical OCR 证据不完整或已漂移", ErrInvalidInput)
	}
	sum := sha256.Sum256([]byte(version.ContentMarkdown))
	if "sha256:"+hex.EncodeToString(sum[:]) != version.OCRConfirmedDigest {
		return fmt.Errorf("%w: 作文正文与接入 OCR 确认摘要不一致", ErrInvalidInput)
	}
	switch intake.ConfirmationProvenance {
	case k12.CreativeWorkEvidenceAutoFreeze,
		k12.CreativeWorkParentConfirmed,
		k12.CreativeWorkParentCorrected:
		return nil
	default:
		return fmt.Errorf("%w: 作文图片接入缺少冻结 provenance", ErrInvalidInput)
	}
}

func (d Deps) prepareWorkFeedbackInvocation(
	ctx context.Context,
	work CreativeWorkView,
	version k12.CreativeWorkVersion,
	req WorkFeedbackRequest,
) (*k12.ImageTaskInvocation, *WorkFeedbackOutput, error) {
	if strings.TrimSpace(work.Fields.SourceIntakeID) == "" {
		return nil, nil, nil
	}
	if d.Records == nil {
		return nil, nil, fmt.Errorf("usecase: image task store 未配置")
	}
	intake, err := d.Records.GetCreativeWorkIntake(
		ctx, work.Record.AgentName, work.Fields.SourceIntakeID,
	)
	if err != nil {
		return nil, nil, err
	}
	if intake.Status != k12.CreativeWorkIntakePromoted ||
		intake.PromotedWorkID != work.Record.RecordID ||
		intake.AgentName != work.Record.AgentName {
		return nil, nil, k12storage.ErrImageTaskConflict
	}
	operationKey := "work:" + work.Record.RecordID + ":version:" +
		version.VersionID + ":feedback"
	requestDigest := digestJSON(req)
	prior, err := d.Records.GetLatestWorkFeedbackInvocation(
		ctx, work.Record.AgentName, work.Record.RecordID, operationKey,
	)
	switch {
	case err == nil && prior.Status == k12.ImageTaskInvocationSucceeded:
		var out WorkFeedbackOutput
		if jsonErr := json.Unmarshal([]byte(prior.ResultJSON), &out); jsonErr != nil {
			return nil, nil, k12storage.ErrImageTaskConflict
		}
		return &prior, &out, nil
	case err == nil && prior.Status == k12.ImageTaskInvocationPrepared:
		if prior.RequestDigest != requestDigest {
			return nil, nil, k12storage.ErrImageTaskConflict
		}
		return &prior, nil, nil
	case err == nil && prior.Status == k12.ImageTaskInvocationFailed && prior.RetrySafe:
		if prior.RequestDigest != requestDigest {
			return nil, nil, k12storage.ErrImageTaskConflict
		}
		next := k12.ImageTaskInvocation{
			InvocationID: idgen.NanoID(), AgentName: work.Record.AgentName,
			WorkRecordID: work.Record.RecordID,
			Operation:    k12.ImageTaskOperationWorkFeedback,
			OperationKey: prior.OperationKey, RequestDigest: prior.RequestDigest,
			RouteSnapshot: prior.RouteSnapshot, Status: k12.ImageTaskInvocationPrepared,
			Attempt: prior.Attempt + 1,
		}
		prepared, _, prepareErr := d.Records.PrepareImageTaskInvocation(ctx, next)
		return &prepared, nil, prepareErr
	case err == nil:
		return nil, nil, k12storage.ErrImageTaskInvalidState
	case !errors.Is(err, k12storage.ErrImageTaskNotFound):
		return nil, nil, err
	}
	route, pinned := workFeedbackRouteSnapshotFromContext(ctx, work.Fields.WorkType)
	if !pinned {
		// Legacy/manual CreativeWork has no owning ImageTask dispatch. Only that
		// path may resolve the current default when its first invocation is
		// prepared.
		if d.WorkFeedbackRoute == nil {
			return nil, nil, fmt.Errorf("usecase: promoted 作品点评 route resolver 未配置")
		}
		route, err = d.WorkFeedbackRoute(ctx, work.Fields.WorkType)
		if err != nil {
			return nil, nil, err
		}
	}
	route = k12.NormalizeImageTaskRouteSnapshot(route)
	if err := route.Validate(); err != nil {
		return nil, nil, err
	}
	invocation := k12.ImageTaskInvocation{
		InvocationID: idgen.NanoID(), AgentName: work.Record.AgentName,
		WorkRecordID: work.Record.RecordID,
		Operation:    k12.ImageTaskOperationWorkFeedback,
		OperationKey: operationKey, RequestDigest: requestDigest,
		RouteSnapshot: route, Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
	}
	prepared, _, err := d.Records.PrepareImageTaskInvocation(ctx, invocation)
	return &prepared, nil, err
}

func validateConfirmedWritingFeedbackSnapshot(version k12.CreativeWorkVersion) error {
	content := strings.TrimSpace(version.ContentMarkdown)
	if version.OCRJobID == "" || version.OCRVersion <= 0 ||
		version.OCRConfirmedDigest == "" || version.ContentConfirmedAt <= 0 || content == "" {
		return fmt.Errorf("%w: 作文照片没有完整的家长 OCR 确认证据，禁止生成点评", ErrInvalidInput)
	}
	sum := sha256.Sum256([]byte(content))
	if hex.EncodeToString(sum[:]) != version.OCRConfirmedDigest {
		return fmt.Errorf("%w: 作文正文与 OCR 确认摘要不一致，禁止复用旧摘要", ErrInvalidInput)
	}
	return nil
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
