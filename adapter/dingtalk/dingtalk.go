// Package dingtalk 提供钉钉 Bot 适配器
//
// 通过钉钉官方 Stream SDK（dingtalk-stream-sdk-go）建立 WebSocket 长连接接收事件，无需公网地址。
// 回复与凭证校验通过钉钉官方 OpenAPI Go SDK 发送。
//
// 连接层整体委托官方 SDK，与飞书走官方 larkws SDK 同一路子：握手、票据协商、心跳、断线重连
// 均由官方 SDK 负责，应用层只注册机器人消息回调。官方 SDK 在未显式配置 proxy 时回退到 gorilla
// 的 websocket.DefaultDialer（Proxy=http.ProxyFromEnvironment），原生遵守 *_PROXY / NO_PROXY，
// 故被墙/需代理环境下亦能建连（根治历史「Stream 未连接」，参见 bug_20260628_stream_proxy_test.go）。
package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	dtoauth "github.com/alibabacloud-go/dingtalk/oauth2_1_0"
	dtrobot "github.com/alibabacloud-go/dingtalk/robot_1_0"
	util "github.com/alibabacloud-go/tea-utils/v2/service"
	"github.com/alibabacloud-go/tea/tea"
	dtchatbot "github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	dtclient "github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	sign_ "github.com/hexagon-codes/toolkit/crypto/sign"
	"github.com/hexagon-codes/toolkit/lang/stringx"
	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// DingtalkAdapter 钉钉 Bot 适配器
type DingtalkAdapter struct {
	cfg     config.DingtalkConfig
	handler adapter.MessageHandler
	queue   *adapter.SendQueue

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
	lastError   string // 最后一次 Stream 连接失败原因（mu 守护）；Health 在未连接时透出，便于定位 creds/网络/代理
	openAPI     dingtalkOpenAPI

	streamClient *dtclient.StreamClient
	streamCancel context.CancelFunc
	connected    atomic.Bool
	stopped      atomic.Bool
}

// New 创建钉钉适配器
func New(cfg config.DingtalkConfig) *DingtalkAdapter {
	a := &DingtalkAdapter{cfg: cfg}
	a.queue = adapter.NewPlatformSendQueue(adapter.PlatformDingtalk, a.sendReplyNow)
	return a
}

type dingtalkOpenAPI interface {
	GetAccessToken(ctx context.Context, appKey, appSecret string) (string, time.Duration, error)
	SendOTO(ctx context.Context, accessToken, robotCode, userID string, msg dingtalkOutboundMessage) error
}

// dingtalkOutboundMessage 是钉钉出站消息的传输形态：MsgKey 选择消息类型
// （sampleText / sampleMarkdown），MsgParam 为对应 JSON 载荷。消息类型策略在
// 适配器层决定，接口只负责传输（可测缝，BUG-20260703 B7）。
type dingtalkOutboundMessage struct {
	MsgKey   string
	MsgParam string
}

const dingtalkThinkingFeedback = "⌨️ 已收到，正在思考…"

type officialDingtalkOpenAPI struct {
	oauth   *dtoauth.Client
	robot   *dtrobot.Client
	runtime *util.RuntimeOptions
}

func newOfficialDingtalkOpenAPI() (*officialDingtalkOpenAPI, error) {
	cfg := &openapi.Config{
		Protocol:       tea.String("https"),
		Endpoint:       tea.String("api.dingtalk.com"),
		ConnectTimeout: tea.Int(10_000),
		ReadTimeout:    tea.Int(10_000),
	}
	oauthClient, err := dtoauth.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	robotClient, err := dtrobot.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &officialDingtalkOpenAPI{
		oauth: oauthClient,
		robot: robotClient,
		runtime: &util.RuntimeOptions{
			Autoretry:      tea.Bool(false),
			ConnectTimeout: tea.Int(10_000),
			ReadTimeout:    tea.Int(10_000),
		},
	}, nil
}

func (a *DingtalkAdapter) apiClient() (dingtalkOpenAPI, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.openAPI != nil {
		return a.openAPI, nil
	}
	client, err := newOfficialDingtalkOpenAPI()
	if err != nil {
		return nil, err
	}
	a.openAPI = client
	return client, nil
}

func (c *officialDingtalkOpenAPI) GetAccessToken(_ context.Context, appKey, appSecret string) (string, time.Duration, error) {
	resp, err := c.oauth.GetAccessTokenWithOptions(
		(&dtoauth.GetAccessTokenRequest{}).
			SetAppKey(appKey).
			SetAppSecret(appSecret),
		map[string]*string{},
		c.runtime,
	)
	if err != nil {
		return "", 0, err
	}
	if resp == nil || resp.Body == nil || resp.Body.AccessToken == nil || *resp.Body.AccessToken == "" {
		return "", 0, fmt.Errorf("钉钉 Access Token 响应为空")
	}
	if resp.StatusCode != nil && *resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("钉钉 Access Token 返回 %d", *resp.StatusCode)
	}
	expire := 2 * time.Hour
	if resp.Body.ExpireIn != nil && *resp.Body.ExpireIn > 0 {
		expire = time.Duration(*resp.Body.ExpireIn) * time.Second
	}
	return *resp.Body.AccessToken, expire, nil
}

func (c *officialDingtalkOpenAPI) SendOTO(_ context.Context, accessToken, robotCode, userID string, msg dingtalkOutboundMessage) error {
	resp, err := c.robot.BatchSendOTOWithOptions(
		(&dtrobot.BatchSendOTORequest{}).
			SetRobotCode(robotCode).
			SetUserIds([]*string{tea.String(userID)}).
			SetMsgKey(msg.MsgKey).
			SetMsgParam(msg.MsgParam),
		(&dtrobot.BatchSendOTOHeaders{}).SetXAcsDingtalkAccessToken(accessToken),
		c.runtime,
	)
	if err != nil {
		return err
	}
	if resp != nil && resp.StatusCode != nil && *resp.StatusCode != http.StatusOK {
		return fmt.Errorf("钉钉机器人发送返回 %d", *resp.StatusCode)
	}
	if resp != nil && resp.Body != nil {
		if len(resp.Body.InvalidStaffIdList) > 0 {
			return fmt.Errorf("钉钉机器人发送失败，无效用户: %s", joinStringPtrs(resp.Body.InvalidStaffIdList))
		}
		if len(resp.Body.FilteredStaffIdList) > 0 {
			return fmt.Errorf("钉钉机器人发送被过滤: %s", joinStringPtrs(resp.Body.FilteredStaffIdList))
		}
		if len(resp.Body.FlowControlledStaffIdList) > 0 {
			return fmt.Errorf("钉钉机器人发送被限流: %s", joinStringPtrs(resp.Body.FlowControlledStaffIdList))
		}
	}
	return nil
}

func joinStringPtrs(values []*string) string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != nil && *v != "" {
			out = append(out, *v)
		}
	}
	return strings.Join(out, ",")
}

func (a *DingtalkAdapter) Name() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return "dingtalk"
}
func (a *DingtalkAdapter) Platform() adapter.Platform { return adapter.PlatformDingtalk }

// Start 启动钉钉 Stream 长连接（使用官方 dingtalk-stream-sdk-go）
func (a *DingtalkAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	a.stopped.Store(false)

	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" {
		return fmt.Errorf("dingtalk app_key/app_secret 未配置")
	}

	cli := dtclient.NewStreamClient(
		dtclient.WithAppCredential(dtclient.NewAppCredentialConfig(a.cfg.AppKey, a.cfg.AppSecret)),
	)
	cli.RegisterChatBotCallbackRouter(a.onChatBotMessage)
	a.streamClient = cli
	streamCtx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	if a.streamCancel != nil {
		a.streamCancel()
	}
	a.streamCancel = cancel
	a.mu.Unlock()

	// 官方 SDK 的 Start 在首次建连成功后即返回（内部起 processLoop + 自动重连），失败才返回 error。
	// 放到 goroutine 中执行：与飞书一致保持 Start 非阻塞，并把建连结果记入 connected/lastError 供
	// Health 透出（首次建连失败是用户「点击测试报 Stream 未连接」最常见的场景）。
	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.connected.Store(false)
				errMsg := fmt.Sprintf("dingtalk Stream panic: %v", r)
				a.mu.Lock()
				a.lastError = errMsg
				a.mu.Unlock()
				if !a.stopped.Load() {
					logger.Error("钉钉 Stream 连接 panic", "error", errMsg)
				}
			}
		}()
		logger.Info("钉钉适配器 Stream 连接启动中", "name", a.Name())
		if err := cli.Start(streamCtx); err != nil {
			a.connected.Store(false)
			if !a.stopped.Load() {
				logger.Error("钉钉 Stream 连接失败", "error", err)
				a.mu.Lock()
				a.lastError = err.Error()
				a.mu.Unlock()
			}
			return
		}
		a.connected.Store(true)
		a.mu.Lock()
		a.lastError = ""
		a.mu.Unlock()
		logger.Info("钉钉 Stream 连接已建立", "name", a.Name())
	}()

	logger.Info("钉钉适配器已启动", "name", a.Name())
	return nil
}

// Stop 停止钉钉适配器
func (a *DingtalkAdapter) Stop(_ context.Context) error {
	a.stopped.Store(true)
	a.connected.Store(false)
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	a.mu.Lock()
	cancel := a.streamCancel
	a.streamCancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	// 官方 SDK 默认 AutoReconnect=true：其 processLoop 在连接断开（含 Close 触发的读失败）后会
	// `go reconnect()` 无限重连。关停前必须先把 AutoReconnect 置 false，否则 Close 后 SDK 仍会
	// 疯狂重连 → goroutine 泄漏。
	if a.streamClient != nil {
		a.streamClient.AutoReconnect = false
		a.streamClient.Close()
	}

	logger.Info("钉钉适配器停止中...", "name", a.Name())
	return nil
}

// Handler 返回 HTTP Handler（保留向后兼容，Stream 模式下不使用）
func (a *DingtalkAdapter) Handler() http.Handler {
	return http.HandlerFunc(a.handleWebhook)
}

// ============== Stream 长连接（官方 SDK 回调）==============

// onChatBotMessage 是注册到官方 SDK 的机器人消息回调。
//
// 立即返回成功 ack（空串），消息处理异步进行：钉钉要求在限定时间内收到 ack，否则会重投；而
// handleMessage 内含完整 LLM 往返（可达分钟级），绝不能在 ack 路径上同步阻塞。语义对齐飞书的
// `go a.handleSDKMessage(...)` 即时返回。
func (a *DingtalkAdapter) onChatBotMessage(_ context.Context, data *dtchatbot.BotCallbackDataModel) ([]byte, error) {
	if data == nil {
		return []byte(""), nil
	}

	event := dtEvent{
		ConversationId:   data.ConversationId,
		ConversationType: data.ConversationType,
		SenderStaffId:    data.SenderStaffId,
		SenderNick:       data.SenderNick,
	}
	event.Text.Content = data.Text.Content

	if strings.TrimSpace(event.Text.Content) != "" {
		go a.handleMessage(event)
	}
	return []byte(""), nil
}

// handleWebhook 处理钉钉回调（向后兼容 HTTP Webhook）
func (a *DingtalkAdapter) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// BUG-20260611: cap webhook body to 1 MiB — external callers must not
	// be able to OOM the sidecar with an unbounded payload.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// Distinguish an oversized payload (413) from a generic read error (400).
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	timestamp := r.Header.Get("timestamp")
	sign := r.Header.Get("sign")
	if a.cfg.AppSecret != "" && !a.verifySign(timestamp, sign) {
		http.Error(w, "name", http.StatusUnauthorized)
		return
	}

	var event dtEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "error", http.StatusBadRequest)
		return
	}

	if event.Text.Content != "" {
		go a.handleMessage(event)
	}

	w.WriteHeader(http.StatusOK)
}

// ============== 消息处理 ==============

// Send 发送消息
//
// v0.4.0 F6：reply.Interactive 非空且 flag interactive.render.v1 OFF 时，
// 自动追加文本 fallback 让按钮/选项/审批/卡片在钉钉基础可用。
func (a *DingtalkAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	adapter.MaybeApplyTextFallback(ctx, reply)
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

func (a *DingtalkAdapter) sendReplyNow(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("获取 Access Token 失败: %w", err)
	}

	api, err := a.apiClient()
	if err != nil {
		return fmt.Errorf("初始化钉钉官方 SDK 失败: %w", err)
	}
	if err := api.SendOTO(ctx, token, a.cfg.RobotCode, chatID, dingtalkMarkdownMessage(reply.Content)); err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	return nil
}

// dingtalkMarkdownMessage 构造 sampleMarkdown 出站消息（BUG-20260703 B7）。
//
// 钉钉 text 消息不渲染 markdown，LLM 产出的 ### 标题/加粗会裸露给用户；
// sampleMarkdown 原生渲染标题/加粗/链接/列表子集（纯文本内容按 markdown 发送
// 显示不变）。载荷为 {"title","text"}，title 必填（用作推送通知摘要），从正文
// 首个非空行派生并剥掉 markdown 标记。
func dingtalkMarkdownMessage(content string) dingtalkOutboundMessage {
	return dingtalkOutboundMessage{
		MsgKey:   "sampleMarkdown",
		MsgParam: marshalMarkdownContent(dingtalkMessageTitle(content), content),
	}
}

// dingtalkMessageTitle 从正文派生 sampleMarkdown 必填的 title：取首个非空行，
// 剥掉行首 markdown 标记（# > - * 及空白），按 rune 截断防超长。
func dingtalkMessageTitle(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#>-*+ \t"))
		if line != "" {
			return stringx.Truncate(line, 30)
		}
	}
	return "小蟹回复"
}

// SendStream 流式发送（拼接后一次性发送）
func (a *DingtalkAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给家长
	return a.Send(ctx, chatID, &adapter.Reply{Content: adapter.StripThinking(sb.String())})
}

// handleMessage 处理消息
func (a *DingtalkAdapter) handleMessage(event dtEvent) {
	if a.handler == nil {
		return
	}

	content := strings.TrimSpace(event.Text.Content)
	if content == "" {
		return
	}

	msg := &adapter.Message{
		ID:         "dt-" + idgen.ShortID(),
		Platform:   adapter.PlatformDingtalk,
		InstanceID: a.Name(),
		ChatID:     event.SenderStaffId,
		UserID:     event.SenderStaffId,
		UserName:   event.SenderNick,
		Content:    content,
		Timestamp:  time.Now(),
		Metadata: map[string]string{
			"conversation_id":   event.ConversationId,
			"conversation_type": event.ConversationType,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	a.sendThinkingFeedback(ctx, msg.ChatID)

	reply, err := a.handler(ctx, msg)
	if err != nil {
		logger.Error("钉钉: 处理消息失败", "error", err)
		errCtx, errCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer errCancel()
		_ = a.Send(errCtx, msg.ChatID, &adapter.Reply{Content: "处理消息时出现错误，请稍后重试。"})
		return
	}
	if reply == nil {
		return
	}

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()
	if err := a.Send(sendCtx, msg.ChatID, reply); err != nil {
		logger.Error("钉钉: 发送回复失败", "error", err)
	}
}

func (a *DingtalkAdapter) sendThinkingFeedback(parent context.Context, chatID string) {
	if strings.TrimSpace(chatID) == "" {
		return
	}
	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" || a.cfg.RobotCode == "" {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	if err := a.Send(ctx, chatID, &adapter.Reply{Content: dingtalkThinkingFeedback}); err != nil {
		logger.Error("[dingtalk] 发送思考反馈失败", "error", err)
	}
}

// ============== Token 管理 ==============

// getAccessToken 获取钉钉 Access Token（带缓存）
func (a *DingtalkAdapter) getAccessToken(ctx context.Context) (string, error) {
	a.mu.RLock()
	if a.accessToken != "" && time.Now().Before(a.tokenExpiry.Add(-5*time.Minute)) {
		token := a.accessToken
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.accessToken != "" && time.Now().Before(a.tokenExpiry.Add(-5*time.Minute)) {
		return a.accessToken, nil
	}

	api := a.openAPI
	if api == nil {
		var err error
		api, err = newOfficialDingtalkOpenAPI()
		if err != nil {
			return "", fmt.Errorf("初始化钉钉官方 SDK 失败: %w", err)
		}
		a.openAPI = api
	}
	token, ttl, err := api.GetAccessToken(ctx, a.cfg.AppKey, a.cfg.AppSecret)
	if err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	a.accessToken = token
	a.tokenExpiry = time.Now().Add(ttl)
	return a.accessToken, nil
}

// verifySign 验证钉钉签名（用于向后兼容 Webhook 模式）
// v0.3.12 C2：复用 toolkit/crypto/sign 的常量时间比较实现
func (a *DingtalkAdapter) verifySign(timestamp, sign string) bool {
	if timestamp == "" || sign == "" {
		return false
	}
	stringToSign := timestamp + "\n" + a.cfg.AppSecret
	return sign_.VerifyHMACSHA256Base64([]byte(stringToSign), []byte(a.cfg.AppSecret), sign)
}

// Health 返回适配器健康状态。
func (a *DingtalkAdapter) ValidateConfig(ctx context.Context) error {
	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" || a.cfg.RobotCode == "" {
		return fmt.Errorf("dingtalk app_key/app_secret/robot_code 未配置")
	}
	if _, err := a.getAccessToken(ctx); err != nil {
		return fmt.Errorf("dingtalk 凭证验证失败: %w", err)
	}
	return nil
}

func (a *DingtalkAdapter) Health(_ context.Context) error {
	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" || a.cfg.RobotCode == "" {
		return fmt.Errorf("dingtalk app_key/app_secret/robot_code 未配置")
	}
	if a.handler == nil {
		return fmt.Errorf("dingtalk handler 未附加")
	}
	if a.stopped.Load() {
		return fmt.Errorf("dingtalk adapter stopped")
	}
	if !a.connected.Load() {
		// 透出真实失败原因（creds/网络/代理），而非 opaque「Stream 未连接」(BUG-20260628)。
		a.mu.RLock()
		lastErr := a.lastError
		a.mu.RUnlock()
		if lastErr != "" {
			return fmt.Errorf("dingtalk Stream 未连接: %s", lastErr)
		}
		return fmt.Errorf("dingtalk Stream 未连接（连接中，请稍候重试）")
	}
	return nil
}

// ============== 数据模型 ==============

// dtEvent 钉钉消息事件
type dtEvent struct {
	ConversationId   string `json:"conversationId"`
	ConversationType string `json:"conversationType"`
	SenderStaffId    string `json:"senderStaffId"`
	SenderNick       string `json:"senderNick"`
	Text             struct {
		Content string `json:"content"`
	} `json:"text"`
	MsgType string `json:"msgtype"`
}

// marshalMarkdownContent 生成 sampleMarkdown 的 {"title","text"} 载荷
// （与 sampleText 的 {"content"} 结构不同，两者不可混用）。
func marshalMarkdownContent(title, text string) string {
	b, _ := json.Marshal(map[string]string{"title": title, "text": text})
	return string(b)
}
