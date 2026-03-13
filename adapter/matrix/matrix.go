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
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/toolkit/util/idgen"
)

// MatrixAdapter Matrix 协议适配器
type MatrixAdapter struct {
	config    Config
	handler   adapter.MessageHandler
	client    *http.Client
	stopCh    chan struct{}
	nextBatch string // sync 的 since token
}

// Config Matrix 适配器配置
type Config struct {
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
	return &MatrixAdapter{
		config: cfg,
		client: &http.Client{Timeout: time.Duration(cfg.SyncTimeout+10) * time.Second},
		stopCh: make(chan struct{}),
	}
}

func (a *MatrixAdapter) Name() string              { return "matrix" }
func (a *MatrixAdapter) Platform() adapter.Platform { return PlatformMatrix }

// Start 启动同步轮询
func (a *MatrixAdapter) Start(ctx context.Context, handler adapter.MessageHandler) error {
	a.handler = handler

	log.Printf("[Matrix] 连接到 %s", a.config.HomeserverURL)

	go a.syncLoop(ctx)
	return nil
}

// Stop 停止适配器
func (a *MatrixAdapter) Stop(_ context.Context) error {
	close(a.stopCh)
	return nil
}

// Send 发送消息到 Room
func (a *MatrixAdapter) Send(ctx context.Context, roomID string, reply *adapter.Reply) error {
	txnID := "hc_" + idgen.ShortID()
	url := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		a.config.HomeserverURL, roomID, txnID)

	payload := map[string]string{
		"msgtype": "m.text",
		"body":    reply.Content,
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
				log.Printf("[Matrix] 同步错误: %v", err)
				time.Sleep(5 * time.Second)
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

	a.nextBatch = syncResp.NextBatch

	// 处理 room 消息
	for roomID, room := range syncResp.Rooms.Join {
		for _, event := range room.Timeline.Events {
			a.handleEvent(roomID, event)
		}
	}

	return nil
}

// handleEvent 处理 Matrix 事件
func (a *MatrixAdapter) handleEvent(roomID string, event matrixEvent) {
	// 只处理文本消息，忽略自己发的
	if event.Type != "m.room.message" || event.Sender == a.config.UserID {
		return
	}

	msgType, _ := event.Content["msgtype"].(string)
	if msgType != "m.text" {
		return
	}

	body, _ := event.Content["body"].(string)
	if body == "" {
		return
	}

	msg := &adapter.Message{
		ID:        event.EventID,
		Platform:  PlatformMatrix,
		ChatID:    roomID,
		UserID:    event.Sender,
		Content:   body,
		Timestamp: time.UnixMilli(event.OriginServerTS),
	}

	go func(m *adapter.Message) {
		if a.handler != nil {
			reply, err := a.handler(context.Background(), m)
			if err != nil {
				log.Printf("[Matrix] 处理消息错误: %v", err)
				return
			}
			if reply != nil {
				if err := a.Send(context.Background(), m.ChatID, reply); err != nil {
					log.Printf("[Matrix] 发送回复错误: %v", err)
				}
			}
		}
	}(msg)
}

// Matrix 同步响应结构
type matrixSyncResponse struct {
	NextBatch string         `json:"next_batch"`
	Rooms     matrixRooms    `json:"rooms"`
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
