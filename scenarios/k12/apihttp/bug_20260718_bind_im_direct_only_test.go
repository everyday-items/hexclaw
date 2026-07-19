package apihttp_test

// BUG-20260718（测试验收清单 §15 / SPEC-006/010 / REG-CONTRACT-001）：后端 bind-im
// 仍带「家庭群/群绑定」旧契约，与架构 §4.11「仅一对一私聊，不接受群 conversation
// 类型」、§6.10「适配器收到群消息必须忽略」矛盾。ChannelBinding.conversation_scope
// 当前只能是 direct（架构 §5.x）。此测试钉死 bind-im 只接受 direct 私聊、显式拒绝群
// conversation 类型（RED 先行：修复前无门禁，群类型照绑）。

import (
	"net/http"
	"testing"
)

func TestBug20260718_BindIMRejectsGroupConversation(t *testing.T) {
	for _, ct := range []string{"2", "group", "GROUP", "multi"} {
		b := &fakeBinder{}
		h := newServerWithBinder(t, b)
		rec, _ := do(t, h, "POST", "/bind-im",
			`{"agent":"mingming","platform":"dingtalk","instance_id":"inst","chat_id":"c1","conversation_type":"`+ct+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("conversation_type=%q（群）必须拒绝为 400, got %d", ct, rec.Code)
		}
		if b.calls != 0 {
			t.Fatalf("conversation_type=%q（群）不得落绑定, binder 被调用 %d 次", ct, b.calls)
		}
	}
}

func TestBug20260718_BindIMAcceptsDirect(t *testing.T) {
	// 显式 direct 与缺省（历史前端不带该字段）都必须照常放行（向后兼容）。
	for _, body := range []string{
		`{"agent":"mingming","platform":"dingtalk","instance_id":"inst","chat_id":"c1","conversation_type":"1"}`,
		`{"agent":"mingming","platform":"dingtalk","instance_id":"inst","chat_id":"c1","conversation_type":"direct"}`,
		`{"agent":"mingming","platform":"dingtalk","instance_id":"inst","chat_id":"c1"}`,
	} {
		b := &fakeBinder{}
		h := newServerWithBinder(t, b)
		rec, _ := do(t, h, "POST", "/bind-im", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("direct 私聊绑定必须放行, got %d (body=%s)", rec.Code, body)
		}
		if b.calls != 1 {
			t.Fatalf("direct 绑定应落一次, got %d", b.calls)
		}
	}
}
