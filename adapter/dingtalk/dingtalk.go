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
	"encoding/base64"
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
	"github.com/hexagon-codes/toolkit/util/retry"
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
	streamWG     sync.WaitGroup
	streamWaiter adapter.LifecycleWaiter
	workerMu     sync.Mutex
	workers      sync.WaitGroup
	workerWaiter adapter.LifecycleWaiter
	workerCtx    context.Context
	workerCancel context.CancelFunc
	stopping     bool
	connected    atomic.Bool
	stopped      atomic.Bool

	// handlerTimeout 单条消息的处理总预算；0 → 默认 defaultHandlerTimeout。可注入以在测试中
	// 复现「占位已发 → handler 超时 → 返回 error」的终态路径，无需真等 2 分钟。
	handlerTimeout time.Duration
}

// defaultHandlerTimeout 是单条消息处理的默认总预算。
const defaultHandlerTimeout = 2 * time.Minute

// terminalNotifyTimeout 是终态用户通知（错误提示 / 占位撤回 / 最终答案）的兜底 ctx 预算。
const terminalNotifyTimeout = 15 * time.Second

// messageHandlerTimeout 返回处理总预算（handlerTimeout 未设时用默认）。
func (a *DingtalkAdapter) messageHandlerTimeout() time.Duration {
	if a.handlerTimeout > 0 {
		return a.handlerTimeout
	}
	return defaultHandlerTimeout
}

// terminalNotifyCtx 返回一个**独立于 handler ctx** 的兜底 context，用于终态用户通知。
// 终态通知是「必达副作用」：handler ctx 可能已因超时失效，派生自它的发送会在发送队列
// admission 处（send_queue.go 的 ctx.Err() 检查）静默失败。根在 Background 才能保证送达
// （对齐 send_queue_test.go 的 FIX 契约）。
func terminalNotifyCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), terminalNotifyTimeout)
}

// retryInitialStreamConnect 补齐官方 SDK AutoReconnect 的边界：SDK 仅在首次成功后的连接
// 断开时自动重连，首次 Start 失败会直接返回。这里持续重试首次握手，直到成功或实例停止。
func retryInitialStreamConnect(
	ctx context.Context,
	start func(context.Context) error,
	onFailure func(error),
	retryDelay func(int) time.Duration,
) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := start(ctx); err != nil {
			if onFailure != nil {
				onFailure(err)
			}
			timer := time.NewTimer(retryDelay(attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		return nil
	}
}

func initialStreamRetryDelay(attempt int) time.Duration {
	cfg := retry.DefaultConfig()
	return retry.ExponentialBackoff(attempt+1, cfg)
}

// New 创建钉钉适配器
func New(cfg config.DingtalkConfig) *DingtalkAdapter {
	workerCtx, workerCancel := context.WithCancel(context.Background())
	a := &DingtalkAdapter{cfg: cfg, workerCtx: workerCtx, workerCancel: workerCancel}
	a.queue = adapter.NewPlatformSendQueue(adapter.PlatformDingtalk, a.sendReplyNow)
	return a
}

// dingtalkThinkingFeedback 是「收到消息、正在处理」的占位提示。发送前先发它给用户即时反馈，
// 最终答案输出后再撤回（recall），使占位不残留（BUG-20260704：不删除占位、改为答案就位后撤回）。
const dingtalkThinkingFeedback = "⌨️ 已收到，正在思考…"

// dtMsgTypePicture 是钉钉图片消息的 msgtype——唯一允许经 downloadCode 进图片
// 多模态管道的富媒体类型（BUG-20260710：audio/video/file 同以 downloadCode 承载，
// 硬贴 image 附件会让 provider 400）。
const dtMsgTypePicture = "picture"

// dingtalkUnsupportedMediaFeedback 是非图片富媒体（语音/视频/文件）的降级提示（BUG-20260710）。
const dingtalkUnsupportedMediaFeedback = "⚠️ 暂不支持语音/视频/文件消息，请发送文字或图片。"

// dtMaxPictureBytes 是图片下载上限（防 OOM）；超限报错而非静默截断（BUG-20260710·审查 M-9）。
const dtMaxPictureBytes = 10 << 20

type dingtalkOpenAPI interface {
	GetAccessToken(ctx context.Context, appKey, appSecret string) (string, time.Duration, error)
	// SendOTO 发送单聊消息，返回 processQueryKey（钉钉消息标识，供后续撤回）。
	SendOTO(ctx context.Context, accessToken, robotCode, userID string, msg dingtalkOutboundMessage) (processQueryKey string, err error)
	// RecallOTO 按 processQueryKey 批量撤回此前主动发送的单聊消息。
	RecallOTO(ctx context.Context, accessToken, robotCode string, processQueryKeys []string) error
	// DownloadMessageFile 用消息 downloadCode 换取媒体文件的临时下载 URL
	//（BUG-20260709：picture 消息进多模态管道的前置步骤）。
	DownloadMessageFile(ctx context.Context, accessToken, robotCode, downloadCode string) (downloadURL string, err error)
}

// dingtalkOutboundMessage 是钉钉出站消息的传输形态：MsgKey 选择消息类型
// （sampleText / sampleMarkdown），MsgParam 为对应 JSON 载荷。消息类型策略在
// 适配器层决定，接口只负责传输（可测缝，BUG-20260703 B7）。
type dingtalkOutboundMessage struct {
	MsgKey   string
	MsgParam string
}

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

func (c *officialDingtalkOpenAPI) SendOTO(_ context.Context, accessToken, robotCode, userID string, msg dingtalkOutboundMessage) (string, error) {
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
		return "", err
	}
	if resp != nil && resp.StatusCode != nil && *resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("钉钉机器人发送返回 %d", *resp.StatusCode)
	}
	if resp != nil && resp.Body != nil {
		if len(resp.Body.InvalidStaffIdList) > 0 {
			return "", fmt.Errorf("钉钉机器人发送失败，无效用户: %s", joinStringPtrs(resp.Body.InvalidStaffIdList))
		}
		if len(resp.Body.FilteredStaffIdList) > 0 {
			return "", fmt.Errorf("钉钉机器人发送被过滤: %s", joinStringPtrs(resp.Body.FilteredStaffIdList))
		}
		if len(resp.Body.FlowControlledStaffIdList) > 0 {
			return "", fmt.Errorf("钉钉机器人发送被限流: %s", joinStringPtrs(resp.Body.FlowControlledStaffIdList))
		}
		if resp.Body.ProcessQueryKey != nil {
			return *resp.Body.ProcessQueryKey, nil
		}
	}
	return "", nil
}

// RecallOTO 按 processQueryKey 撤回此前发送的单聊消息（用于答案就位后撤回「正在思考」占位）。
func (c *officialDingtalkOpenAPI) RecallOTO(_ context.Context, accessToken, robotCode string, processQueryKeys []string) error {
	keys := make([]*string, 0, len(processQueryKeys))
	for _, k := range processQueryKeys {
		if k != "" {
			keys = append(keys, tea.String(k))
		}
	}
	if len(keys) == 0 {
		return nil
	}
	resp, err := c.robot.BatchRecallOTOWithOptions(
		(&dtrobot.BatchRecallOTORequest{}).
			SetRobotCode(robotCode).
			SetProcessQueryKeys(keys),
		(&dtrobot.BatchRecallOTOHeaders{}).SetXAcsDingtalkAccessToken(accessToken),
		c.runtime,
	)
	if err != nil {
		return err
	}
	if resp != nil && resp.StatusCode != nil && *resp.StatusCode != http.StatusOK {
		return fmt.Errorf("钉钉机器人撤回返回 %d", *resp.StatusCode)
	}
	return nil
}

// DownloadMessageFile 用 downloadCode 换媒体文件临时下载 URL
// （官方 robot/messageFiles/download API·BUG-20260709 picture 消息进管道的前置步骤）。
func (c *officialDingtalkOpenAPI) DownloadMessageFile(_ context.Context, accessToken, robotCode, downloadCode string) (string, error) {
	resp, err := c.robot.RobotMessageFileDownloadWithOptions(
		(&dtrobot.RobotMessageFileDownloadRequest{}).
			SetRobotCode(robotCode).
			SetDownloadCode(downloadCode),
		(&dtrobot.RobotMessageFileDownloadHeaders{}).SetXAcsDingtalkAccessToken(accessToken),
		c.runtime,
	)
	if err != nil {
		return "", err
	}
	if resp == nil || resp.Body == nil || resp.Body.DownloadUrl == nil || *resp.Body.DownloadUrl == "" {
		return "", fmt.Errorf("钉钉未返回下载 URL")
	}
	return *resp.Body.DownloadUrl, nil
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
func (a *DingtalkAdapter) Start(ctx context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	a.stopped.Store(false)

	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" {
		return fmt.Errorf("dingtalk app_key/app_secret 未配置")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.streamWaiter.Reset()
	a.workerWaiter.Reset()
	a.workerMu.Lock()
	a.stopping = false
	if a.workerCancel != nil {
		a.workerCancel()
	}
	a.workerCtx, a.workerCancel = context.WithCancel(ctx)
	a.workerMu.Unlock()

	cli := dtclient.NewStreamClient(
		dtclient.WithAppCredential(dtclient.NewAppCredentialConfig(a.cfg.AppKey, a.cfg.AppSecret)),
	)
	cli.RegisterChatBotCallbackRouter(a.onChatBotMessage)
	a.streamClient = cli
	streamCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.streamCancel != nil {
		a.streamCancel()
	}
	a.streamCancel = cancel
	a.mu.Unlock()

	// 官方 SDK 的 AutoReconnect 只覆盖首次成功后的断线；首次建连失败会直接返回。放到 goroutine
	// 中持续重试首次握手，成功后再由 SDK 接管断线重连。Start 保持非阻塞，失败原因同步供 Health 透出。
	a.streamWG.Add(1)
	go func() {
		defer a.streamWG.Done()
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
		err := retryInitialStreamConnect(streamCtx, cli.Start, func(err error) {
			a.connected.Store(false)
			if !a.stopped.Load() {
				logger.Error("钉钉 Stream 首次连接失败，将自动重试", "error", err)
				a.mu.Lock()
				a.lastError = err.Error()
				a.mu.Unlock()
			}
		}, initialStreamRetryDelay)
		if err != nil || a.stopped.Load() {
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
func (a *DingtalkAdapter) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.stopped.Store(true)
	a.connected.Store(false)
	a.workerMu.Lock()
	a.stopping = true
	if a.workerCancel != nil {
		a.workerCancel()
	}
	a.workerMu.Unlock()
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

	var stopErr error
	if err := a.streamWaiter.Wait(ctx, &a.streamWG); err != nil {
		stopErr = err
	}
	if err := a.workerWaiter.Wait(ctx, &a.workers); err != nil && stopErr == nil {
		stopErr = err
	}
	if a.queue != nil {
		if err := a.queue.Stop(ctx); err != nil && stopErr == nil {
			stopErr = err
		}
	}
	logger.Info("钉钉适配器停止中...", "name", a.Name())
	return stopErr
}

func (a *DingtalkAdapter) runWorker(fn func(context.Context)) bool {
	a.workerMu.Lock()
	defer a.workerMu.Unlock()
	if a.stopping {
		return false
	}
	workerCtx := a.workerCtx
	if workerCtx == nil {
		workerCtx = context.Background()
	}
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		fn(workerCtx)
	}()
	return true
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
	event.MsgType = data.Msgtype
	// picture 等富媒体的 downloadCode 在 Content（SDK 为 interface{}，按 map 取）
	// ——BUG-20260709：此前只拷贝 Text.Content，图片消息被静默丢弃、用户零回复。
	if m, ok := data.Content.(map[string]interface{}); ok {
		if code, ok := m["downloadCode"].(string); ok {
			event.Content.DownloadCode = code
		}
	}

	if strings.TrimSpace(event.Text.Content) != "" || event.Content.DownloadCode != "" {
		if !a.runWorker(func(ctx context.Context) { a.handleMessageContext(ctx, event) }) {
			return nil, fmt.Errorf("dingtalk adapter stopping")
		}
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

	// BUG-20260709：picture 消息正文为空但带 downloadCode，同样要进管道
	if event.Text.Content != "" || event.Content.DownloadCode != "" {
		if !a.runWorker(func(ctx context.Context) { a.handleMessageContext(ctx, event) }) {
			http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
			return
		}
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
	if _, err := api.SendOTO(ctx, token, a.cfg.RobotCode, chatID, dingtalkMarkdownMessage(reply.Content)); err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	return nil
}

// sendThinkingFeedback 发送「正在思考」占位并返回其 processQueryKey（供答案就位后撤回）。
// 直连 SendOTO（不过发送队列）以拿到消息标识；任何失败都返回空 key，调用方据此跳过撤回、不阻断主流程。
func (a *DingtalkAdapter) sendThinkingFeedback(ctx context.Context, chatID string) string {
	if strings.TrimSpace(chatID) == "" {
		return ""
	}
	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" || a.cfg.RobotCode == "" {
		return ""
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		logger.Error("[dingtalk] 思考占位取 Access Token 失败", "error", err)
		return ""
	}
	api, err := a.apiClient()
	if err != nil {
		logger.Error("[dingtalk] 思考占位初始化 SDK 失败", "error", err)
		return ""
	}
	key, err := api.SendOTO(ctx, token, a.cfg.RobotCode, chatID, dingtalkMarkdownMessage(dingtalkThinkingFeedback))
	if err != nil {
		logger.Error("[dingtalk] 发送思考占位失败", "error", err)
		return ""
	}
	return key
}

// recallThinkingFeedback 撤回先前发送的「正在思考」占位（processQueryKey 为空则跳过）。
// 撤回失败仅记录不阻断：占位残留是可接受降级，最终答案已送达。
func (a *DingtalkAdapter) recallThinkingFeedback(ctx context.Context, processQueryKey string) {
	if strings.TrimSpace(processQueryKey) == "" {
		return
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		logger.Error("[dingtalk] 撤回思考占位取 Access Token 失败", "error", err)
		return
	}
	api, err := a.apiClient()
	if err != nil {
		logger.Error("[dingtalk] 撤回思考占位初始化 SDK 失败", "error", err)
		return
	}
	if err := api.RecallOTO(ctx, token, a.cfg.RobotCode, []string{processQueryKey}); err != nil {
		logger.Error("[dingtalk] 撤回思考占位失败", "error", err)
	}
}

// dingtalkEmptyReplyFallback 是「正文为空」时的兜底文案（BUG-20260704）。
// 钉钉 sampleMarkdown 的 text 必填，空 text 会被硬拒 400 miss.param.markdownTotext。
// 空正文来源：推理型模型只产出 <think> 被 StripThinking 剥空、纯工具调用轮无正文、审核截断等。
const dingtalkEmptyReplyFallback = "⚠️ 本次没有生成有效内容，请重试。"

// dingtalkMarkdownMessage 构造 sampleMarkdown 出站消息（BUG-20260703 B7）。
//
// 钉钉 text 消息不渲染 markdown，LLM 产出的 ### 标题/加粗会裸露给用户；
// sampleMarkdown 原生渲染标题/加粗/链接/列表子集（纯文本内容按 markdown 发送
// 显示不变）。载荷为 {"title","text"}，title/text 均必填（钉钉硬约束）——title 从正文
// 首个非空行派生（有兜底），text 同理：正文为空/纯空白时用兜底文案，绝不产出空 text
// （BUG-20260704，与 title 兜底对称，使非法载荷在构造点即不可表达）。
func dingtalkMarkdownMessage(content string) dingtalkOutboundMessage {
	// LaTeX 数学降级（BUG-20260712-P，真机取证：解题回复「( 4.5 \times 2 = 9 )」原样漏给
	// 钉钉用户）：sampleMarkdown 渲染 markdown 子集但**不渲染 LaTeX**，出站前确定性转
	// Unicode 数学符号（×÷≤≥√ 等），不靠 prompt 恳求模型改写法。
	text := adapter.NormalizeMathText(content)
	if strings.TrimSpace(text) == "" {
		text = dingtalkEmptyReplyFallback
	}
	return dingtalkOutboundMessage{
		MsgKey:   "sampleMarkdown",
		MsgParam: marshalMarkdownContent(dingtalkMessageTitle(text), text),
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
	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给终端用户
	return a.Send(ctx, chatID, &adapter.Reply{Content: adapter.StripThinking(sb.String())})
}

// handleMessage 处理消息
func (a *DingtalkAdapter) handleMessage(event dtEvent) {
	a.handleMessageContext(context.Background(), event)
}

func (a *DingtalkAdapter) handleMessageContext(baseCtx context.Context, event dtEvent) {
	if a.handler == nil {
		return
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	content := strings.TrimSpace(event.Text.Content)

	ctx, cancel := context.WithTimeout(baseCtx, a.messageHandlerTimeout())
	defer cancel()

	// picture 消息（BUG-20260709）：downloadCode → 临时下载 URL → 图片字节 → image 附件，
	// 与桌面/web 同走 BuildMultimodalUserMessage 多模态管道（无 vision 模型时引擎有友好拒绝）。
	// 下载失败给用户明确提示，绝不静默丢弃。
	var attachments []adapter.Attachment
	if event.Content.DownloadCode != "" {
		// BUG-20260710：钉钉的语音/视频/文件回调同样以 content.downloadCode 承载。
		// 只有 msgtype=picture 才能走图片下载进多模态管道；其余类型硬贴 image 附件
		// 会让 provider 400、用户收到不可归因报错——此处给出明确"暂不支持"提示后返回。
		if event.MsgType != dtMsgTypePicture {
			logger.Warn("钉钉: 收到暂不支持的富媒体消息", "msgtype", event.MsgType)
			ntCtx, ntCancel := context.WithTimeout(ctx, 10*time.Second)
			defer ntCancel()
			_ = a.Send(ntCtx, event.SenderStaffId, &adapter.Reply{Content: dingtalkUnsupportedMediaFeedback})
			return
		}
		att, err := a.downloadPictureAttachment(ctx, event.Content.DownloadCode)
		if err != nil {
			logger.Error("钉钉: 下载图片消息失败", "error", err)
			errCtx, errCancel := context.WithTimeout(ctx, 10*time.Second)
			defer errCancel()
			_ = a.Send(errCtx, event.SenderStaffId, &adapter.Reply{Content: "⚠️ 图片获取失败，请重新发送一次。"})
			return
		}
		attachments = append(attachments, att)
	}
	if !adapter.HasMessageInput(content, attachments) {
		return
	}

	msg := &adapter.Message{
		ID:          "dt-" + idgen.ShortID(),
		Platform:    adapter.PlatformDingtalk,
		InstanceID:  a.Name(),
		ChatID:      event.SenderStaffId,
		UserID:      event.SenderStaffId,
		UserName:    event.SenderNick,
		Content:     content,
		Attachments: attachments,
		Timestamp:   time.Now(),
		Metadata: map[string]string{
			"conversation_id":   event.ConversationId,
			"conversation_type": event.ConversationType,
		},
	}

	// 发送前先发「正在思考」占位给用户即时反馈（BUG-20260704）。占位是对用户的一个**承诺**：
	// 无论成功 / 失败 / 超时，退出时都必须兑现（撤回），否则永久残留（BUG-20260713）。用 defer
	// 统一保证撤回覆盖所有 return 路径，并在**退出时刻**用独立兜底 ctx 执行（不能在 defer 注册处
	// 就创建 ctx——那样 15s 预算会在 2 分钟的 handler 跑完前就过期）。
	thinkingKey := a.sendThinkingFeedback(ctx, msg.ChatID)
	defer func() {
		rc, cancel := terminalNotifyCtx()
		defer cancel()
		a.recallThinkingFeedback(rc, thinkingKey)
	}()

	reply, err := a.handler(ctx, msg)
	if err != nil {
		// 不因 ctx 已超时而提前 return：超时正是用户最需要反馈之时（BUG-20260713）。终态错误
		// 提示用独立兜底 ctx 发送，绝不静默放弃；占位由上面的 defer 撤回。
		logger.Error("钉钉: 处理消息失败", "error", err)
		nc, cancel := terminalNotifyCtx()
		defer cancel()
		_ = a.Send(nc, msg.ChatID, &adapter.Reply{Content: "处理消息时出现错误，请稍后重试。"})
		return
	}
	if reply == nil {
		return // 占位由 defer 撤回
	}

	// 最终答案同为「必达」终态通知：用独立兜底 ctx，避免继承可能已耗尽的 handler ctx。
	sendCtx, sendCancel := terminalNotifyCtx()
	defer sendCancel()
	if err := a.Send(sendCtx, msg.ChatID, reply); err != nil {
		logger.Error("钉钉: 发送回复失败", "error", err)
	}
}

// downloadPictureAttachment 把 picture 消息的 downloadCode 兑换成 image 附件（BUG-20260709）：
// openAPI 换临时下载 URL → GET 图片字节（上限 dtMaxPictureBytes 防 OOM，超限报错不截断）
// → base64 + MIME 嗅探。
func (a *DingtalkAdapter) downloadPictureAttachment(ctx context.Context, downloadCode string) (adapter.Attachment, error) {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return adapter.Attachment{}, fmt.Errorf("获取 Access Token 失败: %w", err)
	}
	api, err := a.apiClient()
	if err != nil {
		return adapter.Attachment{}, fmt.Errorf("初始化钉钉官方 SDK 失败: %w", err)
	}
	url, err := api.DownloadMessageFile(ctx, token, a.cfg.RobotCode, downloadCode)
	if err != nil {
		return adapter.Attachment{}, fmt.Errorf("换取下载 URL 失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return adapter.Attachment{}, fmt.Errorf("构造下载请求失败: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return adapter.Attachment{}, fmt.Errorf("下载图片失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return adapter.Attachment{}, fmt.Errorf("下载图片失败: HTTP %d", resp.StatusCode)
	}
	// BUG-20260710（审查 M-9）：多读 1 字节探测超限——此前 LimitReader 恰好超限时
	// 静默截断产生坏 base64；现在超限返回明确错误，走"图片获取失败"用户反馈路径。
	data, err := io.ReadAll(io.LimitReader(resp.Body, dtMaxPictureBytes+1))
	if err != nil {
		return adapter.Attachment{}, fmt.Errorf("读取图片字节失败: %w", err)
	}
	if len(data) > dtMaxPictureBytes {
		return adapter.Attachment{}, fmt.Errorf("图片超过 %d MiB 上限，拒绝截断收取", dtMaxPictureBytes>>20)
	}
	if len(data) == 0 {
		return adapter.Attachment{}, fmt.Errorf("图片内容为空")
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		return adapter.Attachment{}, fmt.Errorf("下载内容 MIME %q 不是图片", mime)
	}
	return adapter.Attachment{
		Type: "image",
		Mime: mime,
		Name: "dingtalk-picture",
		Data: base64.StdEncoding.EncodeToString(data),
	}, nil
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
	// Content 承载富媒体载荷（BUG-20260709：picture 消息的 downloadCode 在此，
	// 此前未解析 → 图片消息正文为空被静默丢弃、用户零回复）。
	Content struct {
		DownloadCode string `json:"downloadCode"`
	} `json:"content"`
}

// marshalMarkdownContent 生成 sampleMarkdown 的 {"title","text"} 载荷
// （与 sampleText 的 {"content"} 结构不同，两者不可混用）。
func marshalMarkdownContent(title, text string) string {
	b, _ := json.Marshal(map[string]string{"title": title, "text": text})
	return string(b)
}
