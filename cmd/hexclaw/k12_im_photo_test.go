package main

import (
	"encoding/base64"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestK12PhotoReply_AnsweredSheetReturnsAnnotatedPNG(t *testing.T) {
	reply := k12PhotoReply(k12usecase.PhotoGradeResult{
		Mode: k12usecase.PhotoModeGrade, Markdown: "## 作业批改\n\n共 2 道题。",
		AnnotatedImage: &k12usecase.RenderedPhoto{
			Data: []byte("corrected png bytes"), MIME: "image/png",
		},
	})
	if reply == nil || reply.Content != "## 作业批改\n\n共 2 道题。" ||
		len(reply.Attachments) != 1 {
		t.Fatalf("统一结果投影缺少批注图: %#v", reply)
	}
	att := reply.Attachments[0]
	if att.Type != "image" || att.Mime != "image/png" || att.Name != "批改后的作业.png" {
		t.Fatalf("批注附件元数据错误: %#v", att)
	}
	decoded, err := base64.StdEncoding.DecodeString(att.Data)
	if err != nil || string(decoded) != "corrected png bytes" {
		t.Fatalf("批注附件 payload=%q err=%v", decoded, err)
	}
}

func TestREGBUGK12C02StandaloneVariable003_PhotoReplyKeepsMarkdownAndAttachment(t *testing.T) {
	reply := k12PhotoReply(k12usecase.PhotoGradeResult{
		Mode:     k12usecase.PhotoModeGrade,
		Markdown: "## 作业批改\n\n设 $x$ 为边长，面积记为 $S$；价格仍写作 $5。",
		AnnotatedImage: &k12usecase.RenderedPhoto{
			Data: []byte("corrected image"), MIME: "image/png",
		},
	})
	if reply.Content != "## 作业批改\n\n设 x 为边长，面积记为 S；价格仍写作 $5。" {
		t.Fatalf("standalone variable reply projection=%q", reply.Content)
	}
	if len(reply.Attachments) != 1 {
		t.Fatalf("standalone variable projection dropped annotated image: %#v", reply.Attachments)
	}
}

func TestK12PhotoReply_BlankSheetReturnsMarkdownOnly(t *testing.T) {
	reply := k12PhotoReply(k12usecase.PhotoGradeResult{
		Mode: k12usecase.PhotoModeSolve, Markdown: "## 作业解题\n\n### 1. 4.5 × 2",
	})
	if reply == nil || reply.Content == "" || len(reply.Attachments) != 0 {
		t.Fatalf("空白卷不得伪造批注图: %#v", reply)
	}
}

func TestK12PhotoReply_CompressedJPEGUsesMatchingFilename(t *testing.T) {
	reply := k12PhotoReply(k12usecase.PhotoGradeResult{
		Mode: k12usecase.PhotoModeGrade, Markdown: "## 作业批改",
		AnnotatedImage: &k12usecase.RenderedPhoto{
			Data: []byte("bounded jpeg"), MIME: "image/jpeg",
		},
	})
	if len(reply.Attachments) != 1 || reply.Attachments[0].Name != "批改后的作业.jpg" {
		t.Fatalf("JPEG 批注图扩展名不匹配: %#v", reply.Attachments)
	}
}

func TestRouteK12DingtalkPhotoTutor_OnlyExactExplicitDirectBinding(t *testing.T) {
	tests := []struct {
		name     string
		explicit bool
		scenario string
		mutate   func(*adapter.Message)
		want     bool
	}{
		{name: "explicit direct K12", explicit: true, scenario: "k12-tutor", want: true},
		{name: "default K12 does not steal", explicit: false, scenario: "k12-tutor"},
		{name: "non K12 agent", explicit: true, scenario: "general"},
		{name: "legacy ambiguous marker", explicit: true, scenario: "k12"},
		{name: "other platform", explicit: true, scenario: "k12-tutor",
			mutate: func(m *adapter.Message) { m.Platform = adapter.PlatformFeishu }},
		{name: "text only", explicit: true, scenario: "k12-tutor",
			mutate: func(m *adapter.Message) { m.Attachments = nil }},
		{name: "group conversation", explicit: true, scenario: "k12-tutor",
			mutate: func(m *adapter.Message) {
				m.Metadata = map[string]string{"conversation_type": "2"}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := k12PhotoTestMessage()
			if tt.mutate != nil {
				tt.mutate(msg)
			}
			got := routeK12DingtalkPhotoTutor(
				msg, k12PhotoTestRouter(t, tt.explicit, tt.scenario),
			) != nil
			if got != tt.want {
				t.Fatalf("route=%v want=%v", got, tt.want)
			}
		})
	}
}

func k12PhotoTestRouter(t *testing.T, explicit bool, scenario string) *agentrouter.Dispatcher {
	t.Helper()
	r := agentrouter.New()
	agents := []agentrouter.AgentConfig{
		{Name: "general", Metadata: map[string]string{"scenario": "general"}},
		{Name: "child-tutor", Metadata: map[string]string{
			"scenario": scenario, k12.MetaKeyGradeTerm: "五年级下",
		}},
	}
	var rules []agentrouter.Rule
	if explicit {
		rules = []agentrouter.Rule{{
			Platform: "dingtalk", InstanceID: "bot-1",
			ChatID: "family-group", AgentName: "child-tutor",
		}}
	}
	defaultAgent := "general"
	if !explicit {
		defaultAgent = "child-tutor"
	}
	r.LoadAll(agents, defaultAgent, rules)
	return r
}

func k12PhotoTestMessage() *adapter.Message {
	// One valid 1×1 PNG. ImageTask ingress intentionally validates the actual
	// bytes before it creates a durable dispatch.
	raw, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
	)
	return &adapter.Message{
		ID: "msg-1", Platform: adapter.PlatformDingtalk,
		InstanceID: "bot-1", ChatID: "family-group", UserID: "parent-1",
		Attachments: []adapter.Attachment{{
			Type: "image", Name: "homework.png", Mime: "image/png",
			Data: base64.StdEncoding.EncodeToString(raw),
		}},
	}
}
