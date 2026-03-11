// Package desktop 提供桌面客户端集成能力
//
// 为 HexClaw 桌面应用提供后端支持，包括：
//   - 系统托盘通知推送
//   - 剪贴板内容读写
//   - 文件拖放上传到知识库
//   - 快捷键事件接收
//   - 本地系统信息获取
//
// 桌面客户端（Wails/Tauri）通过 REST API 与本模块交互。
//
// 对标 OpenClaw Desktop Mode。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// NotificationType 通知类型
type NotificationType string

const (
	NotifyInfo    NotificationType = "info"    // 普通信息
	NotifyWarning NotificationType = "warning" // 警告
	NotifyError   NotificationType = "error"   // 错误
	NotifySuccess NotificationType = "success" // 成功
)

// Notification 桌面通知
type Notification struct {
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	Type      NotificationType `json:"type"`
	Timestamp time.Time        `json:"timestamp"`
}

// SystemInfo 系统信息
type SystemInfo struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"` // HexClaw 版本
}

// Service 桌面集成服务
//
// 管理通知队列、剪贴板操作等桌面特有功能。
// 通过 HTTP API 与桌面客户端通信。
type Service struct {
	mu            sync.RWMutex
	notifications []Notification  // 通知队列
	maxNotify     int             // 最大通知保留数
	version       string          // HexClaw 版本
	onNotify      func(n Notification) // 通知回调（可选）
}

// NewService 创建桌面集成服务
func NewService(version string) *Service {
	return &Service{
		maxNotify: 100,
		version:   version,
	}
}

// SetNotifyCallback 设置通知回调
//
// 桌面客户端可注册回调，在新通知到达时触发系统通知。
func (s *Service) SetNotifyCallback(fn func(n Notification)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onNotify = fn
}

// Notify 发送桌面通知
//
// 将通知添加到队列，同时触发回调（如果设置）。
func (s *Service) Notify(title, body string, notifyType NotificationType) {
	n := Notification{
		ID:        fmt.Sprintf("notif-%d", time.Now().UnixNano()),
		Title:     title,
		Body:      body,
		Type:      notifyType,
		Timestamp: time.Now(),
	}

	s.mu.Lock()
	s.notifications = append(s.notifications, n)
	// 保持队列大小
	if len(s.notifications) > s.maxNotify {
		s.notifications = s.notifications[len(s.notifications)-s.maxNotify:]
	}
	callback := s.onNotify
	s.mu.Unlock()

	if callback != nil {
		callback(n)
	}

	// 尝试系统通知
	sendSystemNotification(title, body)
}

// Notifications 获取最近的通知列表
func (s *Service) Notifications(limit int) []Notification {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > len(s.notifications) {
		limit = len(s.notifications)
	}

	// 返回最近的通知（从末尾取）
	start := len(s.notifications) - limit
	result := make([]Notification, limit)
	copy(result, s.notifications[start:])
	return result
}

// ClearNotifications 清空通知
func (s *Service) ClearNotifications() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifications = nil
}

// GetSystemInfo 获取系统信息
func (s *Service) GetSystemInfo() SystemInfo {
	hostname := "unknown"
	if h, err := exec.Command("hostname").Output(); err == nil {
		hostname = strings.TrimSpace(string(h))
	}

	return SystemInfo{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Hostname: hostname,
		Version:  s.version,
	}
}

// GetClipboard 读取系统剪贴板内容
//
// 支持 macOS (pbpaste)、Linux (xclip)、Windows (powershell)。
func (s *Service) GetClipboard(ctx context.Context) (string, error) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "pbpaste")
	case "linux":
		cmd = exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-o")
	case "windows":
		cmd = exec.CommandContext(ctx, "powershell", "-command", "Get-Clipboard")
	default:
		return "", fmt.Errorf("不支持的操作系统: %s", runtime.GOOS)
	}

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("读取剪贴板失败: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// SetClipboard 写入系统剪贴板
//
// 支持 macOS (pbcopy)、Linux (xclip)、Windows (clip)。
func (s *Service) SetClipboard(ctx context.Context, content string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "pbcopy")
	case "linux":
		cmd = exec.CommandContext(ctx, "xclip", "-selection", "clipboard")
	case "windows":
		cmd = exec.CommandContext(ctx, "clip")
	default:
		return fmt.Errorf("不支持的操作系统: %s", runtime.GOOS)
	}

	cmd.Stdin = strings.NewReader(content)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("写入剪贴板失败: %w", err)
	}
	return nil
}

// RegisterRoutes 注册桌面 API 路由
//
// 提供以下端点：
//   - GET  /api/v1/desktop/info          系统信息
//   - GET  /api/v1/desktop/notifications  通知列表
//   - POST /api/v1/desktop/notifications  发送通知
//   - DELETE /api/v1/desktop/notifications 清空通知
//   - GET  /api/v1/desktop/clipboard      读取剪贴板
//   - POST /api/v1/desktop/clipboard      写入剪贴板
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/desktop/info", s.handleInfo)
	mux.HandleFunc("GET /api/v1/desktop/notifications", s.handleGetNotifications)
	mux.HandleFunc("POST /api/v1/desktop/notifications", s.handlePostNotification)
	mux.HandleFunc("DELETE /api/v1/desktop/notifications", s.handleClearNotifications)
	mux.HandleFunc("GET /api/v1/desktop/clipboard", s.handleGetClipboard)
	mux.HandleFunc("POST /api/v1/desktop/clipboard", s.handleSetClipboard)
}

// ============== HTTP Handlers ==============

func (s *Service) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.GetSystemInfo())
}

func (s *Service) handleGetNotifications(w http.ResponseWriter, _ *http.Request) {
	notifications := s.Notifications(50)
	writeJSON(w, http.StatusOK, map[string]any{
		"notifications": notifications,
		"total":         len(notifications),
	})
}

func (s *Service) handlePostNotification(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string           `json:"title"`
		Body  string           `json:"body"`
		Type  NotificationType `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title 不能为空"})
		return
	}
	if req.Type == "" {
		req.Type = NotifyInfo
	}

	s.Notify(req.Title, req.Body, req.Type)
	writeJSON(w, http.StatusOK, map[string]string{"message": "通知已发送"})
}

func (s *Service) handleClearNotifications(w http.ResponseWriter, _ *http.Request) {
	s.ClearNotifications()
	writeJSON(w, http.StatusOK, map[string]string{"message": "通知已清空"})
}

func (s *Service) handleGetClipboard(w http.ResponseWriter, r *http.Request) {
	content, err := s.GetClipboard(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": content})
}

func (s *Service) handleSetClipboard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if err := s.SetClipboard(r.Context(), req.Content); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "已复制到剪贴板"})
}

// writeJSON 写入 JSON 响应
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// sendSystemNotification 发送系统级通知
//
// macOS 使用 osascript，Linux 使用 notify-send。
func sendSystemNotification(title, body string) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display notification "%s" with title "%s"`, body, title)
		cmd = exec.Command("osascript", "-e", script)
	case "linux":
		cmd = exec.Command("notify-send", title, body)
	default:
		return // Windows 等平台暂不支持系统通知
	}

	if err := cmd.Start(); err != nil {
		log.Printf("发送系统通知失败: %v", err)
	}
}
