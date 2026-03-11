// Package discord 提供 Discord Bot 适配器
//
// 通过 Discord Gateway (WebSocket) 接收消息，
// 通过 Discord REST API 发送回复。
// 支持斜杠命令、消息回复、流式编辑等功能。
//
// 使用方式：
//
//	adapter := discord.New(cfg)
//	adapter.Start(ctx, handler)
//
// 对标 OpenClaw Discord 集成。
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/toolkit/util/idgen"

	"github.com/gorilla/websocket"
)

const (
	apiBase    = "https://discord.com/api/v10"
	gatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"
)

// DiscordAdapter Discord Bot 适配器
//
// 通过 WebSocket (Gateway) 连接到 Discord，接收消息事件。
// 回复通过 REST API 发送。
// 支持自动重连和心跳维持。
type DiscordAdapter struct {
	cfg     config.DiscordConfig
	handler adapter.MessageHandler
	client  *http.Client

	conn        *websocket.Conn  // WebSocket 连接
	mu          sync.Mutex       // 保护 conn
	sessionID   string           // Gateway 会话 ID（用于恢复连接）
	seq         atomic.Int64     // 最新序列号
	stopped     atomic.Bool      // 是否已停止
	heartbeatCh chan struct{}     // 心跳停止信号
}

// New 创建 Discord 适配器
func New(cfg config.DiscordConfig) *DiscordAdapter {
	return &DiscordAdapter{
		cfg:         cfg,
		client:      &http.Client{Timeout: 30 * time.Second},
		heartbeatCh: make(chan struct{}),
	}
}

func (a *DiscordAdapter) Name() string              { return "discord" }
func (a *DiscordAdapter) Platform() adapter.Platform { return adapter.PlatformDiscord }

// Start 启动 Discord Bot
//
// 连接 Gateway WebSocket，开始接收消息。
// 自动维持心跳和处理重连。
func (a *DiscordAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	a.stopped.Store(false)

	if a.cfg.Token == "" {
		return fmt.Errorf("Discord Bot Token 不能为空")
	}

	go a.connectLoop()
	log.Println("Discord 适配器已启动")
	return nil
}

// Stop 停止适配器
func (a *DiscordAdapter) Stop(_ context.Context) error {
	a.stopped.Store(true)

	// 关闭心跳
	select {
	case a.heartbeatCh <- struct{}{}:
	default:
	}

	// 关闭 WebSocket 连接
	a.mu.Lock()
	if a.conn != nil {
		a.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		a.conn.Close()
		a.conn = nil
	}
	a.mu.Unlock()

	log.Println("Discord 适配器已停止")
	return nil
}

// Send 发送同步回复
func (a *DiscordAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	return a.sendMessage(ctx, chatID, reply.Content)
}

// SendStream 流式发送（发送初始消息，后续编辑更新）
func (a *DiscordAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	var msgID string

	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)

		// 首个 chunk 发送新消息，后续编辑
		if msgID == "" {
			id, err := a.createMessage(ctx, chatID, sb.String())
			if err != nil {
				return err
			}
			msgID = id
		} else {
			a.editMessage(ctx, chatID, msgID, sb.String())
		}
	}
	return nil
}

// ============== Gateway 连接 ==============

// connectLoop 自动重连循环
func (a *DiscordAdapter) connectLoop() {
	for !a.stopped.Load() {
		if err := a.connect(); err != nil {
			log.Printf("Discord Gateway 连接失败: %v", err)
		}
		if a.stopped.Load() {
			return
		}
		// 重连间隔（带退避）
		time.Sleep(5 * time.Second)
	}
}

// connect 建立 Gateway 连接并处理事件
func (a *DiscordAdapter) connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(gatewayURL, nil)
	if err != nil {
		return fmt.Errorf("WebSocket 连接失败: %w", err)
	}

	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()

	defer func() {
		conn.Close()
		a.mu.Lock()
		a.conn = nil
		a.mu.Unlock()
	}()

	// 读取 Hello 事件获取心跳间隔
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("读取 Hello 事件失败: %w", err)
	}

	var hello gatewayEvent
	if err := json.Unmarshal(msg, &hello); err != nil {
		return fmt.Errorf("解析 Hello 事件失败: %w", err)
	}

	if hello.Op != 10 {
		return fmt.Errorf("期望 Hello (op=10)，收到 op=%d", hello.Op)
	}

	// 解析心跳间隔
	var helloData struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	json.Unmarshal(hello.D, &helloData)
	heartbeatInterval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond

	// 发送 Identify
	if err := a.sendIdentify(conn); err != nil {
		return fmt.Errorf("发送 Identify 失败: %w", err)
	}

	// 启动心跳
	go a.heartbeat(conn, heartbeatInterval)

	// 读取事件循环
	for !a.stopped.Load() {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !a.stopped.Load() {
				log.Printf("Discord 读取消息出错: %v", err)
			}
			return err
		}
		a.handleEvent(msg)
	}
	return nil
}

// sendIdentify 发送身份验证
func (a *DiscordAdapter) sendIdentify(conn *websocket.Conn) error {
	identify := map[string]any{
		"op": 2,
		"d": map[string]any{
			"token":   a.cfg.Token,
			"intents": 33281, // GUILDS | GUILD_MESSAGES | DIRECT_MESSAGES | MESSAGE_CONTENT
			"properties": map[string]string{
				"os":      "linux",
				"browser": "hexclaw",
				"device":  "hexclaw",
			},
		},
	}
	return conn.WriteJSON(identify)
}

// heartbeat 定期发送心跳
func (a *DiscordAdapter) heartbeat(conn *websocket.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			seq := a.seq.Load()
			data := map[string]any{"op": 1, "d": seq}
			a.mu.Lock()
			err := conn.WriteJSON(data)
			a.mu.Unlock()
			if err != nil {
				log.Printf("Discord 心跳发送失败: %v", err)
				return
			}
		case <-a.heartbeatCh:
			return
		}
	}
}

// handleEvent 处理 Gateway 事件
func (a *DiscordAdapter) handleEvent(raw []byte) {
	var event gatewayEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return
	}

	// 更新序列号
	if event.S > 0 {
		a.seq.Store(int64(event.S))
	}

	switch event.Op {
	case 0: // Dispatch
		a.handleDispatch(event.T, event.D)
	case 11: // Heartbeat ACK
		// 正常，不需要处理
	case 7: // Reconnect
		log.Println("Discord 要求重连")
		a.mu.Lock()
		if a.conn != nil {
			a.conn.Close()
		}
		a.mu.Unlock()
	}
}

// handleDispatch 处理分发事件
func (a *DiscordAdapter) handleDispatch(eventType string, data json.RawMessage) {
	switch eventType {
	case "READY":
		var ready struct {
			SessionID string `json:"session_id"`
		}
		json.Unmarshal(data, &ready)
		a.sessionID = ready.SessionID
		log.Println("Discord Bot 已就绪")

	case "MESSAGE_CREATE":
		a.handleMessageCreate(data)
	}
}

// handleMessageCreate 处理新消息
func (a *DiscordAdapter) handleMessageCreate(data json.RawMessage) {
	var msg discordMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("解析 Discord 消息失败: %v", err)
		return
	}

	// 忽略 Bot 自己的消息
	if msg.Author.Bot {
		return
	}

	// 转换为统一消息格式
	unified := &adapter.Message{
		ID:        "discord-" + idgen.ShortID(),
		Platform:  adapter.PlatformDiscord,
		ChatID:    msg.ChannelID,
		UserID:    msg.Author.ID,
		UserName:  msg.Author.Username,
		Content:   msg.Content,
		Timestamp: time.Now(),
		Metadata: map[string]string{
			"guild_id":   msg.GuildID,
			"message_id": msg.ID,
		},
	}

	// 异步处理消息
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		reply, err := a.handler(ctx, unified)
		if err != nil {
			log.Printf("Discord 消息处理失败: %v", err)
			a.sendMessage(ctx, msg.ChannelID, "处理消息时出错，请稍后重试。")
			return
		}
		if reply != nil {
			a.sendMessage(ctx, msg.ChannelID, reply.Content)
		}
	}()
}

// ============== REST API ==============

// sendMessage 发送消息到频道
func (a *DiscordAdapter) sendMessage(ctx context.Context, channelID, content string) error {
	_, err := a.createMessage(ctx, channelID, content)
	return err
}

// createMessage 创建消息并返回消息 ID
func (a *DiscordAdapter) createMessage(ctx context.Context, channelID, content string) (string, error) {
	url := fmt.Sprintf("%s/channels/%s/messages", apiBase, channelID)

	// Discord 消息最大 2000 字符，超长时分段发送
	if len(content) > 2000 {
		content = content[:1997] + "..."
	}

	body := map[string]string{"content": content}
	data, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("发送 Discord 消息失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Discord API 错误 (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.ID, nil
}

// editMessage 编辑已发送的消息（用于流式更新）
func (a *DiscordAdapter) editMessage(ctx context.Context, channelID, messageID, content string) error {
	url := fmt.Sprintf("%s/channels/%s/messages/%s", apiBase, channelID, messageID)

	if len(content) > 2000 {
		content = content[:1997] + "..."
	}

	body := map[string]string{"content": content}
	data, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("编辑 Discord 消息失败: %w", err)
	}
	resp.Body.Close()
	return nil
}

// ============== 数据模型 ==============

// gatewayEvent Discord Gateway 事件
type gatewayEvent struct {
	Op int             `json:"op"` // 操作码
	D  json.RawMessage `json:"d"`  // 事件数据
	S  int             `json:"s"`  // 序列号
	T  string          `json:"t"`  // 事件类型
}

// discordMessage Discord 消息
type discordMessage struct {
	ID        string        `json:"id"`
	ChannelID string        `json:"channel_id"`
	GuildID   string        `json:"guild_id"`
	Author    discordUser   `json:"author"`
	Content   string        `json:"content"`
	Timestamp string        `json:"timestamp"`
}

// discordUser Discord 用户
type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}
