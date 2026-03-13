// Package email 提供邮件适配器
//
// 通过 IMAP 轮询收件箱获取新邮件，通过 SMTP 发送回复。
// 将邮件转换为统一的 adapter.Message 格式。
package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
)

// EmailConfig 邮件适配器配置
type EmailConfig struct {
	IMAP         IMAPConfig `yaml:"imap"`
	SMTP         SMTPConfig `yaml:"smtp"`
	PollInterval int        `yaml:"poll_interval"` // 轮询间隔（秒），默认 60
	MaxFetch     int        `yaml:"max_fetch"`     // 每次最多拉取邮件数，默认 10
}

// IMAPConfig IMAP 配置
type IMAPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`     // 默认 993
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	TLS      bool   `yaml:"tls"`      // 默认 true
	Folder   string `yaml:"folder"`   // 默认 INBOX
}

// SMTPConfig SMTP 配置
type SMTPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`     // 默认 587
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	From     string `yaml:"from"`
}

// EmailAdapter 邮件适配器
type EmailAdapter struct {
	cfg     EmailConfig
	handler adapter.MessageHandler
	stopped atomic.Bool
}

// New 创建邮件适配器
func New(cfg EmailConfig) *EmailAdapter {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 60
	}
	if cfg.MaxFetch <= 0 {
		cfg.MaxFetch = 10
	}
	if cfg.IMAP.Port == 0 {
		cfg.IMAP.Port = 993
	}
	if cfg.IMAP.Folder == "" {
		cfg.IMAP.Folder = "INBOX"
	}
	if cfg.SMTP.Port == 0 {
		cfg.SMTP.Port = 587
	}
	return &EmailAdapter{cfg: cfg}
}

func (a *EmailAdapter) Name() string            { return "email" }
func (a *EmailAdapter) Platform() adapter.Platform { return adapter.PlatformEmail }

// Start 启动邮件轮询
func (a *EmailAdapter) Start(ctx context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	a.stopped.Store(false)

	go a.pollLoop(ctx)
	log.Printf("邮件适配器已启动，轮询间隔: %ds", a.cfg.PollInterval)
	return nil
}

// Stop 停止轮询
func (a *EmailAdapter) Stop(_ context.Context) error {
	a.stopped.Store(true)
	log.Println("邮件适配器已停止")
	return nil
}

// Send 发送邮件回复
func (a *EmailAdapter) Send(_ context.Context, chatID string, reply *adapter.Reply) error {
	subject := "Re: HexClaw"
	if reply.Metadata != nil {
		if s, ok := reply.Metadata["subject"]; ok {
			subject = "Re: " + s
		}
	}
	return a.sendEmail(chatID, subject, reply.Content)
}

// SendStream 缓冲流式内容后发送
func (a *EmailAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var buf strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		buf.WriteString(chunk.Content)
	}
	return a.Send(ctx, chatID, &adapter.Reply{Content: buf.String()})
}

func (a *EmailAdapter) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(a.cfg.PollInterval) * time.Second)
	defer ticker.Stop()

	// 首次立即拉取
	a.fetchAndProcess(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.stopped.Load() {
				return
			}
			a.fetchAndProcess(ctx)
		}
	}
}

func (a *EmailAdapter) fetchAndProcess(ctx context.Context) {
	// 简化的 IMAP 客户端 — 生产环境建议使用专业 IMAP 库
	// 当前实现作为适配器框架，核心流程：连接 → 登录 → 搜索未读 → 获取 → 标记已读
	log.Printf("邮件适配器: 检查新邮件 (%s@%s:%d)",
		a.cfg.IMAP.Username, a.cfg.IMAP.Host, a.cfg.IMAP.Port)

	// TODO: 接入完整 IMAP 实现
	// 当前仅输出日志，等待插件系统成熟后通过 IMAP 库插件提供
}

// sanitizeHeader 过滤 SMTP 头部注入字符
func sanitizeHeader(s string) string {
	r := strings.NewReplacer("\r", "", "\n", "")
	return r.Replace(s)
}

func (a *EmailAdapter) sendEmail(to, subject, body string) error {
	cfg := a.cfg.SMTP
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	// 防止邮件头部注入
	to = sanitizeHeader(to)
	subject = sanitizeHeader(subject)

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		cfg.From, to, subject, body)

	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)

	// TLS 连接
	tlsConfig := &tls.Config{ServerName: cfg.Host}
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp", addr, tlsConfig,
	)
	if err != nil {
		// 回退到 STARTTLS
		return smtp.SendMail(addr, auth, cfg.From, []string{to}, []byte(msg))
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("创建 SMTP 客户端失败: %w", err)
	}
	defer client.Close()

	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("smtp 认证失败: %w", err)
	}
	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	return w.Close()
}

// ParseEmailAddress 从 "Name <email>" 格式提取邮箱地址
func ParseEmailAddress(raw string) (name, email string) {
	re := regexp.MustCompile(`(?:(.+?)\s*)?<([^>]+)>`)
	matches := re.FindStringSubmatch(raw)
	if len(matches) >= 3 {
		return strings.TrimSpace(matches[1]), matches[2]
	}
	return "", strings.TrimSpace(raw)
}
