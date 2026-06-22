package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 回归测试 — C10（2026-06-22 hex-test 审计）：
// handleShareAgent / handleInviteTeamMember 此前对 visibility/role 不做枚举校验，
// 任意字符串原样落库，破坏前端 union 类型（team.ts: visibility public/team/private、
// role admin/member/viewer）。本测试钉死"非法枚举必须 400"。
func TestHandleShareAgent_C10_RejectInvalidVisibility(t *testing.T) {
	s := &Server{teamStore: NewTeamStore(t.TempDir())}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/team/agents",
		strings.NewReader(`{"name":"x","visibility":"__bogus__"}`))
	rec := httptest.NewRecorder()
	s.handleShareAgent(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 visibility 应返回 400，实际=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleShareAgent_C10_AcceptValidVisibility(t *testing.T) {
	s := &Server{teamStore: NewTeamStore(t.TempDir())}
	for _, v := range []string{"public", "team", "private"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/team/agents",
			strings.NewReader(`{"name":"x","visibility":"`+v+`"}`))
		rec := httptest.NewRecorder()
		s.handleShareAgent(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("合法 visibility %q 应 200，实际=%d", v, rec.Code)
		}
	}
}

func TestHandleInviteTeamMember_C10_RejectInvalidRole(t *testing.T) {
	s := &Server{teamStore: NewTeamStore(t.TempDir())}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/team/members",
		strings.NewReader(`{"email":"a@b.com","role":"__bogus__"}`))
	rec := httptest.NewRecorder()
	s.handleInviteTeamMember(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 role 应返回 400，实际=%d body=%s", rec.Code, rec.Body.String())
	}
}
