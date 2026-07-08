// Package matrix 提供 Matrix 协议适配器
//
// Matrix 是去中心化、开放标准的即时通讯协议。
// 适用于自建服务器场景（如 Element/Synapse），注重隐私和数据主权。
//
// 接入方式：
//   - 使用 Matrix Client-Server API
//   - 长轮询（/sync）接收消息
//   - REST API 发送消息
package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hexagon-codes/toolkit/util/logger"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/toolkit/net/httpx"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// MatrixAdapter Matrix 协议适配器
type MatrixAdapter struct {
	config    Config
	handler   adapter.MessageHandler
	client    *http.Client
	queue     *adapter.SendQueue
	stopCh    chan struct{}
	nextBatch string // sync 的 since token
	stopped   atomic.Bool
}

// Config Matrix 适配器配置
type Config struct {
	Name          string `yaml:"name"`
	HomeserverURL string `yaml:"homeserver_url"` // Homeserver URL（如 https://matrix.org）
	AccessToken   string `yaml:"access_token"`   // Bot 的 Access Token
	UserID        string `yaml:"user_id"`        // Bot 的 User ID（如 @bot:matrix.org）
	SyncTimeout   int    `yaml:"sync_timeout"`   // 长轮询超时（秒），默认 30
}

// PlatformMatrix Matrix 平台常量
const PlatformMatrix adapter.Platform = "matrix"

// New 创建 Matrix 适配器
func New(cfg Config) *MatrixAdapter {
	if cfg.SyncTimeout == 0 {
		cfg.SyncTimeout = 30
	}
	a := &MatrixAdapter{
		config: cfg,
		client: httpx.RawClient(httpx.WithRawTimeout(time.Duration(cfg.SyncTimeout+10) * time.Second)),
		stopCh: make(chan struct{}),
	}
	a.queue = adapter.NewPlatformSendQueue(PlatformMatrix, a.sendReplyNow)
	return a
}

func (a *MatrixAdapter) Name() string {
	if a.config.Name != "" {
		return a.config.Name
	}
	return "matrix"
}
func (a *MatrixAdapter) Platform() adapter.Platform { return PlatformMatrix }

// Start 启动同步轮询
func (a *MatrixAdapter) Start(ctx context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	a.stopped.Store(false)

	logger.Info("[Matrix] 连接到", "homeserver_url", a.config.HomeserverURL)

	go a.syncLoop(ctx)
	return nil
}

// Stop 停止适配器。
//
// 幂等：用 stopped 标志的 CAS 守卫 close(stopCh)，二次调用（热重载/优雅停机重入）
// 直接返回，避免 close 已关闭 channel 触发 panic。
func (a *MatrixAdapter) Stop(_ context.Context) error {
	if !a.stopped.CompareAndSwap(false, true) {
		return nil // 已停止，幂等返回
	}
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	close(a.stopCh)
	return nil
}

// Send 发送消息到 Room
func (a *MatrixAdapter) Send(ctx context.Context, roomID string, reply *adapter.Reply) error {
	if a.queue == nil {
		return a.sendReplyNow(ctx, roomID, reply)
	}
	return a.queue.Send(ctx, roomID, reply)
}

func (a *MatrixAdapter) sendReplyNow(ctx context.Context, roomID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	txnID := "hc_" + idgen.ShortID()
	url := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		a.config.HomeserverURL, roomID, txnID)

	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给终端用户
	payload := map[string]string{
		"msgtype": "m.text",
		"body":    adapter.StripThinking(reply.Content),
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.config.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("matrix API 返回 %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SendStream 流式发送（Matrix 不支持流式，降级为完整发送）
func (a *MatrixAdapter) SendStream(ctx context.Context, roomID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	return a.Send(ctx, roomID, &adapter.Reply{Content: sb.String()})
}

// syncMinInterval 是 syncLoop 成功路径上的最小轮询间隔。
//
// W3-16: Matrix /sync 是服务端长轮询，正常情况下会阻塞 SyncTimeout 秒才返回；
// 但当服务端忽略 timeout 立即返回空响应（或网络异常导致快速返回）时，无节流的
// for 循环会以最高速度空转，瞬间打满 CPU 并对 homeserver 形成请求风暴。
// 在成功路径上加一个很小的最小间隔，既不影响正常长轮询的实时性，又能为这类
// 退化场景兜底。错误路径已有 5s 退避，无需此节流。
const syncMinInterval = 50 * time.Millisecond

// syncLoop 同步轮询循环
func (a *MatrixAdapter) syncLoop(ctx context.Context) {
	for {
		select {
		case <-a.stopCh:
			return
		case <-ctx.Done():
			return
		default:
			if err := a.doSync(ctx); err != nil {
				logger.Error("[Matrix] 同步错误", "error", err)
				time.Sleep(5 * time.Second)
				continue
			}
			// W3-16: 成功路径节流，防止服务端快速返回空响应时 CPU 空转。
			// 用 select 等待，使 Stop/ctx 取消仍能即时打断、保持优雅停机响应性。
			select {
			case <-a.stopCh:
				return
			case <-ctx.Done():
				return
			case <-time.After(syncMinInterval):
			}
		}
	}
}

// doSync 执行一次同步请求
func (a *MatrixAdapter) doSync(ctx context.Context) error {
	url := fmt.Sprintf("%s/_matrix/client/v3/sync?timeout=%d",
		a.config.HomeserverURL, a.config.SyncTimeout*1000)
	if a.nextBatch != "" {
		url += "&since=" + a.nextBatch
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("创建同步请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.config.AccessToken)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("同步请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("同步 API 返回 %d: %s", resp.StatusCode, string(body))
	}

	var syncResp matrixSyncResponse
	if err := json.NewDecoder(resp.Body).Decode(&syncResp); err != nil {
		return fmt.Errorf("解析同步响应失败: %w", err)
	}

	// W3-15: 仅在响应携带有效 next_batch 时才推进游标。
	// 空响应（如 {} 或无 next_batch）若无条件覆盖，会把已有增量游标重置为空串，
	// 导致下次 sync 丢失游标、退回全量重新拉取整个会话历史。
	if syncResp.NextBatch != "" {
		a.nextBatch = syncResp.NextBatch
	}

	// 处理 room 消息
	for roomID, room := range syncResp.Rooms.Join {
		for _, event := range room.Timeline.Events {
			a.handleEvent(ctx, roomID, event)
		}
	}

	return nil
}

// handleEvent 处理 Matrix 事件
func (a *MatrixAdapter) handleEvent(ctx context.Context, roomID string, event matrixEvent) {
	// 只处理文本消息，忽略自己发的
	if event.Type != "m.room.message" || event.Sender == a.config.UserID {
		return
	}

	msgType, _ := event.Content["msgtype"].(string)
	if msgType != "m.text" {
		return
	}

	body, _ := event.Content["body"].(string)
	// W3-17: 纯空白/换行/制表符的 body 等价于空消息，TrimSpace 后判空，
	// 避免空白消息浪费一次下游 LLM 调用。
	if strings.TrimSpace(body) == "" {
		return
	}

	msg := &adapter.Message{
		ID:         event.EventID,
		Platform:   PlatformMatrix,
		InstanceID: a.Name(),
		ChatID:     roomID,
		UserID:     event.Sender,
		Content:    body,
		Timestamp:  time.UnixMilli(event.OriginServerTS),
	}

	go func(m *adapter.Message) {
		if a.handler == nil {
			return
		}
		// H7: Detach(syncCtx) 保留 logger，脱离 sync loop cancel 避免消息处理半途被杀
		bgCtx, cancel := context.WithTimeout(trace.Detach(ctx), 2*time.Minute)
		defer cancel()
		reply, err := a.handler(bgCtx, m)
		if err != nil {
			logger.Error("[Matrix] 处理消息错误", "error", err)
			return
		}
		if reply != nil {
			if err := a.Send(bgCtx, m.ChatID, reply); err != nil {
				logger.Error("[Matrix] 发送回复错误", "error", err)
			}
		}
	}(msg)
}

// ValidateConfig validates credentials by calling /account/whoami.
func (a *MatrixAdapter) ValidateConfig(ctx context.Context) error {
	if a.config.HomeserverURL == "" {
		return fmt.Errorf("matrix homeserver_url 未配置")
	}
	if a.config.AccessToken == "" {
		return fmt.Errorf("matrix access_token 未配置")
	}
	if a.config.UserID == "" {
		return fmt.Errorf("matrix user_id 未配置")
	}
	url := a.config.HomeserverURL + "/_matrix/client/v3/account/whoami"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.config.AccessToken)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("matrix whoami 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("matrix 凭证验证失败 (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// Health 返回适配器健康状态。
func (a *MatrixAdapter) Health(_ context.Context) error {
	if a.handler == nil {
		return fmt.Errorf("matrix handler 未附加")
	}
	if a.config.HomeserverURL == "" || a.config.AccessToken == "" || a.config.UserID == "" {
		return fmt.Errorf("matrix homeserver/access_token/user_id 未配置")
	}
	if a.stopped.Load() {
		return fmt.Errorf("matrix adapter stopped")
	}
	return nil
}

// Matrix 同步响应结构
type matrixSyncResponse struct {
	NextBatch string      `json:"next_batch"`
	Rooms     matrixRooms `json:"rooms"`
}

type matrixRooms struct {
	Join map[string]matrixJoinedRoom `json:"join"`
}

type matrixJoinedRoom struct {
	Timeline matrixTimeline `json:"timeline"`
}

type matrixTimeline struct {
	Events []matrixEvent `json:"events"`
}

type matrixEvent struct {
	Type           string         `json:"type"`
	EventID        string         `json:"event_id"`
	Sender         string         `json:"sender"`
	OriginServerTS int64          `json:"origin_server_ts"`
	Content        map[string]any `json:"content"`
}
