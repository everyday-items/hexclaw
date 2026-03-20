// Package email 提供邮件适配器
//
// 通过 IMAP 轮询收件箱获取新邮件，通过 SMTP 发送回复。
// 将邮件转换为统一的 adapter.Message 格式。
package email

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/mail"
	"net/smtp"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
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
	Port     int    `yaml:"port"` // 默认 993
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	TLS      bool   `yaml:"tls"`    // 默认 true
	Folder   string `yaml:"folder"` // 默认 INBOX
}

// SMTPConfig SMTP 配置
type SMTPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"` // 默认 587
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	From     string `yaml:"from"`
}

// EmailAdapter 邮件适配器
type EmailAdapter struct {
	cfg      EmailConfig
	handler  adapter.MessageHandler
	stopped  atomic.Bool
	dialIMAP func(context.Context, IMAPConfig) (imapSession, error)
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
	return &EmailAdapter{cfg: cfg, dialIMAP: func(ctx context.Context, cfg IMAPConfig) (imapSession, error) {
		return dialIMAP(ctx, cfg)
	}}
}

func (a *EmailAdapter) Name() string               { return "email" }
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
	if a.handler == nil {
		return
	}

	log.Printf("邮件适配器: 检查新邮件 (%s@%s:%d)",
		a.cfg.IMAP.Username, a.cfg.IMAP.Host, a.cfg.IMAP.Port)

	client, err := a.dialIMAP(ctx, a.cfg.IMAP)
	if err != nil {
		log.Printf("邮件适配器: IMAP 连接失败: %v", err)
		return
	}
	defer func() { _ = client.Close() }()

	if err := client.Login(a.cfg.IMAP.Username, a.cfg.IMAP.Password); err != nil {
		log.Printf("邮件适配器: IMAP 登录失败: %v", err)
		return
	}
	defer func() {
		if err := client.Logout(); err != nil {
			log.Printf("邮件适配器: IMAP 登出失败: %v", err)
		}
	}()

	if err := client.Select(a.cfg.IMAP.Folder); err != nil {
		log.Printf("邮件适配器: 选择邮箱失败: %v", err)
		return
	}

	ids, err := client.SearchUnseen(a.cfg.MaxFetch)
	if err != nil {
		log.Printf("邮件适配器: 搜索未读邮件失败: %v", err)
		return
	}

	for _, id := range ids {
		raw, err := client.FetchRFC822(id)
		if err != nil {
			log.Printf("邮件适配器: 拉取邮件失败: id=%s err=%v", id, err)
			continue
		}

		msg, subject, err := parseIncomingEmail(raw)
		if err != nil {
			log.Printf("邮件适配器: 解析邮件失败: id=%s err=%v", id, err)
			continue
		}

		reply, err := a.handler(ctx, msg)
		if err != nil {
			log.Printf("邮件适配器: 处理邮件失败: id=%s err=%v", id, err)
			continue
		}
		if reply != nil {
			if reply.Metadata == nil {
				reply.Metadata = make(map[string]string, 1)
			}
			if _, ok := reply.Metadata["subject"]; !ok && subject != "" {
				reply.Metadata["subject"] = subject
			}
			if err := a.Send(ctx, msg.ChatID, reply); err != nil {
				log.Printf("邮件适配器: 发送回复失败: id=%s err=%v", id, err)
			}
		}

		if err := client.MarkSeen(id); err != nil {
			log.Printf("邮件适配器: 标记已读失败: id=%s err=%v", id, err)
		}
	}
}

type imapClient struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
	tagSeq int
}

type imapSession interface {
	Close() error
	Login(username, password string) error
	Select(folder string) error
	SearchUnseen(maxFetch int) ([]string, error)
	FetchRFC822(id string) ([]byte, error)
	MarkSeen(id string) error
	Logout() error
}

func dialIMAP(ctx context.Context, cfg IMAPConfig) (*imapClient, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	dialer := &net.Dialer{Timeout: 10 * time.Second}

	var (
		conn net.Conn
		err  error
	)
	if cfg.TLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	client := &imapClient{
		conn:   conn,
		reader: bufio.NewReader(conn),
		writer: bufio.NewWriter(conn),
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	line, err := client.readLine()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取 greeting 失败: %w", err)
	}
	if !strings.HasPrefix(line, "* OK") {
		_ = conn.Close()
		return nil, fmt.Errorf("IMAP greeting 非 OK: %s", strings.TrimSpace(line))
	}
	return client, nil
}

func (c *imapClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *imapClient) Login(username, password string) error {
	return c.runSimple("LOGIN %q %q", username, password)
}

func (c *imapClient) Select(folder string) error {
	return c.runSimple("SELECT %q", folder)
}

func (c *imapClient) MarkSeen(id string) error {
	return c.runSimple("STORE %s +FLAGS (\\\\Seen)", id)
}

func (c *imapClient) Logout() error {
	return c.runSimple("LOGOUT")
}

func (c *imapClient) SearchUnseen(maxFetch int) ([]string, error) {
	res, err := c.run("SEARCH UNSEEN")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range res.lines {
		if !strings.HasPrefix(line, "* SEARCH") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 2 {
			ids = append(ids, fields[2:]...)
		}
	}
	if maxFetch > 0 && len(ids) > maxFetch {
		ids = ids[:maxFetch]
	}
	return ids, nil
}

func (c *imapClient) FetchRFC822(id string) ([]byte, error) {
	res, err := c.run("FETCH %s RFC822", id)
	if err != nil {
		return nil, err
	}
	if len(res.literals) == 0 {
		return nil, fmt.Errorf("FETCH %s 未返回 RFC822 内容", id)
	}
	return bytes.TrimRight(res.literals[0], "\r\n"), nil
}

type imapResponse struct {
	lines    []string
	literals [][]byte
}

func (c *imapClient) runSimple(format string, args ...any) error {
	_, err := c.run(format, args...)
	return err
}

func (c *imapClient) run(format string, args ...any) (*imapResponse, error) {
	tag := c.nextTag()
	cmd := fmt.Sprintf(format, args...)
	if _, err := c.writer.WriteString(tag + " " + cmd + "\r\n"); err != nil {
		return nil, err
	}
	if err := c.writer.Flush(); err != nil {
		return nil, err
	}

	resp := &imapResponse{}
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		resp.lines = append(resp.lines, line)

		if size, ok := parseLiteralSize(line); ok {
			literal := make([]byte, size)
			if _, err := io.ReadFull(c.reader, literal); err != nil {
				return nil, err
			}
			resp.literals = append(resp.literals, literal)
			continue
		}

		if strings.HasPrefix(line, tag+" ") {
			if !strings.Contains(line, " OK") {
				return nil, fmt.Errorf("IMAP %s 失败: %s", cmd, strings.TrimSpace(line))
			}
			return resp, nil
		}
	}
}

func (c *imapClient) nextTag() string {
	c.tagSeq++
	return fmt.Sprintf("A%04d", c.tagSeq)
}

func (c *imapClient) readLine() (string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return line, nil
}

func parseLiteralSize(line string) (int, bool) {
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	start := strings.LastIndexByte(line, '{')
	end := strings.LastIndexByte(line, '}')
	if start < 0 || end <= start+1 || end != len(line)-1 {
		return 0, false
	}
	size, err := strconv.Atoi(line[start+1 : end])
	if err != nil {
		return 0, false
	}
	return size, true
}

func parseIncomingEmail(raw []byte) (*adapter.Message, string, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}

	fromName, fromEmail := ParseEmailAddress(m.Header.Get("From"))
	subject := m.Header.Get("Subject")
	body, err := io.ReadAll(m.Body)
	if err != nil {
		return nil, "", err
	}

	ts := time.Now()
	if date := m.Header.Get("Date"); date != "" {
		if parsed, err := mail.ParseDate(date); err == nil {
			ts = parsed
		}
	}

	msgID := strings.TrimSpace(strings.Trim(m.Header.Get("Message-ID"), "<>"))
	if msgID == "" {
		msgID = fmt.Sprintf("email-%d", ts.UnixNano())
	}

	return &adapter.Message{
		ID:       msgID,
		Platform: adapter.PlatformEmail,
		ChatID:   fromEmail,
		UserID:   fromEmail,
		UserName: fromName,
		Content:  strings.TrimSpace(string(body)),
		Metadata: map[string]string{
			"subject":    subject,
			"from_email": fromEmail,
		},
		Timestamp: ts,
	}, subject, nil
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
	defer func() { _ = conn.Close() }()

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("创建 SMTP 客户端失败: %w", err)
	}
	defer func() { _ = client.Close() }()

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
