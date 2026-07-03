package api

// handler_connections.go —— 连接中心「测试连接」端点（v0.4.3 §15）。
//
// POST /api/v1/connections/test 是一个**无状态**端点：凭据随请求体到达，仅用于本次
// 瞬态探测，**绝不持久化、绝不落日志**。供桌面端在"保存"前验证一组连接凭据是否可用。
//
// 与已有的 POST /api/v1/im/channels/{provider}/test 的区别：
//   - 统一契约 {type, config} → {ok, detail}，type 含 email 与各 IM 平台；
//   - email 走真正的 SMTP AUTH + 可选 IMAP 登录探测；
//   - IM 平台复用 instances.BuildAdapter + adapter.ConfigValidator 这一既有、经过验证的探测口子，
//     不重复造轮子，也不向外部 API 发垃圾请求。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/adapter/email"
	"github.com/hexagon-codes/hexclaw/instances"
)

// connectionSummary 是连接中心的脱敏只读视图（一等公民对象）。
// 刻意不含 Config / 任何凭据字段——该端点永不下发凭据。
type connectionSummary struct {
	ID           string   `json:"id"`
	Provider     string   `json:"provider"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	Status       string   `json:"status"`
	Enabled      bool     `json:"enabled"`
}

// connectionCapabilities 由 provider 推导连接能力。IM 平台已接入 instances.Manager（BuildAdapter +
// modeForProvider），可收可发 → ["receive","send"]。email 也接入 instances.Manager（IMAP 轮询 +
// SMTP Send），因此和 IM 一样是可投递连接。
// 未知 provider 返回空切片（非 nil，便于 JSON 出 []）。
func connectionCapabilities(provider string) []string {
	switch provider {
	case "feishu", "dingtalk", "discord", "telegram", "wecom", "wechat",
		"slack", "line", "whatsapp", "matrix", "email":
		return []string{"receive", "send"}
	default:
		return []string{}
	}
}

// handleListConnections GET /api/v1/connections
//
// 返回所有连接的脱敏列表（一处存 / first-class object）。**绝不**序列化 Config 或任何凭据，
// 仅暴露 id / provider / name / capabilities / status / enabled。
func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	list, err := s.instanceMgr.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]connectionSummary, 0, len(list))
	for _, inst := range list {
		out = append(out, connectionSummary{
			ID:           inst.ID,
			Provider:     inst.Provider,
			Name:         inst.Name,
			Capabilities: connectionCapabilities(inst.Provider),
			Status:       string(inst.Status),
			Enabled:      inst.Enabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connections": out,
		"total":       len(out),
	})
}

// connectionTestRequest 测试连接请求体。Config 保持原始 JSON，按 Type 解码到对应配置结构。
type connectionTestRequest struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

// connectionTestResponse 测试连接响应体。Detail 为人类可读的成功/失败原因，
// 由调用方直接展示给用户，**不含任何凭据**。
type connectionTestResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// connectionTestTimeout 单次探测的总超时。外部网络握手/认证应在此时间内完成，
// 否则视为失败，确保端点永不长时间挂起。
const connectionTestTimeout = 10 * time.Second

// handleConnectionsTest POST /api/v1/connections/test
//
// 无状态验证一组连接凭据。凭据仅在本次请求内瞬态使用，绝不持久化或记录。
func (s *Server) handleConnectionsTest(w http.ResponseWriter, r *http.Request) {
	var req connectionTestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}

	connType := strings.ToLower(strings.TrimSpace(req.Type))
	if connType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type 不能为空"})
		return
	}
	if len(req.Config) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config 不能为空"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), connectionTestTimeout)
	defer cancel()

	switch connType {
	case "email":
		ok, detail := testEmailConnection(ctx, req.Config)
		writeJSON(w, http.StatusOK, connectionTestResponse{OK: ok, Detail: detail})
	case "feishu", "dingtalk", "discord", "telegram", "wecom", "wechat", "slack", "line", "whatsapp", "matrix":
		ok, detail := testIMConnection(ctx, connType, req.Config)
		writeJSON(w, http.StatusOK, connectionTestResponse{OK: ok, Detail: detail})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "不支持的连接类型: " + connType,
		})
	}
}

// emailTestConfig 测试连接专用的邮件配置入参。字段对齐 adapter/email 的 SMTP/IMAP 结构，
// 但 IMAP host 缺省时跳过 IMAP 探测（许多场景只需验证发件能力）。
type emailTestConfig struct {
	SMTP struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		From     string `json:"from"`
	} `json:"smtp"`
	IMAP struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		TLS      *bool  `json:"tls"`
		Folder   string `json:"folder"`
	} `json:"imap"`
}

// testEmailConnection 探测邮件凭据：先做 SMTP AUTH（不发邮件），IMAP host 给定时再做 IMAP 登录。
// host 缺省时由账号邮箱域名推导常见默认值。返回 (ok, 人类可读 detail)，detail 不含凭据。
func testEmailConnection(ctx context.Context, raw json.RawMessage) (bool, string) {
	var cfg emailTestConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return false, "邮件配置格式错误: " + err.Error()
	}

	smtp := cfg.SMTP
	// SMTP host 缺省时尝试由账号/发件人邮箱域名推导（smtp.<domain>）。
	if smtp.Host == "" {
		if host := deriveMailHost("smtp", smtp.Username, smtp.From); host != "" {
			smtp.Host = host
		}
	}
	if smtp.Host == "" || smtp.Username == "" || smtp.Password == "" {
		return false, "SMTP host/username/password 不能为空（host 也无法从邮箱域名推导）"
	}

	if err := email.ProbeSMTP(ctx, email.SMTPConfig{
		Host:     smtp.Host,
		Port:     smtp.Port,
		Username: smtp.Username,
		Password: smtp.Password,
		From:     smtp.From,
	}); err != nil {
		// ProbeSMTP 已返回分类好的、面向用户的消息（连接失败 / STARTTLS / 认证失败），
		// 直接透出。不再统一前缀"认证失败"——否则连接层失败（如 EOF）会被误标为认证问题，
		// 误导用户反复检查密码。
		return false, err.Error()
	}
	detail := "SMTP 认证通过"

	// IMAP 为可选探测：仅在显式给定 IMAP host（或可由域名推导且账号齐备）时执行。
	imap := cfg.IMAP
	if imap.Host == "" && imap.Username != "" {
		if host := deriveMailHost("imap", imap.Username, ""); host != "" {
			imap.Host = host
		}
	}
	if imap.Host == "" {
		return true, detail + "（未配置 IMAP，已跳过收件验证）"
	}
	if imap.Username == "" || imap.Password == "" {
		return false, detail + "，但 IMAP username/password 缺失，无法验证收件"
	}

	useTLS := true
	if imap.TLS != nil {
		useTLS = *imap.TLS
	}
	// 复用 adapter/email 的 IMAP 登录路径（ValidateConfig 同款逻辑），不另起 IMAP 实现。
	a := email.New(email.EmailConfig{
		IMAP: email.IMAPConfig{
			Host:     imap.Host,
			Port:     imap.Port,
			Username: imap.Username,
			Password: imap.Password,
			TLS:      useTLS,
			Folder:   imap.Folder,
		},
		// SMTP 字段填齐以满足 ValidateConfig 的非空前置校验；此处不会再次发起 SMTP 连接，
		// 因为上面已单独探测过 SMTP。
		SMTP: email.SMTPConfig{
			Host:     smtp.Host,
			Port:     smtp.Port,
			Username: smtp.Username,
			Password: smtp.Password,
			From:     firstNonEmpty(smtp.From, smtp.Username),
		},
	})
	if err := a.ValidateConfig(ctx); err != nil {
		return false, detail + "，但 IMAP 登录失败: " + err.Error()
	}
	return true, "SMTP 认证通过 · IMAP 已登录"
}

// testIMConnection 复用 instances.BuildAdapter + adapter.ConfigValidator 探测 IM 凭据。
// BuildAdapter 解析 config 并构造适配器；ConfigValidator 做字段校验 +（飞书/电报/Discord/
// 企业微信/微信）一次轻量凭据校验（拉 token / getMe）。钉钉只做字段校验（无廉价 REST token 口子）。
func testIMConnection(ctx context.Context, connType string, raw json.RawMessage) (bool, string) {
	inst := &instances.Instance{
		Provider: connType,
		Name:     "__connection_test__",
		Config:   raw,
	}
	adp, err := instances.BuildAdapter(inst)
	if err != nil {
		return false, "配置解析失败: " + err.Error()
	}
	cv, ok := adp.(adapter.ConfigValidator)
	if !ok {
		// 适配器未实现校验口子：构造成功即视为字段格式可用。
		return true, "配置格式有效（该平台不支持在线凭据校验）"
	}
	if err := cv.ValidateConfig(ctx); err != nil {
		return false, "凭据校验失败: " + err.Error()
	}
	return true, "凭据校验通过"
}

// deriveMailHost 由邮箱地址域名推导常见的 SMTP/IMAP 主机名（如 smtp.example.com）。
// 仅在用户未显式提供 host 时作为便利默认；推导不出则返回空串，由调用方判定。
func deriveMailHost(prefix, primary, fallback string) string {
	addr := primary
	if addr == "" {
		addr = fallback
	}
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	domain := strings.TrimSpace(addr[at+1:])
	if domain == "" {
		return ""
	}
	return prefix + "." + domain
}
