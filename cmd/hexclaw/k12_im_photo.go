package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const k12TutorScenario = "k12-tutor"

type k12PhotoProcessor func(context.Context, k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error)

// k12PhotoJobOrchestrator 是钉钉拍照批改入口所需的统一 GradingJob 编排器子集
// （usecase.GradingOrchestrator 实现；接口化便于入口测试注入替身）。
type k12PhotoJobOrchestrator interface {
	StartPhotoGradingJob(ctx context.Context, in k12usecase.StartPhotoGradingInput) (k12usecase.GradingJobView, bool, error)
	RunGradingJob(ctx context.Context, jobID string) (k12usecase.GradingJobView, error)
	ConfirmAndRun(ctx context.Context, jobID string, corrections []string) (k12usecase.GradingJobView, error)
	PhotoResult(jobID string) (k12usecase.PhotoGradeResult, bool)
	ReleaseGradingRun(jobID string)
}

// maybeHandleK12DingtalkPhotoJob 是钉钉拍照批改的统一 GradingJob 入口（架构设计 §6.7）：
// 收到照片 → CreateGradingJob → 编排器推进 → awaiting_confirmation 按现有 IM 交互确认 →
// completed 后按现有投递逻辑发结果。行为对家长不变（消息文案/附件与旧路径一致），内部走 Job。
//
// 过渡降级（一次切换前的临时路径，执行计划 §3.4「入口自编排」钉钉侧收敛后删除）：
// Job 创建失败回退旧自编排路径并记日志；orchestrator 未装配时同样走旧路径。
func maybeHandleK12DingtalkPhotoJob(
	ctx context.Context,
	msg *adapter.Message,
	router *agentrouter.Dispatcher,
	orchestrator k12PhotoJobOrchestrator,
	process k12PhotoProcessor,
) (*adapter.Reply, bool, error) {
	routed := routeK12DingtalkPhotoTutor(msg, router)
	if routed == nil || process == nil {
		return nil, false, nil
	}
	req, err := k12PhotoRequestFromMessage(msg, routed)
	if err != nil {
		return nil, true, err
	}
	if orchestrator != nil {
		result, jobStarted, jobErr := gradeK12PhotoViaJob(ctx, orchestrator, msg, req)
		if jobStarted {
			if jobErr != nil {
				// Job 已创建：阶段失败已安全落 failed_retryable/failed_terminal，
				// 错误面与旧路径一致返回；不得二次跑旧路径重复调模型。
				return nil, true, jobErr
			}
			return k12PhotoReply(result), true, nil
		}
		slog.Warn("K12 钉钉拍照批改: GradingJob 创建失败，本次回退旧自编排路径（过渡降级）",
			"err", jobErr, "message_id", msg.ID)
	}
	result, err := process(ctx, req)
	if err != nil {
		return nil, true, err
	}
	return k12PhotoReply(result), true, nil
}

// gradeK12PhotoViaJob 走统一 GradingJob 状态机推进一次照片批改。
// 返回 jobStarted=false 表示 Job 尚未创建成功（调用方可回退旧路径）。
func gradeK12PhotoViaJob(
	ctx context.Context,
	orchestrator k12PhotoJobOrchestrator,
	msg *adapter.Message,
	req k12usecase.PhotoGradeRequest,
) (k12usecase.PhotoGradeResult, bool, error) {
	sourceKey := strings.TrimSpace(msg.ID)
	if sourceKey == "" {
		sourceKey = req.SourceSession
	}
	view, _, err := orchestrator.StartPhotoGradingJob(ctx, k12usecase.StartPhotoGradingInput{
		Photo:      req,
		SourceKind: "im", // §4.10 统一幂等键：IM 用 message_id
		SourceKey:  sourceKey,
	})
	if err != nil {
		return k12usecase.PhotoGradeResult{}, false, err
	}
	jobID := view.Record.RecordID
	defer orchestrator.ReleaseGradingRun(jobID)

	view, err = orchestrator.RunGradingJob(ctx, jobID)
	if err != nil {
		return k12usecase.PhotoGradeResult{}, true, err
	}
	if view.Record.Status == k12.GradingStageAwaitingConfirmation {
		// 现有钉钉交互没有确认停留（拍照即整卷按识别结果批改）。Job 化保持对家长
		// 行为不变 = 到停点立即按识别结果确认（corrections 空）；将来上确认卡片
		// 交互时，这里改为真正停等家长的 Confirm 命令。
		view, err = orchestrator.ConfirmAndRun(ctx, jobID, nil)
		if err != nil {
			return k12usecase.PhotoGradeResult{}, true, err
		}
	}
	if view.Record.Status != k12.GradingStageCompleted {
		return k12usecase.PhotoGradeResult{}, true, fmt.Errorf(
			"K12 钉钉拍照批改: 任务未完成（stage=%s failure=%s）", view.Record.Status, view.Fields.FailureKind)
	}
	result, ok := orchestrator.PhotoResult(jobID)
	if !ok {
		return k12usecase.PhotoGradeResult{}, true, fmt.Errorf("K12 钉钉拍照批改: 任务已完成但批改产物缺失（job=%s）", jobID)
	}
	return result, true, nil
}

// maybeHandleK12DingtalkPhoto is the composition-root seam between generic IM
// delivery and the K12 photo workflow. It deliberately requires an explicit
// routing rule: making a K12 agent the global default must not turn every
// unrelated DingTalk picture into a homework-grading request.
//
// 旧自编排路径（执行计划 §3.4 待删除项·钉钉侧）：生产 messageHandler 已改走
// maybeHandleK12DingtalkPhotoJob；本函数保留为其过渡降级与既有测试入口，
// 切换观察窗结束后随「入口自编排」一并删除。
func maybeHandleK12DingtalkPhoto(
	ctx context.Context,
	msg *adapter.Message,
	router *agentrouter.Dispatcher,
	process k12PhotoProcessor,
) (*adapter.Reply, bool, error) {
	return maybeHandleK12DingtalkPhotoJob(ctx, msg, router, nil, process)
}

// routeK12DingtalkPhotoTutor 路由门禁：仅钉钉单图 + 显式绑定 K12 辅导 Agent 时接管。
// K12-INV-015：当前渠道只允许 direct，群 conversation 永不进入业务流——群聊消息
// （钉钉 conversation_type="2"）在此静默交还通用路径，绝不触发批改。
func routeK12DingtalkPhotoTutor(msg *adapter.Message, router *agentrouter.Dispatcher) *agentrouter.RoutingResult {
	if msg == nil || router == nil || msg.Platform != adapter.PlatformDingtalk {
		return nil
	}
	if msg.Metadata["conversation_type"] == "2" { // 钉钉群聊约定值（adapter/dingtalk 同一口径）
		return nil
	}
	if len(msg.Attachments) != 1 || strings.TrimSpace(msg.Attachments[0].Type) != "image" {
		return nil
	}
	routed := router.Route(agentrouter.RouteRequest{
		Platform:   string(msg.Platform),
		InstanceID: msg.InstanceID,
		UserID:     msg.UserID,
		ChatID:     msg.ChatID,
	})
	if routed == nil || routed.Rule == nil || routed.AgentConfig == nil ||
		strings.TrimSpace(routed.AgentConfig.Metadata["scenario"]) != k12TutorScenario {
		return nil
	}
	return routed
}

// k12PhotoRequestFromMessage 解码附件并组装批改请求（含 source session 回退链）。
func k12PhotoRequestFromMessage(msg *adapter.Message, routed *agentrouter.RoutingResult) (k12usecase.PhotoGradeRequest, error) {
	raw, err := decodeK12PhotoAttachment(msg.Attachments[0])
	if err != nil {
		return k12usecase.PhotoGradeRequest{}, err
	}
	sourceSession := strings.TrimSpace(msg.SessionID)
	if sourceSession == "" {
		sourceSession = strings.TrimSpace(msg.ChatID)
	}
	if sourceSession == "" {
		sourceSession = strings.TrimSpace(msg.ID)
	}
	return k12usecase.PhotoGradeRequest{
		AgentName:     routed.AgentName,
		Grade:         strings.TrimSpace(routed.AgentConfig.Metadata[k12.MetaKeyGradeTerm]),
		SourceSession: sourceSession,
		Image:         raw,
	}, nil
}

// k12PhotoReply 按既有投递逻辑组装 IM 回复：先产 ChannelNeutralMessage（§6.10 ChannelPort
// 收敛，批改结果 Markdown + 可选批注图），再经钉钉投影为 adapter.Reply——
// 拍照批改是入站消息的同步回复，传输仍走 adapter 回执链，仅消息构造收敛到通道中立层，
// 附件名/MIME/base64 编码与直连时代逐字节一致。
func k12PhotoReply(result k12usecase.PhotoGradeResult) *adapter.Reply {
	return adapterReplyFromChannelMessage(k12PhotoChannelMessage(result))
}

// k12PhotoChannelMessage 把批改产物装配为通道中立图文消息（发图文：批改结果+批注图）。
// 批改 Markdown 在此过 IM 出口 LaTeX→Unicode 兜底（钉钉不渲染 LaTeX；识别/批改侧
// 真机取证过模型会违反提示词的 Unicode 约束——BUG-20260712-U）；桌面 HTTP 面的
// 批改结果不经此路径，保持原文。
func k12PhotoChannelMessage(result k12usecase.PhotoGradeResult) channel.Message {
	attachments := make([]channel.Attachment, 0, 1)
	if result.AnnotatedImage != nil && len(result.AnnotatedImage.Data) > 0 {
		mime := strings.TrimSpace(result.AnnotatedImage.MIME)
		if mime == "" {
			mime = "image/png"
		}
		attachments = append(attachments, channel.Attachment{
			Name: correctedPhotoFilename(mime),
			MIME: mime,
			Data: result.AnnotatedImage.Data,
		})
	}
	projected := imLaTeXFallback(result.Markdown, "k12_photo_grading")
	fallbackReason := ""
	if projected != result.Markdown {
		fallbackReason = messagecontent.FallbackMathToReadableText
	}
	msg, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		result.Markdown,
		projected,
		fallbackReason,
		attachments,
	)
	if err != nil {
		return channel.Message{Text: "批改结果渲染失败，请重试。"}
	}
	return msg
}

func correctedPhotoFilename(mime string) string {
	if strings.EqualFold(strings.TrimSpace(mime), "image/jpeg") || strings.EqualFold(strings.TrimSpace(mime), "image/jpg") {
		return "批改后的作业.jpg"
	}
	return "批改后的作业.png"
}

func decodeK12PhotoAttachment(att adapter.Attachment) ([]byte, error) {
	encoded := strings.TrimSpace(att.Data)
	if encoded == "" {
		return nil, fmt.Errorf("K12 钉钉拍照批改: 图片数据为空")
	}
	if strings.HasPrefix(encoded, "data:") {
		comma := strings.IndexByte(encoded, ',')
		if comma < 0 {
			return nil, fmt.Errorf("K12 钉钉拍照批改: 图片 data URL 无效")
		}
		encoded = encoded[comma+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("K12 钉钉拍照批改: 解码图片: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("K12 钉钉拍照批改: 图片数据为空")
	}
	return raw, nil
}
