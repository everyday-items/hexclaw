// Package webhook 提供外部事件触发能力
//
// 让外部系统（GitHub、GitLab、通用 JSON）通过 HTTP Webhook
// 触发 Agent 工作。每个 Webhook 绑定一个处理指令，
// Agent 根据指令和 payload 自动处理事件。
//
// 安全措施：
//   - HMAC-SHA256 签名验证
//   - 每个 Webhook 独立 Secret
//   - 请求体大小限制（1MB）
//
// 对标 OpenClaw 的 Webhooks 机制。
package webhook

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/crypto/sign"
	"github.com/hexagon-codes/toolkit/util/logger"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const maxPayloadSize = 1 << 20 // 1MB

// WebhookType 预置 Webhook 类型
type WebhookType string

const (
	TypeGeneric WebhookType = "generic" // 通用 JSON
	TypeGitHub  WebhookType = "github"  // GitHub Events
	TypeGitLab  WebhookType = "gitlab"  // GitLab Events
)

// Webhook 配置
type Webhook struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`             // 名称（也是 URL 路径）
	Type        WebhookType `json:"type"`             // 类型
	Secret      string      `json:"-"`                // 签名验证 Secret（JSON 序列化时隐藏）
	HasSecret   bool        `json:"has_secret"`       // 是否配置了 Secret
	Prompt      string      `json:"prompt"`           // Agent 处理指令（JobID 为空时跑此 prompt）
	JobID       string      `json:"job_id,omitempty"` // §13.3(1) 非空 → 触发指定 cron job 而非跑 prompt
	UserID      string      `json:"user_id"`          // 所属用户
	Enabled     bool        `json:"enabled"`          // 是否启用
	LastEventAt time.Time   `json:"last_event_at"`
	EventCount  int         `json:"event_count"`
	CreatedAt   time.Time   `json:"created_at"`
}

// Event Webhook 接收到的事件
type Event struct {
	WebhookID   string         `json:"webhook_id"`
	WebhookName string         `json:"webhook_name"`
	Type        WebhookType    `json:"type"`
	EventType   string         `json:"event_type"`       // 事件类型（如 push, pull_request）
	Payload     map[string]any `json:"payload"`          // 原始 payload
	Summary     string         `json:"summary"`          // 解析后的摘要
	JobID       string         `json:"job_id,omitempty"` // §13.3(1) 随 webhook 配置带下来的目标 job（非空 → 触发 job）
	ReceivedAt  time.Time      `json:"received_at"`
}

// EventHandler 事件处理回调
//
// 接收解析后的事件和 Webhook 的处理指令，
// 返回 Agent 的处理结果。
type EventHandler func(ctx context.Context, event *Event, prompt string) error

// Manager Webhook 管理器
//
// 管理 Webhook 注册、接收和分发。
// 提供 HTTP Handler 挂载到 API 路由。
type Manager struct {
	mu       sync.RWMutex
	db       *sql.DB
	webhooks map[string]*Webhook // name -> webhook
	handler  EventHandler
}

// NewManager 创建 Webhook 管理器
func NewManager(db *sql.DB) *Manager {
	return &Manager{
		db:       db,
		webhooks: make(map[string]*Webhook),
	}
}

// Init 初始化 Webhook 存储表
func (m *Manager) Init(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS webhooks (
		id TEXT PRIMARY KEY,
		name TEXT UNIQUE NOT NULL,
		type TEXT NOT NULL DEFAULT 'generic',
		secret TEXT DEFAULT '',
		prompt TEXT NOT NULL,
		user_id TEXT NOT NULL,
		enabled INTEGER DEFAULT 1,
		last_event_at DATETIME,
		event_count INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		job_id TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return fmt.Errorf("初始化 webhook 表失败: %w", err)
	}

	// §13.3(1) 兼容升级：旧库补 job_id 列。已存在时报 "duplicate column name"，预期忽略。
	if _, aerr := m.db.ExecContext(ctx, `ALTER TABLE webhooks ADD COLUMN job_id TEXT NOT NULL DEFAULT ''`); aerr != nil && !strings.Contains(aerr.Error(), "duplicate column") {
		logger.Warn("Webhook: 添加 job_id 列失败（非 duplicate）", "err", aerr.Error())
	}

	return m.loadWebhooks(ctx)
}

// SetHandler 设置事件处理回调
func (m *Manager) SetHandler(handler EventHandler) {
	m.mu.Lock()
	m.handler = handler
	m.mu.Unlock()
}

// Register 注册新 Webhook
func (m *Manager) Register(ctx context.Context, wh *Webhook) error {
	if wh.ID == "" {
		wh.ID = "wh-" + idgen.ShortID()
	}
	if wh.Type == "" {
		wh.Type = TypeGeneric
	}
	if wh.CreatedAt.IsZero() {
		wh.CreatedAt = time.Now()
	}
	wh.Enabled = true

	_, err := m.db.ExecContext(ctx,
		`INSERT INTO webhooks (id, name, type, secret, prompt, user_id, enabled, created_at, job_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		wh.ID, wh.Name, wh.Type, wh.Secret, wh.Prompt, wh.UserID, 1, wh.CreatedAt, wh.JobID,
	)
	if err != nil {
		return fmt.Errorf("注册 webhook 失败: %w", err)
	}

	m.mu.Lock()
	m.webhooks[wh.Name] = wh
	m.mu.Unlock()

	logger.Info("Webhook 已注册", "name", wh.Name, "type", wh.Type)
	return nil
}

// Unregister 注销 Webhook
func (m *Manager) Unregister(ctx context.Context, name string) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM webhooks WHERE name = ?`, name)
	if err != nil {
		return err
	}

	m.mu.Lock()
	delete(m.webhooks, name)
	m.mu.Unlock()
	return nil
}

// List 列出所有 Webhook
func (m *Manager) List(ctx context.Context, userID string) ([]*Webhook, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, name, type, secret, prompt, user_id, enabled, last_event_at, event_count, created_at, job_id
		 FROM webhooks WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var webhooks []*Webhook
	for rows.Next() {
		wh := &Webhook{}
		var lastEvent sql.NullTime
		var enabled int
		if err := rows.Scan(&wh.ID, &wh.Name, &wh.Type, &wh.Secret, &wh.Prompt,
			&wh.UserID, &enabled, &lastEvent, &wh.EventCount, &wh.CreatedAt, &wh.JobID); err != nil {
			return nil, err
		}
		wh.Enabled = enabled == 1
		wh.HasSecret = wh.Secret != ""
		if lastEvent.Valid {
			wh.LastEventAt = lastEvent.Time
		}
		webhooks = append(webhooks, wh)
	}
	return webhooks, rows.Err()
}

// Handler 返回处理 Webhook 请求的 HTTP Handler
//
// 路由格式：/api/v1/webhooks/{name}
func (m *Manager) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "webhook name required", http.StatusBadRequest)
			return
		}

		// 查找 webhook
		m.mu.RLock()
		wh, ok := m.webhooks[name]
		m.mu.RUnlock()

		if !ok || !wh.Enabled {
			http.Error(w, "webhook not found", http.StatusNotFound)
			return
		}

		// 读取请求体
		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadSize))
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}

		// 签名验证
		if wh.Secret != "" {
			if !m.verifySignature(wh, r, body) {
				logger.Error("Webhook", "name", name)
				http.Error(w, "signature verification failed", http.StatusUnauthorized)
				return
			}
		}

		// 解析事件
		event, err := m.parseEvent(wh, r, body)
		if err != nil {
			logger.Error("Webhook", "name", name, "error", err)
			http.Error(w, "parse event failed", http.StatusBadRequest)
			return
		}

		// 更新统计
		now := time.Now()
		if _, err := m.db.ExecContext(r.Context(),
			`UPDATE webhooks SET last_event_at = ?, event_count = event_count + 1 WHERE id = ?`,
			now, wh.ID); err != nil {
			logger.Error("Webhook: 更新统计失败", "error", err)
		}
		m.mu.Lock()
		wh.LastEventAt = now
		wh.EventCount++
		m.mu.Unlock()

		// 异步处理事件
		m.mu.RLock()
		handler := m.handler
		m.mu.RUnlock()

		if handler != nil {
			// §13.3(1)：把 webhook 绑定的目标 job 随 event 带给 handler（非空 →
			// handler 触发该 job 而非跑 prompt）。EventHandler 签名不变。
			event.JobID = wh.JobID
			// v0.3.12 H7：用 trace.Go 保留父 ctx 的 logger 链路（session_id / trace_id）
			// + panic recover（防止 handler 崩了静默吞）。
			// 父 ctx 来自 HTTP r.Context()；Detach 断开父取消但保留 Values。
			trace.Go(r.Context(), "webhook.handle-"+name, func(ctx context.Context) {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				if err := handler(ctx, event, wh.Prompt); err != nil {
					logger.Error("Webhook", "name", name, "处理事件失败", err)
				}
			})
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"accepted"}`))
	}
}

// --- 内部方法 ---

// verifySignature 验证 Webhook 签名
func (m *Manager) verifySignature(wh *Webhook, r *http.Request, body []byte) bool {
	switch wh.Type {
	case TypeGitHub:
		// GitHub 使用 X-Hub-Signature-256 头
		sig := r.Header.Get("X-Hub-Signature-256")
		if sig == "" {
			return false
		}
		sig = strings.TrimPrefix(sig, "sha256=")
		expected := sign.HMACSHA256Hex(body, []byte(wh.Secret))
		return hmac.Equal([]byte(sig), []byte(expected))

	case TypeGitLab:
		// GitLab 使用 X-Gitlab-Token 头（使用常量时间比较防止 timing attack）
		token := r.Header.Get("X-Gitlab-Token")
		return hmac.Equal([]byte(token), []byte(wh.Secret))

	default:
		// 通用：X-Webhook-Signature 或 X-Signature
		sig := r.Header.Get("X-Webhook-Signature")
		if sig == "" {
			sig = r.Header.Get("X-Signature")
		}
		if sig == "" {
			return false
		}
		sig = strings.TrimPrefix(sig, "sha256=")
		expected := sign.HMACSHA256Hex(body, []byte(wh.Secret))
		return hmac.Equal([]byte(sig), []byte(expected))
	}
}

// parseEvent 解析 Webhook 事件
func (m *Manager) parseEvent(wh *Webhook, r *http.Request, body []byte) (*Event, error) {
	event := &Event{
		WebhookID:   wh.ID,
		WebhookName: wh.Name,
		Type:        wh.Type,
		ReceivedAt:  time.Now(),
	}

	// 解析 JSON payload
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		// 非 JSON，存为原始文本
		payload = map[string]any{"raw": string(body)}
	}
	event.Payload = payload

	// 根据类型解析事件类型和摘要
	switch wh.Type {
	case TypeGitHub:
		event.EventType = r.Header.Get("X-GitHub-Event")
		event.Summary = parseGitHubSummary(event.EventType, payload)
	case TypeGitLab:
		if objKind, ok := payload["object_kind"].(string); ok {
			event.EventType = objKind
		}
		event.Summary = fmt.Sprintf("GitLab %s 事件", event.EventType)
	default:
		event.EventType = r.Header.Get("X-Event-Type")
		if event.EventType == "" {
			event.EventType = "generic"
		}
		event.Summary = fmt.Sprintf("收到 %s Webhook 事件", wh.Name)
	}

	return event, nil
}

// parseGitHubSummary 解析 GitHub 事件摘要
func parseGitHubSummary(eventType string, payload map[string]any) string {
	switch eventType {
	case "push":
		ref, _ := payload["ref"].(string)
		repo := getNestedString(payload, "repository", "full_name")
		commits, _ := payload["commits"].([]any)
		return fmt.Sprintf("Push to %s (%s): %d commit(s)", repo, ref, len(commits))

	case "pull_request":
		action, _ := payload["action"].(string)
		title := getNestedString(payload, "pull_request", "title")
		repo := getNestedString(payload, "repository", "full_name")
		return fmt.Sprintf("PR %s in %s: %s", action, repo, title)

	case "issues":
		action, _ := payload["action"].(string)
		title := getNestedString(payload, "issue", "title")
		return fmt.Sprintf("Issue %s: %s", action, title)

	default:
		return fmt.Sprintf("GitHub %s 事件", eventType)
	}
}

// getNestedString 从嵌套 map 中获取字符串值
func getNestedString(m map[string]any, keys ...string) string {
	current := m
	for i, key := range keys {
		if i == len(keys)-1 {
			if v, ok := current[key].(string); ok {
				return v
			}
			return ""
		}
		if next, ok := current[key].(map[string]any); ok {
			current = next
		} else {
			return ""
		}
	}
	return ""
}

// loadWebhooks 从数据库加载所有启用的 webhook
func (m *Manager) loadWebhooks(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, name, type, secret, prompt, user_id, enabled, last_event_at, event_count, created_at, job_id
		 FROM webhooks WHERE enabled = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		wh := &Webhook{}
		var lastEvent sql.NullTime
		var enabled int
		if err := rows.Scan(&wh.ID, &wh.Name, &wh.Type, &wh.Secret, &wh.Prompt,
			&wh.UserID, &enabled, &lastEvent, &wh.EventCount, &wh.CreatedAt, &wh.JobID); err != nil {
			return err
		}
		wh.Enabled = enabled == 1
		wh.HasSecret = wh.Secret != ""
		if lastEvent.Valid {
			wh.LastEventAt = lastEvent.Time
		}
		m.webhooks[wh.Name] = wh
	}

	logger.Info("Webhook 已加载", "len", len(m.webhooks))
	return rows.Err()
}
