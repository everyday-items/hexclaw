package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type captureInstanceReplySender struct {
	ctx    context.Context
	target string
	chatID string
	reply  *adapter.Reply
}

func (s *captureInstanceReplySender) Send(ctx context.Context, target, chatID string, reply *adapter.Reply) error {
	s.ctx = ctx
	s.target = target
	s.chatID = chatID
	s.reply = reply
	return nil
}

func TestBUG20260715_InstanceMessageSenderForwardsAttachments(t *testing.T) {
	capture := &captureInstanceReplySender{}
	sender := &instanceMessageSender{mgr: capture}
	attachments := []adapter.Attachment{{
		Type: "image",
		Name: "graded-homework.png",
		Mime: "image/png",
		Data: "iVBORw0KGgo=",
	}}

	err := sender.Send(context.Background(), "dingtalk", "conversation-1", "批改完成", attachments)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if capture.target != "dingtalk" || capture.chatID != "conversation-1" {
		t.Fatalf("routing was not forwarded: target=%q chatID=%q", capture.target, capture.chatID)
	}
	if capture.reply == nil {
		t.Fatal("manager received nil reply")
	}
	if capture.reply.Content != "批改完成" {
		t.Fatalf("content = %q, want %q", capture.reply.Content, "批改完成")
	}
	if !reflect.DeepEqual(capture.reply.Attachments, attachments) {
		t.Fatalf("attachments = %#v, want %#v", capture.reply.Attachments, attachments)
	}
}
