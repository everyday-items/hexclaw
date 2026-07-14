package main

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestMaybeHandleK12DingtalkPhoto_ExplicitTutorBindingReturnsAnnotatedPNG(t *testing.T) {
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	rawImage := []byte("real inbound jpeg bytes")
	var captured k12usecase.PhotoGradeRequest
	calls := 0
	process := func(_ context.Context, req k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		calls++
		captured = req
		return k12usecase.PhotoGradeResult{
			Mode:     k12usecase.PhotoModeGrade,
			Markdown: "## 作业批改\n\n共 2 道题。",
			AnnotatedImage: &k12usecase.RenderedPhoto{
				Data: []byte("corrected png bytes"),
				MIME: "image/png",
			},
		}, nil
	}

	reply, handled, err := maybeHandleK12DingtalkPhoto(context.Background(), &adapter.Message{
		ID:         "msg-1",
		Platform:   adapter.PlatformDingtalk,
		InstanceID: "bot-1",
		ChatID:     "family-group",
		UserID:     "parent-1",
		SessionID:  "session-1",
		Attachments: []adapter.Attachment{{
			Type: "image", Name: "homework.jpg", Mime: "image/jpeg",
			Data: base64.StdEncoding.EncodeToString(rawImage),
		}},
	}, router, process)
	if err != nil {
		t.Fatalf("maybeHandleK12DingtalkPhoto: %v", err)
	}
	if !handled || calls != 1 {
		t.Fatalf("explicit k12 binding should be handled exactly once: handled=%v calls=%d", handled, calls)
	}
	if captured.AgentName != "child-tutor" || captured.Grade != "五年级下" || captured.SourceSession != "session-1" {
		t.Fatalf("photo request lost routed tutor/profile/session: %#v", captured)
	}
	if string(captured.Image) != string(rawImage) {
		t.Fatalf("processor image = %q, want %q", captured.Image, rawImage)
	}
	if reply == nil || reply.Content != "## 作业批改\n\n共 2 道题。" {
		t.Fatalf("reply markdown = %#v", reply)
	}
	if len(reply.Attachments) != 1 {
		t.Fatalf("answered sheet should return corrected image attachment: %#v", reply.Attachments)
	}
	att := reply.Attachments[0]
	if att.Type != "image" || att.Mime != "image/png" || att.Name != "批改后的作业.png" {
		t.Fatalf("corrected attachment metadata = %#v", att)
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(att.Data)
	if decodeErr != nil || string(decoded) != "corrected png bytes" {
		t.Fatalf("corrected attachment payload decode=%q err=%v", decoded, decodeErr)
	}
}

func TestMaybeHandleK12DingtalkPhoto_BlankSheetReturnsMarkdownOnly(t *testing.T) {
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	process := func(_ context.Context, _ k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		return k12usecase.PhotoGradeResult{
			Mode:     k12usecase.PhotoModeSolve,
			Markdown: "## 作业解题\n\n### 1. 4.5 × 2",
		}, nil
	}
	reply, handled, err := maybeHandleK12DingtalkPhoto(context.Background(), k12PhotoTestMessage(), router, process)
	if err != nil || !handled {
		t.Fatalf("blank sheet should be handled: handled=%v err=%v", handled, err)
	}
	if reply == nil || reply.Content == "" || len(reply.Attachments) != 0 {
		t.Fatalf("blank sheet must return markdown without fake correction image: %#v", reply)
	}
}

func TestMaybeHandleK12DingtalkPhoto_CompressedJPEGUsesMatchingFilename(t *testing.T) {
	router := k12PhotoTestRouter(t, true, "k12-tutor")
	process := func(_ context.Context, _ k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		return k12usecase.PhotoGradeResult{
			Mode:     k12usecase.PhotoModeGrade,
			Markdown: "## 作业批改",
			AnnotatedImage: &k12usecase.RenderedPhoto{
				Data: []byte("bounded jpeg"), MIME: "image/jpeg",
			},
		}, nil
	}
	reply, handled, err := maybeHandleK12DingtalkPhoto(context.Background(), k12PhotoTestMessage(), router, process)
	if err != nil || !handled || reply == nil || len(reply.Attachments) != 1 {
		t.Fatalf("compressed correction image missing: reply=%#v handled=%v err=%v", reply, handled, err)
	}
	if got := reply.Attachments[0].Name; got != "批改后的作业.jpg" {
		t.Fatalf("JPEG attachment filename = %q, want matching .jpg", got)
	}
}

func TestMaybeHandleK12DingtalkPhoto_DefaultTutorDoesNotStealUnboundPicture(t *testing.T) {
	router := k12PhotoTestRouter(t, false, "k12-tutor")
	calls := 0
	process := func(_ context.Context, _ k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		calls++
		return k12usecase.PhotoGradeResult{}, nil
	}
	reply, handled, err := maybeHandleK12DingtalkPhoto(context.Background(), k12PhotoTestMessage(), router, process)
	if err != nil || handled || reply != nil || calls != 0 {
		t.Fatalf("default route must not steal an unbound DingTalk picture: reply=%#v handled=%v calls=%d err=%v", reply, handled, calls, err)
	}
}

func TestMaybeHandleK12DingtalkPhoto_OnlyExactScenarioAndDingtalkImage(t *testing.T) {
	tests := []struct {
		name     string
		scenario string
		mutate   func(*adapter.Message)
	}{
		{name: "non k12 explicitly bound agent", scenario: "general"},
		{name: "legacy ambiguous k12 marker", scenario: "k12"},
		{name: "other platform", scenario: "k12-tutor", mutate: func(m *adapter.Message) { m.Platform = adapter.PlatformFeishu }},
		{name: "text only", scenario: "k12-tutor", mutate: func(m *adapter.Message) { m.Attachments = nil }},
		{name: "non image attachment", scenario: "k12-tutor", mutate: func(m *adapter.Message) { m.Attachments[0].Type = "file" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := k12PhotoTestRouter(t, true, tt.scenario)
			msg := k12PhotoTestMessage()
			if tt.mutate != nil {
				tt.mutate(msg)
			}
			calls := 0
			process := func(_ context.Context, _ k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
				calls++
				return k12usecase.PhotoGradeResult{}, nil
			}
			reply, handled, err := maybeHandleK12DingtalkPhoto(context.Background(), msg, router, process)
			if err != nil || handled || reply != nil || calls != 0 {
				t.Fatalf("unmatched message should fall through: reply=%#v handled=%v calls=%d err=%v", reply, handled, calls, err)
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
			"scenario":           scenario,
			k12.MetaKeyGradeTerm: "五年级下",
		}},
	}
	var rules []agentrouter.Rule
	if explicit {
		rules = []agentrouter.Rule{{
			Platform: "dingtalk", InstanceID: "bot-1", ChatID: "family-group", AgentName: "child-tutor",
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
	return &adapter.Message{
		ID:         "msg-1",
		Platform:   adapter.PlatformDingtalk,
		InstanceID: "bot-1",
		ChatID:     "family-group",
		UserID:     "parent-1",
		Attachments: []adapter.Attachment{{
			Type: "image", Name: "homework.jpg", Mime: "image/jpeg",
			Data: base64.StdEncoding.EncodeToString([]byte("image")),
		}},
	}
}
