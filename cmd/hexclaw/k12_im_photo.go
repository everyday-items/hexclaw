package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const k12TutorScenario = "k12-tutor"

type k12PhotoProcessor func(context.Context, k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error)

// maybeHandleK12DingtalkPhoto is the composition-root seam between generic IM
// delivery and the K12 photo workflow. It deliberately requires an explicit
// routing rule: making a K12 agent the global default must not turn every
// unrelated DingTalk picture into a homework-grading request.
func maybeHandleK12DingtalkPhoto(
	ctx context.Context,
	msg *adapter.Message,
	router *agentrouter.Dispatcher,
	process k12PhotoProcessor,
) (*adapter.Reply, bool, error) {
	if msg == nil || router == nil || process == nil || msg.Platform != adapter.PlatformDingtalk {
		return nil, false, nil
	}
	if len(msg.Attachments) != 1 || strings.TrimSpace(msg.Attachments[0].Type) != "image" {
		return nil, false, nil
	}

	routed := router.Route(agentrouter.RouteRequest{
		Platform:   string(msg.Platform),
		InstanceID: msg.InstanceID,
		UserID:     msg.UserID,
		ChatID:     msg.ChatID,
	})
	if routed == nil || routed.Rule == nil || routed.AgentConfig == nil ||
		strings.TrimSpace(routed.AgentConfig.Metadata["scenario"]) != k12TutorScenario {
		return nil, false, nil
	}

	raw, err := decodeK12PhotoAttachment(msg.Attachments[0])
	if err != nil {
		return nil, true, err
	}
	sourceSession := strings.TrimSpace(msg.SessionID)
	if sourceSession == "" {
		sourceSession = strings.TrimSpace(msg.ChatID)
	}
	if sourceSession == "" {
		sourceSession = strings.TrimSpace(msg.ID)
	}
	result, err := process(ctx, k12usecase.PhotoGradeRequest{
		AgentName:     routed.AgentName,
		Grade:         strings.TrimSpace(routed.AgentConfig.Metadata[k12.MetaKeyGradeTerm]),
		SourceSession: sourceSession,
		Image:         raw,
	})
	if err != nil {
		return nil, true, err
	}

	reply := &adapter.Reply{Content: result.Markdown}
	if result.AnnotatedImage != nil && len(result.AnnotatedImage.Data) > 0 {
		mime := strings.TrimSpace(result.AnnotatedImage.MIME)
		if mime == "" {
			mime = "image/png"
		}
		reply.Attachments = []adapter.Attachment{{
			Type: "image",
			Name: correctedPhotoFilename(mime),
			Mime: mime,
			Data: base64.StdEncoding.EncodeToString(result.AnnotatedImage.Data),
		}}
	}
	return reply, true, nil
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
