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
	"github.com/hexagon-codes/toolkit/util/logger"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/net/httpx"
	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/retry"

	"github.com/gorilla/websocket"
)

const (
	apiBase    = "https://discord.com/api/v10"
	gatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"

	maxDiscordErrorBodyBytes = 64 << 10

	// W3-3：Gateway 重连指数退避参数。
	reconnectBaseDelay = 1 * time.Second  // 首次重连退避起步时长
	reconnectMaxDelay  = 60 * time.Second // 退避封顶时长
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
	queue   *adapter.SendQueue
	// v0.4.0 E5：Start 收到的 ctx，仅作 logger/trace 链路源头使用；
	// 实际异步处理用 trace.Detach(baseCtx) 派生新 ctx，避免被父 cancel 杀死。
	baseCtx context.Context

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	loopWG          sync.WaitGroup
	loopWaiter      adapter.LifecycleWaiter
	workerMu        sync.Mutex
	handlerWG       sync.WaitGroup
	handlerWaiter   adapter.LifecycleWaiter
	workerCtx       context.Context
	workerCancel    context.CancelFunc
	stopping        bool

	conn          *websocket.Conn // WebSocket 连接
	mu            sync.Mutex      // 保护 conn
	sessionID     string          // Gateway 会话 ID（用于恢复连接）
	seq           atomic.Int64    // 最新序列号
	stopped       atomic.Bool     // 是否已停止
	heartbeatStop chan struct{}   // 当前连接的心跳停止信号
}

// New 创建 Discord 适配器
func New(cfg config.DiscordConfig) *DiscordAdapter {
	workerCtx, workerCancel := context.WithCancel(context.Background())
	a := &DiscordAdapter{
		cfg:          cfg,
		client:       httpx.MustNewRawClient(httpx.WithRawTimeout(30 * time.Second)),
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
	}
	a.queue = adapter.NewPlatformSendQueue(adapter.PlatformDiscord, a.sendReplyNow)
	return a
}

func (a *DiscordAdapter) Name() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return "discord"
}
func (a *DiscordAdapter) Platform() adapter.Platform { return adapter.PlatformDiscord }

// Start 启动 Discord Bot
//
// 连接 Gateway WebSocket，开始接收消息。
// 自动维持心跳和处理重连。
func (a *DiscordAdapter) Start(ctx context.Context, handler adapter.MessageHandler) error {
	if a.cfg.Token == "" {
		return fmt.Errorf("discord bot token 不能为空")
	}
	if handler == nil {
		return fmt.Errorf("discord message handler 不能为空")
	}
	a.handler = handler
	a.stopped.Store(false)
	// v0.4.0 E5：保留 Start ctx，handleMessageCreate 用 Detach 派生异步处理 ctx
	if ctx == nil {
		ctx = context.Background()
	}
	a.baseCtx = ctx
	a.loopWaiter.Reset()
	a.handlerWaiter.Reset()

	a.workerMu.Lock()
	a.stopping = false
	if a.workerCtx == nil || a.workerCtx.Err() != nil {
		a.workerCtx, a.workerCancel = context.WithCancel(context.Background())
	}
	a.lifecycleCtx, a.lifecycleCancel = context.WithCancel(ctx)
	loopCtx := a.lifecycleCtx
	a.loopWG.Add(1)
	a.workerMu.Unlock()

	go func() {
		defer a.loopWG.Done()
		a.connectLoop(loopCtx)
	}()
	logger.Info("Discord 适配器已启动")
	return nil
}

// Stop 停止适配器
func (a *DiscordAdapter) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.stopped.Store(true)

	a.workerMu.Lock()
	a.stopping = true
	if a.lifecycleCancel != nil {
		a.lifecycleCancel()
	}
	if a.workerCancel != nil {
		a.workerCancel()
	}
	a.workerMu.Unlock()

	a.mu.Lock()
	if a.heartbeatStop != nil {
		close(a.heartbeatStop)
		a.heartbeatStop = nil
	}
	if a.conn != nil {
		_ = a.conn.Close()
		a.conn = nil
	}
	a.mu.Unlock()

	stopQueue := func() error {
		if a.queue != nil {
			return a.queue.Stop(ctx)
		}
		return nil
	}
	if err := a.loopWaiter.Wait(ctx, &a.loopWG); err != nil {
		_ = stopQueue()
		return err
	}
	if err := a.handlerWaiter.Wait(ctx, &a.handlerWG); err != nil {
		_ = stopQueue()
		return err
	}
	if err := stopQueue(); err != nil {
		return err
	}

	logger.Info("Discord 适配器已停止")
	return nil
}

// Send 发送同步回复
//
// v0.4.0 F6：reply.Interactive 非空时，flag interactive.render.v1 OFF 自动追加文本
// fallback，让按钮/选项/审批/卡片在 Discord 基础可用；flag ON 时 no-op，等待后续
// Embed Components 原生 renderer。
func (a *DiscordAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	adapter.MaybeApplyTextFallback(ctx, reply)
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

// SendStream 流式发送（发送初始消息，后续编辑更新）
//
// v0.4.0 E1：接入 adapter.StreamEditLimiter，避免高频 PATCH 触发 Discord
// channel-level 限流（默认 5 req/5s/channel）。三级保护：字符阈值 + 时间间隔 + 失败降级。
func (a *DiscordAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	var msgID string
	limiter := adapter.NewStreamEditLimiter()

	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)

		// v0.4.0 E2：每次发送/编辑都 strip thinking
		clean := adapter.StripThinking(sb.String())
		if msgID == "" {
			if !limiter.ShouldEmit(sb.Len()) {
				continue
			}
			id, err := a.createMessage(ctx, chatID, clean)
			if err != nil {
				if degraded := limiter.Failed(); degraded {
					logger.Warn("[discord] 流式限流降级", "delay", limiter.CurrentDelay())
				}
				return err
			}
			msgID = id
			limiter.Emitted(sb.Len())
		} else if limiter.ShouldEmit(sb.Len()) {
			if err := a.editMessage(ctx, chatID, msgID, clean); err != nil {
				logger.Error("[discord] 编辑流式消息失败", "error", err)
				if degraded := limiter.Failed(); degraded {
					logger.Warn("[discord] 流式限流降级", "delay", limiter.CurrentDelay())
				}
			} else {
				limiter.Emitted(sb.Len())
			}
		}
	}

	// 收尾：限流期间残留内容必须落地
	finalClean := adapter.StripThinking(sb.String())
	if finalClean == "" {
		return nil
	}
	if msgID == "" {
		_, err := a.createMessage(ctx, chatID, finalClean)
		return err
	}
	return a.editMessage(ctx, chatID, msgID, finalClean)
}

// ============== Gateway 连接 ==============

// connectLoop 自动重连循环
//
// W3-3：此前固定 5s 重连无退避，Discord 网关抖动或限流时会形成稳定的高频
// 重连风暴。改为复用 toolkit/util/retry 的指数退避：连接成功（事件循环正常
// 运行过一段时间）后重置退避计数，连续失败时延迟按 1s→2s→4s... 增长，封顶
// reconnectMaxDelay。
func (a *DiscordAdapter) connectLoop(ctx context.Context) {
	attempt := 0
	for !a.stopped.Load() && ctx.Err() == nil {
		start := time.Now()
		if err := a.connect(ctx); err != nil && ctx.Err() == nil {
			logger.Error("Discord Gateway 连接失败", "error", err)
		}
		if a.stopped.Load() || ctx.Err() != nil {
			return
		}
		// 连接维持超过 60s 视为一次成功会话，重置退避计数。
		if time.Since(start) >= 60*time.Second {
			attempt = 0
		}
		delay := a.reconnectDelay(attempt)
		attempt++
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// reconnectDelay 计算第 attempt 次重连前的退避时长。
//
// W3-3：复用 toolkit/util/retry 的指数退避算法（1s 起步、2 倍增长、封顶
// reconnectMaxDelay），attempt 从 0 开始。toolkit v0.3.0 的 ExponentialBackoff
// 采用一基重试序号（非正序号返回 0），故映射为 attempt+1 保持原语义：
// 第 0 次重连 1s、第 1 次 2s、第 2 次 4s。
func (a *DiscordAdapter) reconnectDelay(attempt int) time.Duration {
	cfg := retry.DefaultConfig()
	cfg.Delay = reconnectBaseDelay
	cfg.MaxDelay = reconnectMaxDelay
	cfg.Multiplier = 2
	return retry.ExponentialBackoff(attempt+1, cfg)
}

// connect 建立 Gateway 连接并处理事件
func (a *DiscordAdapter) connect(ctx context.Context) error {
	return a.connectToContext(ctx, gatewayURL)
}

// connectTo 连接指定 Gateway URL 并处理事件。
//
// 从 connect 抽出 URL 参数，便于在测试中指向本地 WebSocket 服务器，
// 验证 Resume/Identify 握手选择与 Hello 心跳间隔校验（W3-3 / W3-4）。
func (a *DiscordAdapter) connectTo(url string) error {
	return a.connectToContext(context.Background(), url)
}

func (a *DiscordAdapter) connectToContext(ctx context.Context, url string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("webSocket 连接失败: %w", err)
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()

	defer func() {
		_ = conn.Close()
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
	// W3-4：此前忽略 Unmarshal 返回值，解析失败会静默走零值 → 心跳间隔为 0，
	// time.NewTicker(0) 会 panic。显式校验并把错误上抛触发重连。
	var helloData struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &helloData); err != nil {
		return fmt.Errorf("解析心跳间隔失败: %w", err)
	}
	if helloData.HeartbeatInterval <= 0 {
		return fmt.Errorf("非法心跳间隔: %d", helloData.HeartbeatInterval)
	}
	heartbeatInterval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond

	// W3-3：握手阶段优先尝试 Resume——若已有 sessionID（上次连接成功获得），
	// 发送 op=6 Resume 复用旧会话，避免漏掉断连期间的事件；否则全新 Identify。
	a.mu.Lock()
	resumeSession := a.sessionID
	a.mu.Unlock()
	if resumeSession != "" {
		if err := a.sendResume(conn, resumeSession, a.seq.Load()); err != nil {
			return fmt.Errorf("发送 Resume 失败: %w", err)
		}
	} else if err := a.sendIdentify(conn); err != nil {
		return fmt.Errorf("发送 Identify 失败: %w", err)
	}

	// 停止旧心跳并启动新心跳
	a.mu.Lock()
	if a.heartbeatStop != nil {
		close(a.heartbeatStop)
	}
	a.heartbeatStop = make(chan struct{})
	stopCh := a.heartbeatStop
	a.mu.Unlock()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		a.heartbeat(conn, heartbeatInterval, stopCh)
	}()
	defer func() {
		_ = conn.Close()
		a.mu.Lock()
		if a.heartbeatStop == stopCh {
			close(stopCh)
			a.heartbeatStop = nil
		}
		a.mu.Unlock()
		<-heartbeatDone
	}()

	// 读取事件循环
	for !a.stopped.Load() {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !a.stopped.Load() {
				logger.Error("Discord 读取消息出错", "error", err)
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

// sendResume 发送 op=6 Resume 恢复中断的会话。
//
// W3-3：携带 token / session_id / 最新 seq，让 Discord 重放断连期间漏掉的事件。
func (a *DiscordAdapter) sendResume(conn *websocket.Conn, sessionID string, seq int64) error {
	resume := map[string]any{
		"op": 6,
		"d": map[string]any{
			"token":      a.cfg.Token,
			"session_id": sessionID,
			"seq":        seq,
		},
	}
	return conn.WriteJSON(resume)
}

// heartbeat 定期发送心跳
func (a *DiscordAdapter) heartbeat(conn *websocket.Conn, interval time.Duration, stopCh <-chan struct{}) {
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
				logger.Error("Discord 心跳发送失败", "error", err)
				return
			}
		case <-stopCh:
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
		logger.Info("Discord 要求重连")
		a.mu.Lock()
		if a.conn != nil {
			a.conn.Close()
		}
		a.mu.Unlock()
	case 9: // Invalid Session
		// W3-3：此前完全丢弃 op=9，导致 Resume 被拒后仍反复用失效会话重连。
		// d 为布尔值，表示该会话是否可 Resume。不可 Resume 时清空 sessionID/seq，
		// 强制下次连接走全新 Identify；随后关闭连接触发 connectLoop 重连。
		a.handleInvalidSession(event.D)
		a.mu.Lock()
		if a.conn != nil {
			a.conn.Close()
		}
		a.mu.Unlock()
	}
}

// handleInvalidSession 处理 op=9 Invalid Session。
//
// W3-3：data 为 Discord 下发的布尔值，true 表示会话仍可 Resume（保留 sessionID/seq），
// false 表示会话已失效（清空 sessionID/seq，下次重连走全新 Identify）。
// 解析失败时按"不可 Resume"保守处理。
func (a *DiscordAdapter) handleInvalidSession(data json.RawMessage) {
	var resumable bool
	if err := json.Unmarshal(data, &resumable); err != nil {
		resumable = false
	}
	if resumable {
		logger.Info("Discord 会话失效但可恢复，保留 sessionID 等待 Resume")
		return
	}
	logger.Info("Discord 会话失效且不可恢复，清空会话状态走全新 Identify")
	a.mu.Lock()
	a.sessionID = ""
	a.mu.Unlock()
	a.seq.Store(0)
}

// handleDispatch 处理分发事件
func (a *DiscordAdapter) handleDispatch(eventType string, data json.RawMessage) {
	switch eventType {
	case "READY":
		var ready struct {
			SessionID string `json:"session_id"`
		}
		// W3-4：此前忽略 Unmarshal 返回值，解析失败会静默清空 sessionID，
		// 导致后续无法发送 Resume。解析失败时保留旧 sessionID 并记录日志。
		if err := json.Unmarshal(data, &ready); err != nil {
			logger.Error("Discord 解析 READY 事件失败", "error", err)
			return
		}
		a.sessionID = ready.SessionID
		logger.Info("Discord Bot 已就绪")

	case "MESSAGE_CREATE":
		a.handleMessageCreate(data)
	}
}

// handleMessageCreate 处理新消息
func (a *DiscordAdapter) handleMessageCreate(data json.RawMessage) {
	var msg discordMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		logger.Error("error", "error", err)
		return
	}

	// 忽略 Bot 自己的消息
	if msg.Author.Bot {
		return
	}

	// 转换为统一消息格式
	unified := &adapter.Message{
		ID:         "discord-" + idgen.ShortID(),
		Platform:   adapter.PlatformDiscord,
		InstanceID: a.Name(),
		ChatID:     msg.ChannelID,
		UserID:     msg.Author.ID,
		UserName:   msg.Author.Username,
		Content:    msg.Content,
		Timestamp:  time.Now(),
		Metadata: map[string]string{
			"guild_id":   msg.GuildID,
			"message_id": msg.ID,
		},
	}

	// 异步处理消息
	// v0.4.0 E5：Detach(baseCtx) 保留 logger/Values，但脱离 Start ctx cancel
	a.runHandler(func(workerCtx context.Context) {
		base := a.baseCtx
		if base == nil {
			base = context.Background()
		}
		ctx, cancel := context.WithTimeout(trace.Detach(base), 120*time.Second)
		defer cancel()
		stopCancel := context.AfterFunc(workerCtx, cancel)
		defer stopCancel()

		reply, err := a.handler(ctx, unified)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("Discord 消息处理失败", "error", err)
			errCtx, errCancel := context.WithTimeout(trace.Detach(base), 10*time.Second)
			defer errCancel()
			_ = a.Send(errCtx, msg.ChannelID, &adapter.Reply{Content: "处理消息时出错，请稍后重试。"})
			return
		}
		if reply != nil {
			sendCtx, sendCancel := context.WithTimeout(ctx, 30*time.Second)
			defer sendCancel()
			_ = a.Send(sendCtx, msg.ChannelID, reply)
		}
	})
}

func (a *DiscordAdapter) runHandler(fn func(context.Context)) bool {
	a.workerMu.Lock()
	defer a.workerMu.Unlock()
	if a.stopping {
		return false
	}
	workerCtx := a.workerCtx
	if workerCtx == nil {
		workerCtx = context.Background()
	}
	a.handlerWG.Add(1)
	go func() {
		defer a.handlerWG.Done()
		fn(workerCtx)
	}()
	return true
}

// ============== REST API ==============

// sendMessage 发送消息到频道
func (a *DiscordAdapter) sendMessage(ctx context.Context, channelID, content string) error {
	_, err := a.createMessage(ctx, channelID, content)
	return err
}

func (a *DiscordAdapter) sendReplyNow(ctx context.Context, channelID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	return a.sendMessage(ctx, channelID, reply.Content)
}

// createMessage 创建消息并返回消息 ID
func (a *DiscordAdapter) createMessage(ctx context.Context, channelID, content string) (string, error) {
	url := fmt.Sprintf("%s/channels/%s/messages", apiBase, channelID)

	// Discord 消息最大 2000 字符，超长时分段发送
	if utf8.RuneCountInString(content) > 2000 {
		runes := []rune(content)
		content = string(runes[:1997]) + "..."
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

	// W3-1：Discord POST /channels/{id}/messages 成功返回 201 Created，
	// 此前仅接受 200 会把正常成功误判为错误。按 2xx 整段判定成功。
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxDiscordErrorBodyBytes))
		return "", fmt.Errorf("discord API 错误 (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析 Discord 响应失败: %w", err)
	}
	return result.ID, nil
}

// editMessage 编辑已发送的消息（用于流式更新）
func (a *DiscordAdapter) editMessage(ctx context.Context, channelID, messageID, content string) error {
	url := fmt.Sprintf("%s/channels/%s/messages/%s", apiBase, channelID, messageID)

	if utf8.RuneCountInString(content) > 2000 {
		runes := []rune(content)
		content = string(runes[:1997]) + "..."
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
	defer resp.Body.Close()

	// W3-2：此前不校验响应状态码会静默吞掉 404/429/500 等失败，
	// 导致流式编辑失败被掩盖。非 2xx 时读取错误体并返回 error。
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxDiscordErrorBodyBytes))
		return fmt.Errorf("discord 编辑消息错误 (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ValidateConfig validates credentials by calling /users/@me.
func (a *DiscordAdapter) ValidateConfig(ctx context.Context) error {
	if a.cfg.Token == "" {
		return fmt.Errorf("discord bot token 未配置")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/users/@me", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+a.cfg.Token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord /users/@me 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxDiscordErrorBodyBytes))
		return fmt.Errorf("discord token 验证失败 (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// Health 返回当前连接健康状态。
func (a *DiscordAdapter) Health(_ context.Context) error {
	if a.cfg.Token == "" {
		return fmt.Errorf("discord bot token 不能为空")
	}
	if a.stopped.Load() {
		return fmt.Errorf("discord adapter stopped")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return fmt.Errorf("discord gateway 未连接")
	}
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
	ID        string      `json:"id"`
	ChannelID string      `json:"channel_id"`
	GuildID   string      `json:"guild_id"`
	Author    discordUser `json:"author"`
	Content   string      `json:"content"`
	Timestamp string      `json:"timestamp"`
}

// discordUser Discord 用户
type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}
