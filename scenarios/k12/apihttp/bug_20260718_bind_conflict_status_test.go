package apihttp_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
)

type conflictBinder struct{}

func (conflictBinder) Bind(context.Context, string, string, string, string) error {
	return fmt.Errorf("%w: 这个私聊已经在接收「小明助手」的消息：一个私聊同时只能接收一个孩子的助手，请先解绑「小明助手」再绑定「小王助手」",
		apihttp.ErrBindConflict)
}

// Bug 2026-07-18（真实环境回归矩阵 C2 发现）：
// 限绑拒绝（§3.12 业务裁决「一个私聊同时只能接收一个孩子的助手」）此前被 bindIM
// 一刀切包装成 HTTP 500——业务冲突不是服务器故障。
// 契约：绑定冲突 → 409 Conflict + 家长向文案原样透传；真正的内部错误仍 500。
func TestBug20260718_BindConflictIs409(t *testing.T) {
	h := newServerWithBinder(t, conflictBinder{})
	rec, out := do(t, h, "POST", "/bind-im",
		`{"agent":"mingming","platform":"dingtalk","instance_id":"pi-x","chat_id":"c1"}`)
	if rec.Code != 409 {
		t.Fatalf("限绑冲突应 409（业务裁决非服务器故障），got %d body=%v", rec.Code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "一个私聊同时只能接收一个孩子的助手") {
		t.Errorf("§3.12 家长向文案应原样透传, got %q", msg)
	}
}

type brokenBinder struct{}

func (brokenBinder) Bind(context.Context, string, string, string, string) error {
	return fmt.Errorf("db locked")
}

// 真内部错误仍 500（不因 409 映射而放宽）。
func TestBug20260718_BindInternalErrorStays500(t *testing.T) {
	h := newServerWithBinder(t, brokenBinder{})
	rec, _ := do(t, h, "POST", "/bind-im",
		`{"agent":"mingming","platform":"dingtalk","instance_id":"pi-x","chat_id":"c1"}`)
	if rec.Code != 500 {
		t.Fatalf("内部错误应 500, got %d", rec.Code)
	}
}
