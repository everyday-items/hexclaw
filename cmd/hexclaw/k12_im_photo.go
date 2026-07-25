package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const k12TutorScenario = "k12-tutor"

const k12IMImageTaskWaitTimeout = 125 * time.Second

// k12ImageTaskFacade is the only application boundary accepted by image
// ingress adapters. Desktop, API, webhook and IM all create the same durable
// dispatch before any model call; adapters cannot fall back to GradingJob or a
// provider function after that boundary.
type k12ImageTaskFacade interface {
	Create(context.Context, k12usecase.CreateImageTaskInput) (k12usecase.ImageTaskView, bool, error)
	StartAsync(agentName, dispatchID string) bool
	Get(context.Context, string, string) (k12usecase.ImageTaskView, error)
	Confirm(context.Context, k12usecase.ConfirmImageTaskInput) (k12usecase.ImageTaskView, error)
	Result(context.Context, string, string) (k12usecase.ImageTaskResult, error)
}

// maybeHandleK12DingtalkPhoto is the composition-root seam between generic IM
// delivery and the unified K12 ImageTask facade. It deliberately requires an
// explicit direct-message routing rule: a K12 default agent must not steal an
// unrelated picture. Once matched, failure is surfaced honestly; there is no
// second provider path that could duplicate model work or bind idempotency to a
// different route.
func maybeHandleK12DingtalkPhoto(
	ctx context.Context,
	msg *adapter.Message,
	router *agentrouter.Dispatcher,
	imageTasks k12ImageTaskFacade,
) (*adapter.Reply, bool, error) {
	routed := routeK12DingtalkPhotoTutor(msg, router)
	if routed == nil {
		return nil, false, nil
	}
	if imageTasks == nil {
		return nil, true, fmt.Errorf("K12 图片任务服务未配置")
	}
	raw, err := decodeK12PhotoAttachment(msg.Attachments[0])
	if err != nil {
		return nil, true, err
	}
	assetRef, err := assetstore.Save(routed.AgentName, raw)
	if err != nil {
		return nil, true, fmt.Errorf("K12 钉钉图片入库: %w", err)
	}
	sourceRef := strings.TrimSpace(msg.ID)
	sourceSession := k12PhotoSourceSession(msg)
	if sourceRef == "" {
		sourceRef = sourceSession
	}
	view, created, err := imageTasks.Create(ctx, k12usecase.CreateImageTaskInput{
		AgentName: routed.AgentName, LearnerID: routed.AgentName,
		SourceKind: k12.ImageTaskSourceIM, SourceRef: sourceRef,
		SourceSessionID: sourceSession, SourceAssetRefs: []string{assetRef},
		MessageIntent: strings.TrimSpace(msg.Content), AttemptGeneration: 1,
	})
	if err != nil {
		return nil, true, err
	}
	if started := imageTasks.StartAsync(
		routed.AgentName, view.Dispatch.DispatchID,
	); created && !started {
		return nil, true, fmt.Errorf(
			"K12 图片任务已创建但未能启动（dispatch=%s）",
			view.Dispatch.DispatchID,
		)
	}

	waitCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, k12IMImageTaskWaitTimeout)
		defer cancel()
	}
	reply, err := waitK12IMImageTaskResult(
		waitCtx, imageTasks, routed.AgentName, view.Dispatch.DispatchID,
	)
	return reply, true, err
}

func waitK12IMImageTaskResult(
	ctx context.Context,
	imageTasks k12ImageTaskFacade,
	agentName, dispatchID string,
) (*adapter.Reply, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	homeworkConfirmed := false
	for {
		view, err := imageTasks.Get(ctx, agentName, dispatchID)
		if err != nil {
			return nil, err
		}
		switch view.Dispatch.Status {
		case k12.ImageTaskStatusFailed:
			return nil, fmt.Errorf("K12 图片任务失败: %s", view.Dispatch.FailureKind)
		case k12.ImageTaskStatusCancelled:
			return nil, fmt.Errorf("K12 图片任务已取消")
		case k12.ImageTaskStatusAwaitingConfirmation:
			return nil, fmt.Errorf("K12 图片任务需要家长确认图片类型")
		}
		if !homeworkConfirmed && view.HomeworkProjection != nil &&
			view.HomeworkProjection.Stage == k12.GradingStageAwaitingConfirmation {
			if _, err := imageTasks.Confirm(ctx, k12usecase.ConfirmImageTaskInput{
				AgentName: agentName, DispatchID: dispatchID,
				ExpectedVersion: view.Dispatch.Version, Intent: view.Dispatch.TaskIntent,
				Subject: view.HomeworkProjection.Subject,
			}); err != nil {
				return nil, err
			}
			homeworkConfirmed = true
		}
		result, err := imageTasks.Result(ctx, agentName, dispatchID)
		if err != nil {
			return nil, err
		}
		switch result.Kind {
		case string(k12.ImageTaskIntentCompletedHomework), string(k12.ImageTaskIntentBlankWorksheet):
			if result.Photo == nil {
				return nil, fmt.Errorf("K12 图片任务已完成但结果缺失")
			}
			return k12PhotoReply(*result.Photo), nil
		case "creative":
			return k12CreativeWorkReply(result)
		case "awaiting_confirmation":
			return nil, fmt.Errorf("K12 图片任务需要家长确认识别内容")
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("等待 K12 图片任务结果: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func k12CreativeWorkReply(result k12usecase.ImageTaskResult) (*adapter.Reply, error) {
	if result.CreativeWork == nil || len(result.CreativeWork.Fields.Versions) == 0 {
		return nil, fmt.Errorf("K12 作品任务已完成但点评缺失")
	}
	version := result.CreativeWork.Fields.Versions[len(result.CreativeWork.Fields.Versions)-1]
	markdown := strings.TrimSpace(version.Feedback)
	if version.StructuredFeedback != nil {
		markdown = strings.TrimSpace(version.StructuredFeedback.ProjectionMarkdown)
	}
	if markdown == "" {
		return nil, fmt.Errorf("K12 作品任务已完成但点评投影为空")
	}
	projected := imLaTeXFallback(markdown, "k12_creative_feedback")
	fallbackReason := ""
	if projected != markdown {
		fallbackReason = messagecontent.FallbackMathToReadableText
	}
	msg, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12, "zh-CN", markdown, projected, fallbackReason, nil,
	)
	if err != nil {
		return nil, err
	}
	return adapterReplyFromChannelMessage(msg), nil
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

func k12PhotoSourceSession(msg *adapter.Message) string {
	sourceSession := strings.TrimSpace(msg.SessionID)
	if sourceSession == "" {
		sourceSession = strings.TrimSpace(msg.ChatID)
	}
	if sourceSession == "" {
		sourceSession = strings.TrimSpace(msg.ID)
	}
	return sourceSession
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
