package desktop

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewService 测试创建服务
func TestNewService(t *testing.T) {
	svc := NewService("v1.0.0")
	if svc == nil {
		t.Fatal("服务不应为 nil")
	}
	if svc.version != "v1.0.0" {
		t.Errorf("版本不匹配: %q", svc.version)
	}
}

// TestNotify 测试发送通知
func TestNotify(t *testing.T) {
	svc := NewService("test")

	svc.Notify("测试标题", "测试内容", NotifyInfo)
	svc.Notify("警告", "something wrong", NotifyWarning)

	notifications := svc.Notifications(10)
	if len(notifications) != 2 {
		t.Fatalf("应有 2 条通知，实际 %d", len(notifications))
	}
	if notifications[0].Title != "测试标题" {
		t.Errorf("标题不匹配: %q", notifications[0].Title)
	}
	if notifications[1].Type != NotifyWarning {
		t.Errorf("类型不匹配: %q", notifications[1].Type)
	}
}

// TestNotify_Callback 测试通知回调
func TestNotify_Callback(t *testing.T) {
	svc := NewService("test")

	var received Notification
	svc.SetNotifyCallback(func(n Notification) {
		received = n
	})

	svc.Notify("回调测试", "内容", NotifySuccess)

	if received.Title != "回调测试" {
		t.Errorf("回调未触发或标题不匹配: %q", received.Title)
	}
}

// TestNotifications_Limit 测试通知数量限制
func TestNotifications_Limit(t *testing.T) {
	svc := NewService("test")
	svc.maxNotify = 5

	for i := 0; i < 10; i++ {
		svc.Notify("title", "body", NotifyInfo)
	}

	all := svc.Notifications(0)
	if len(all) != 5 {
		t.Errorf("应最多保留 5 条，实际 %d", len(all))
	}

	// 限制返回数量
	limited := svc.Notifications(2)
	if len(limited) != 2 {
		t.Errorf("应返回 2 条，实际 %d", len(limited))
	}
}

// TestClearNotifications 测试清空通知
func TestClearNotifications(t *testing.T) {
	svc := NewService("test")
	svc.Notify("title", "body", NotifyInfo)
	svc.ClearNotifications()

	if len(svc.Notifications(0)) != 0 {
		t.Error("清空后应为空")
	}
}

// TestGetSystemInfo 测试获取系统信息
func TestGetSystemInfo(t *testing.T) {
	svc := NewService("v1.0.0-test")
	info := svc.GetSystemInfo()

	if info.OS == "" {
		t.Error("OS 不应为空")
	}
	if info.Arch == "" {
		t.Error("Arch 不应为空")
	}
	if info.Version != "v1.0.0-test" {
		t.Errorf("版本不匹配: %q", info.Version)
	}
}

// TestHandleInfo 测试系统信息 API
func TestHandleInfo(t *testing.T) {
	svc := NewService("v1.0.0")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/desktop/info", nil)
	w := httptest.NewRecorder()

	svc.handleInfo(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("状态码不匹配: %d", w.Code)
	}

	var info SystemInfo
	json.NewDecoder(w.Body).Decode(&info)
	if info.Version != "v1.0.0" {
		t.Errorf("版本不匹配: %q", info.Version)
	}
}

// TestHandleNotifications 测试通知 API
func TestHandleNotifications(t *testing.T) {
	svc := NewService("test")
	svc.Notify("test", "body", NotifyInfo)

	// GET 通知列表
	req := httptest.NewRequest(http.MethodGet, "/api/v1/desktop/notifications", nil)
	w := httptest.NewRecorder()
	svc.handleGetNotifications(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("状态码不匹配: %d", w.Code)
	}

	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	total := result["total"].(float64)
	if total != 1 {
		t.Errorf("应有 1 条通知，实际 %.0f", total)
	}
}

// TestHandlePostNotification 测试发送通知 API
func TestHandlePostNotification(t *testing.T) {
	svc := NewService("test")

	body := `{"title":"API 通知","body":"from API","type":"success"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/desktop/notifications", strings.NewReader(body))
	w := httptest.NewRecorder()
	svc.handlePostNotification(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("状态码不匹配: %d", w.Code)
	}

	notifications := svc.Notifications(0)
	if len(notifications) != 1 {
		t.Fatalf("应有 1 条通知，实际 %d", len(notifications))
	}
	if notifications[0].Type != NotifySuccess {
		t.Errorf("类型不匹配: %q", notifications[0].Type)
	}
}

// TestHandlePostNotification_EmptyTitle 测试空标题
func TestHandlePostNotification_EmptyTitle(t *testing.T) {
	svc := NewService("test")

	body := `{"title":"","body":"no title"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/desktop/notifications", strings.NewReader(body))
	w := httptest.NewRecorder()
	svc.handlePostNotification(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("空标题应返回 400，实际 %d", w.Code)
	}
}

// TestHandleClearNotifications 测试清空通知 API
func TestHandleClearNotifications(t *testing.T) {
	svc := NewService("test")
	svc.Notify("test", "body", NotifyInfo)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/desktop/notifications", nil)
	w := httptest.NewRecorder()
	svc.handleClearNotifications(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("状态码不匹配: %d", w.Code)
	}
	if len(svc.Notifications(0)) != 0 {
		t.Error("清空后应为空")
	}
}
