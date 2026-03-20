package instances

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/adapter/dingtalk"
	"github.com/hexagon-codes/hexclaw/adapter/discord"
	"github.com/hexagon-codes/hexclaw/adapter/feishu"
	"github.com/hexagon-codes/hexclaw/adapter/line"
	"github.com/hexagon-codes/hexclaw/adapter/matrix"
	"github.com/hexagon-codes/hexclaw/adapter/slack"
	"github.com/hexagon-codes/hexclaw/adapter/telegram"
	"github.com/hexagon-codes/hexclaw/adapter/wechat"
	"github.com/hexagon-codes/hexclaw/adapter/wecom"
	"github.com/hexagon-codes/hexclaw/adapter/whatsapp"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

type Status string

const (
	StatusStopped Status = "stopped"
	StatusRunning Status = "running"
	StatusError   Status = "error"
)

type Instance struct {
	ID          string          `json:"id"`
	Provider    string          `json:"provider"`
	Name        string          `json:"name"`
	Enabled     bool            `json:"enabled"`
	Mode        string          `json:"mode"`
	Status      Status          `json:"status"`
	Config      json.RawMessage `json:"config"`
	LastEventAt time.Time       `json:"last_event_at,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type HealthReport struct {
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Mode        string    `json:"mode"`
	Status      Status    `json:"status"`
	Healthy     bool      `json:"healthy"`
	LastEventAt time.Time `json:"last_event_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

type Manager struct {
	db      *sql.DB
	handler adapter.MessageHandler

	mu       sync.RWMutex
	running  map[string]adapter.Adapter
	inbound  map[string]http.Handler
	metadata map[string]*Instance
}

func NewManager(db *sql.DB) *Manager {
	return &Manager{
		db:       db,
		running:  make(map[string]adapter.Adapter),
		inbound:  make(map[string]http.Handler),
		metadata: make(map[string]*Instance),
	}
}

func (m *Manager) SetHandler(h adapter.MessageHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handler = h
}

func (m *Manager) Init(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS platform_instances (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			name TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1,
			mode TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'stopped',
			config_json TEXT NOT NULL DEFAULT '{}',
			last_event_at DATETIME,
			last_error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS platform_events (
			instance_name TEXT NOT NULL,
			event_id TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (instance_name, event_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_platform_instances_provider ON platform_instances(provider)`,
		`CREATE INDEX IF NOT EXISTS idx_platform_events_created_at ON platform_events(created_at)`,
	}
	for _, stmt := range stmts {
		if _, err := m.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) SeedFromConfig(ctx context.Context, cfg *config.Config) error {
	var count int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM platform_instances`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	instances, err := instancesFromConfig(cfg)
	if err != nil {
		return err
	}
	for i := range instances {
		if err := m.Upsert(ctx, &instances[i]); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) List(ctx context.Context) ([]*Instance, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, provider, name, enabled, mode, status, config_json, last_event_at, last_error, created_at, updated_at
		 FROM platform_instances ORDER BY provider, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*Instance
	for rows.Next() {
		inst := &Instance{}
		var enabled int
		var configJSON string
		var lastEvent sql.NullTime
		if err := rows.Scan(&inst.ID, &inst.Provider, &inst.Name, &enabled, &inst.Mode, &inst.Status, &configJSON, &lastEvent, &inst.LastError, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
			return nil, err
		}
		inst.Enabled = enabled == 1
		inst.Config = json.RawMessage(configJSON)
		if lastEvent.Valid {
			inst.LastEventAt = lastEvent.Time
		}
		list = append(list, inst)
	}
	return list, rows.Err()
}

func (m *Manager) Get(ctx context.Context, name string) (*Instance, error) {
	row := m.db.QueryRowContext(ctx,
		`SELECT id, provider, name, enabled, mode, status, config_json, last_event_at, last_error, created_at, updated_at
		 FROM platform_instances WHERE name = ?`,
		name,
	)
	inst := &Instance{}
	var enabled int
	var configJSON string
	var lastEvent sql.NullTime
	if err := row.Scan(&inst.ID, &inst.Provider, &inst.Name, &enabled, &inst.Mode, &inst.Status, &configJSON, &lastEvent, &inst.LastError, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
		return nil, err
	}
	inst.Enabled = enabled == 1
	inst.Config = json.RawMessage(configJSON)
	if lastEvent.Valid {
		inst.LastEventAt = lastEvent.Time
	}
	return inst, nil
}

func (m *Manager) Upsert(ctx context.Context, inst *Instance) error {
	if inst.ID == "" {
		inst.ID = "pi-" + idgen.ShortID()
	}
	if inst.Name == "" || inst.Provider == "" {
		return fmt.Errorf("provider 和 name 不能为空")
	}
	mode, err := modeForProvider(inst.Provider)
	if err != nil {
		return err
	}
	inst.Mode = mode
	now := time.Now()
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = now
	}
	inst.UpdatedAt = now

	_, err = m.db.ExecContext(ctx,
		`INSERT INTO platform_instances (id, provider, name, enabled, mode, status, config_json, last_error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		    provider=excluded.provider,
		    enabled=excluded.enabled,
		    mode=excluded.mode,
		    config_json=excluded.config_json,
		    updated_at=excluded.updated_at`,
		inst.ID, inst.Provider, inst.Name, boolToInt(inst.Enabled), inst.Mode, StatusStopped, string(inst.Config), inst.LastError, inst.CreatedAt, inst.UpdatedAt,
	)
	return err
}

func (m *Manager) Delete(ctx context.Context, name string) error {
	_ = m.Stop(ctx, name)
	_, err := m.db.ExecContext(ctx, `DELETE FROM platform_instances WHERE name = ?`, name)
	if err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.metadata, name)
	delete(m.inbound, name)
	delete(m.running, name)
	m.mu.Unlock()
	return nil
}

func (m *Manager) StartEnabled(ctx context.Context) error {
	instances, err := m.List(ctx)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if inst.Enabled {
			if err := m.Start(ctx, inst.Name); err != nil {
				_ = m.setStatus(ctx, inst.Name, StatusError, err.Error())
			}
		}
	}
	return nil
}

func (m *Manager) Start(ctx context.Context, name string) error {
	inst, err := m.Get(ctx, name)
	if err != nil {
		return err
	}

	m.mu.RLock()
	_, alreadyRunning := m.running[name]
	_, alreadyInbound := m.inbound[name]
	handler := m.handler
	m.mu.RUnlock()
	if alreadyRunning || alreadyInbound {
		return nil
	}
	if handler == nil {
		return fmt.Errorf("instance message handler 未设置")
	}

	adp, err := buildAdapter(inst)
	if err != nil {
		_ = m.setStatus(ctx, name, StatusError, err.Error())
		return err
	}
	wrapped := m.wrapHandler(inst, handler)

	if wa, ok := adp.(adapter.WebhookAdapter); ok {
		if err := wa.Attach(wrapped); err != nil {
			_ = m.setStatus(ctx, name, StatusError, err.Error())
			return err
		}
		m.mu.Lock()
		m.running[name] = adp
		m.inbound[name] = wa.Handler()
		m.metadata[name] = inst
		m.mu.Unlock()
		return m.setStatus(ctx, name, StatusRunning, "")
	}

	if err := adp.Start(ctx, wrapped); err != nil {
		_ = m.setStatus(ctx, name, StatusError, err.Error())
		return err
	}

	m.mu.Lock()
	m.running[name] = adp
	m.metadata[name] = inst
	m.mu.Unlock()
	return m.setStatus(ctx, name, StatusRunning, "")
}

func (m *Manager) Stop(ctx context.Context, name string) error {
	m.mu.Lock()
	adp := m.running[name]
	delete(m.running, name)
	delete(m.inbound, name)
	delete(m.metadata, name)
	m.mu.Unlock()

	if adp != nil {
		if err := adp.Stop(ctx); err != nil {
			_ = m.setStatus(ctx, name, StatusError, err.Error())
			return err
		}
	}
	return m.setStatus(ctx, name, StatusStopped, "")
}

func (m *Manager) StopAll(ctx context.Context) error {
	instances, err := m.List(ctx)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if err := m.Stop(ctx, inst.Name); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	name := r.PathValue("name")

	m.mu.RLock()
	handler, ok := m.inbound[name]
	inst := m.metadata[name]
	m.mu.RUnlock()

	if !ok || inst == nil || inst.Provider != provider {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	handler.ServeHTTP(w, r)
}

func (m *Manager) Health(ctx context.Context, name string) (*HealthReport, error) {
	inst, err := m.Get(ctx, name)
	if err != nil {
		return nil, err
	}

	report := &HealthReport{
		Name:        inst.Name,
		Provider:    inst.Provider,
		Mode:        inst.Mode,
		Status:      inst.Status,
		LastEventAt: inst.LastEventAt,
		LastError:   inst.LastError,
		CheckedAt:   time.Now(),
	}

	m.mu.RLock()
	adp := m.running[name]
	m.mu.RUnlock()

	switch {
	case adp == nil && inst.Status == StatusStopped:
		report.Healthy = false
		return report, nil
	case adp == nil:
		report.Status = StatusError
		report.LastError = "instance runtime 未启动"
		_ = m.setStatus(ctx, name, StatusError, report.LastError)
		return report, nil
	}

	if hc, ok := adp.(adapter.HealthChecker); ok {
		if err := hc.Health(ctx); err != nil {
			report.Status = StatusError
			report.LastError = err.Error()
			_ = m.setStatus(ctx, name, StatusError, report.LastError)
			return report, nil
		}
	}

	report.Status = StatusRunning
	report.Healthy = true
	report.LastError = ""
	_ = m.setStatus(ctx, name, StatusRunning, "")
	return report, nil
}

func (m *Manager) HealthAll(ctx context.Context) ([]*HealthReport, error) {
	list, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	reports := make([]*HealthReport, 0, len(list))
	for _, inst := range list {
		report, err := m.Health(ctx, inst.Name)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func (m *Manager) wrapHandler(inst *Instance, next adapter.MessageHandler) adapter.MessageHandler {
	return func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		if msg.InstanceID == "" {
			msg.InstanceID = inst.Name
		}
		if msg.ID != "" {
			seen, err := m.recordEvent(ctx, inst.Name, msg.ID)
			if err == nil && seen {
				return nil, nil
			}
		}
		_ = m.touchEvent(ctx, inst.Name)
		reply, err := next(ctx, msg)
		if err != nil {
			_ = m.setStatus(ctx, inst.Name, StatusError, err.Error())
			return nil, err
		}
		_ = m.setStatus(ctx, inst.Name, StatusRunning, "")
		return reply, nil
	}
}

func (m *Manager) recordEvent(ctx context.Context, instanceName, eventID string) (bool, error) {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO platform_events (instance_name, event_id, created_at) VALUES (?, ?, ?)`,
		instanceName, eventID, time.Now(),
	)
	if err == nil {
		return false, nil
	}
	// sqlite duplicate primary key
	if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint") {
		return true, nil
	}
	return false, err
}

func (m *Manager) touchEvent(ctx context.Context, name string) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE platform_instances SET last_event_at = ?, updated_at = ? WHERE name = ?`,
		time.Now(), time.Now(), name,
	)
	return err
}

func (m *Manager) setStatus(ctx context.Context, name string, status Status, lastError string) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE platform_instances SET status = ?, last_error = ?, updated_at = ? WHERE name = ?`,
		status, lastError, time.Now(), name,
	)
	return err
}

func instancesFromConfig(cfg *config.Config) ([]Instance, error) {
	var out []Instance
	add := func(provider, name string, enabled bool, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out = append(out, Instance{
			Provider: provider,
			Name:     name,
			Enabled:  enabled,
			Config:   raw,
		})
		return nil
	}

	for _, v := range cfg.Platforms.Feishu {
		if err := add("feishu", defaultName(v.Name, "feishu"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.Platforms.Dingtalk {
		if err := add("dingtalk", defaultName(v.Name, "dingtalk"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.Platforms.Wecom {
		if err := add("wecom", defaultName(v.Name, "wecom"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.Platforms.Slack {
		if err := add("slack", defaultName(v.Name, "slack"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.Platforms.Discord {
		if err := add("discord", defaultName(v.Name, "discord"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.Platforms.Telegram {
		if err := add("telegram", defaultName(v.Name, "telegram"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.Platforms.Wechat {
		if err := add("wechat", defaultName(v.Name, "wechat"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.Platforms.WhatsApp {
		if err := add("whatsapp", defaultName(v.Name, "whatsapp"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.Platforms.LINE {
		if err := add("line", defaultName(v.Name, "line"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.Platforms.Matrix {
		if err := add("matrix", defaultName(v.Name, "matrix"), v.Enabled, v); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider == out[j].Provider {
			return out[i].Name < out[j].Name
		}
		return out[i].Provider < out[j].Provider
	})
	return out, nil
}

func buildAdapter(inst *Instance) (adapter.Adapter, error) {
	switch inst.Provider {
	case "feishu":
		var cfg config.FeishuConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		cfg.Name = inst.Name
		return feishu.New(cfg), nil
	case "dingtalk":
		var cfg config.DingtalkConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		cfg.Name = inst.Name
		return dingtalk.New(cfg), nil
	case "wecom":
		var cfg config.WecomConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		cfg.Name = inst.Name
		return wecom.New(cfg), nil
	case "wechat":
		var cfg config.WechatConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		cfg.Name = inst.Name
		return wechat.New(cfg), nil
	case "slack":
		var cfg config.SlackConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		cfg.Name = inst.Name
		return slack.New(cfg), nil
	case "telegram":
		var cfg config.TelegramConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		cfg.Name = inst.Name
		return telegram.New(cfg), nil
	case "discord":
		var cfg config.DiscordConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		cfg.Name = inst.Name
		return discord.New(cfg), nil
	case "line":
		var cfg config.LINEConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		return line.New(line.Config{
			Name:          inst.Name,
			ChannelSecret: cfg.ChannelSecret,
			ChannelToken:  cfg.ChannelToken,
		}), nil
	case "whatsapp":
		var cfg config.WhatsAppConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		return whatsapp.New(whatsapp.Config{
			Name:        inst.Name,
			Token:       cfg.Token,
			PhoneID:     cfg.PhoneID,
			VerifyToken: cfg.VerifyToken,
		}), nil
	case "matrix":
		var cfg config.MatrixConfig
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			return nil, err
		}
		return matrix.New(matrix.Config{
			Name:          inst.Name,
			HomeserverURL: cfg.HomeserverURL,
			AccessToken:   cfg.AccessToken,
			UserID:        cfg.UserID,
		}), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", inst.Provider)
	}
}

func modeForProvider(provider string) (string, error) {
	switch provider {
	case "feishu", "dingtalk", "wecom", "wechat", "slack", "line", "whatsapp":
		return "webhook", nil
	case "telegram", "discord", "matrix":
		return "runtime", nil
	default:
		return "", fmt.Errorf("unsupported provider %q", provider)
	}
}

func defaultName(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
