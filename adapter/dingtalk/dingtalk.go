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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
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
	"github.com/hexagon-codes/hexclaw/messagecontent"
	sign_ "github.com/hexagon-codes/toolkit/crypto/sign"
	"github.com/hexagon-codes/toolkit/lang/stringx"
	"github.com/hexagon-codes/toolkit/util/logger"
	"github.com/hexagon-codes/toolkit/util/retry"
)

// InboundPhotoAdmissionPort 在平台 ACK 之前接收已经下载完成的 direct 图片消息。
// 返回 handled=true 表示消息已进入外部耐久队列，适配器不得再启动旧业务 handler。
type InboundPhotoAdmissionPort interface {
	AdmitInboundPhoto(context.Context, *adapter.Message) (handled bool, err error)
}

// DingtalkAdapter 钉钉 Bot 适配器
type DingtalkAdapter struct {
	cfg     config.DingtalkConfig
	handler adapter.MessageHandler
	queue   *adapter.SendQueue

	inboundPhotoAdmission InboundPhotoAdmissionPort // mu 守护

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

// photoHandlerTimeout 给整页识题 + 多题批改留出真实预算。注入耐久接纳端口后，图片回调
// 只同步等待下载与落盘；后续 worker 不阻塞钉钉，用户会先收到带 ETA 的进度消息。
const photoHandlerTimeout = 10 * time.Minute

// terminalNotifyTimeout 是终态用户通知（错误提示 / 占位撤回 / 最终答案）的兜底 ctx 预算。
const terminalNotifyTimeout = 45 * time.Second

// messageHandlerTimeout 返回处理总预算（handlerTimeout 未设时用默认）。
func (a *DingtalkAdapter) messageHandlerTimeout() time.Duration {
	if a.handlerTimeout > 0 {
		return a.handlerTimeout
	}
	return defaultHandlerTimeout
}

func (a *DingtalkAdapter) messageHandlerTimeoutFor(event dtEvent) time.Duration {
	if a.handlerTimeout > 0 {
		return a.handlerTimeout
	}
	if event.MsgType == dtMsgTypePicture {
		return photoHandlerTimeout
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

// SetInboundPhotoAdmissionPort 注入 ACK 前的图片耐久接纳端口；nil 保持非 K12 消费者的原行为。
func (a *DingtalkAdapter) SetInboundPhotoAdmissionPort(port InboundPhotoAdmissionPort) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.inboundPhotoAdmission = port
	a.mu.Unlock()
}

func (a *DingtalkAdapter) currentInboundPhotoAdmissionPort() InboundPhotoAdmissionPort {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.inboundPhotoAdmission
}

// dingtalkThinkingFeedback 是「收到消息、正在处理」的占位提示。发送前先发它给用户即时反馈，
// 最终答案输出后再撤回（recall），使占位不残留（BUG-20260704：不删除占位、改为答案就位后撤回）。
const dingtalkThinkingFeedback = "⌨️ 已收到，正在思考…"

const dingtalkPhotoProcessingFeedback = "📷 已收到图片，正在识别和处理。通常预计 2–5 分钟；题量或图片内容较多时约 5–10 分钟。完成后会把处理结果（如有批改图）发回当前对话。"

func thinkingFeedbackForEvent(event dtEvent) string {
	if event.MsgType == dtMsgTypePicture {
		return dingtalkPhotoProcessingFeedback
	}
	return dingtalkThinkingFeedback
}

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

type dingtalkGroupOpenAPI interface {
	SendGroup(ctx context.Context, accessToken, robotCode, conversationID string, msg dingtalkOutboundMessage) (processQueryKey string, err error)
	RecallGroup(ctx context.Context, accessToken, robotCode, conversationID string, processQueryKeys []string) error
}

// dingtalkReceiptOpenAPI is kept optional so existing transport fakes and
// older providers retain the base send contract. The official implementation
// exposes BatchOTOQuery and therefore satisfies it in production.
type dingtalkReceiptOpenAPI interface {
	QueryOTO(ctx context.Context, accessToken, robotCode, processQueryKey string) (sendStatus string, err error)
}

type dingtalkMediaOpenAPI interface {
	UploadImage(ctx context.Context, accessToken string, attachment adapter.Attachment) (mediaID string, err error)
}

type dingtalkFileMediaOpenAPI interface {
	UploadFile(ctx context.Context, accessToken string, attachment adapter.Attachment) (mediaID string, err error)
}

// dingtalkOutboundMessage 是钉钉出站消息的传输形态：MsgKey 选择消息类型
// （sampleText / sampleMarkdown），MsgParam 为对应 JSON 载荷。消息类型策略在
// 适配器层决定，接口只负责传输（可测缝，BUG-20260703 B7）。
type dingtalkOutboundMessage struct {
	MsgKey   string
	MsgParam string
}

type dingtalkDefiniteProviderRejection interface {
	definiteDingTalkProviderRejection()
}

type dingtalkDefiniteProviderRejectionError struct {
	cause error
}

func (e *dingtalkDefiniteProviderRejectionError) Error() string {
	if e == nil || e.cause == nil {
		return "DingTalk provider rejected the request"
	}
	return e.cause.Error()
}

func (e *dingtalkDefiniteProviderRejectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (*dingtalkDefiniteProviderRejectionError) definiteDingTalkProviderRejection() {}

func markDingTalkDefiniteProviderRejection(err error) error {
	if err == nil {
		return nil
	}
	var rejection dingtalkDefiniteProviderRejection
	if errors.As(err, &rejection) {
		return err
	}
	return &dingtalkDefiniteProviderRejectionError{cause: err}
}

func dingTalkReceiptFailureStatus(providerSendStarted bool, err error) adapter.DeliveryStatus {
	if !providerSendStarted {
		return adapter.DeliveryFailed
	}
	var rejection dingtalkDefiniteProviderRejection
	if errors.As(err, &rejection) {
		return adapter.DeliveryFailed
	}
	return adapter.DeliveryOutcomeUnknown
}

func isDingTalkDefiniteProviderRejectionStatus(status int) bool {
	switch status {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusUnprocessableEntity,
		http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// groupQueueTargetPrefix lets the existing per-adapter SendQueue serialize
// both OTO and group replies without exposing conversation type in the public
// adapter.Send signature. A NUL-prefixed value cannot collide with a DingTalk
// staff ID/openConversationId.
const groupQueueTargetPrefix = "\x00dingtalk-group:"

func groupQueueTarget(conversationID string) string {
	return groupQueueTargetPrefix + conversationID
}

func parseGroupQueueTarget(target string) (string, bool) {
	if !strings.HasPrefix(target, groupQueueTargetPrefix) {
		return "", false
	}
	return strings.TrimPrefix(target, groupQueueTargetPrefix), true
}

type officialDingtalkOpenAPI struct {
	oauth    *dtoauth.Client
	robot    *dtrobot.Client
	runtime  *util.RuntimeOptions
	http     *http.Client
	mediaURL string
}

const dingtalkMediaUploadURL = "https://oapi.dingtalk.com/media/upload"

// dingtalkMaxOutboundImageBytes follows DingTalk's robot image limit while also
// preventing an accidentally huge in-memory attachment from being decoded and
// copied into a multipart body.
const dingtalkMaxOutboundImageBytes = 20 << 20

const dingtalkMaxOutboundPDFBytes = 20 << 20

type dingtalkMediaUploadResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	MediaID string `json:"media_id"`
}

// uploadDingtalkImage uploads an in-memory corrected worksheet to DingTalk's
// media endpoint. The returned media_id can be embedded directly in a
// sampleMarkdown image expression, so no public object-storage URL is needed.
func uploadDingtalkImage(
	ctx context.Context,
	httpClient *http.Client,
	endpoint string,
	accessToken string,
	attachment adapter.Attachment,
) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if httpClient == nil {
		return "", errors.New("钉钉图片上传 HTTP 客户端为空")
	}
	if !adapter.IsImageAttachment(attachment) || isDingTalkPDFAttachment(attachment) {
		return "", fmt.Errorf("钉钉仅支持上传图片附件: %s", attachment.Name)
	}
	raw, err := dingTalkAttachmentBytes(attachment)
	if err != nil {
		return "", err
	}

	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("钉钉图片上传地址无效: %q", endpoint)
	}
	query := u.Query()
	query.Set("access_token", accessToken)
	query.Set("type", "image")
	u.RawQuery = query.Encode()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("type", "image"); err != nil {
		return "", fmt.Errorf("构造钉钉图片上传参数失败: %w", err)
	}
	name := safeDingTalkAttachmentName(attachment.Name)
	part, err := writer.CreateFormFile("media", name)
	if err != nil {
		return "", fmt.Errorf("构造钉钉图片上传文件失败: %w", err)
	}
	if _, err := part.Write(raw); err != nil {
		return "", fmt.Errorf("写入钉钉图片上传文件失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("完成钉钉图片上传表单失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return "", fmt.Errorf("创建钉钉图片上传请求失败: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	uploadClient := *httpClient
	uploadClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := uploadClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传钉钉图片失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return "", errors.New("DingTalk image media upload redirects are forbidden")
	}

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("读取钉钉图片上传响应失败: %w", err)
	}
	var result dingtalkMediaUploadResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("解析钉钉图片上传响应失败（HTTP %d）: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || result.ErrCode != 0 {
		return "", fmt.Errorf("钉钉图片上传失败（HTTP %d, errcode=%d）: %s", resp.StatusCode, result.ErrCode, result.ErrMsg)
	}
	if strings.TrimSpace(result.MediaID) == "" {
		return "", errors.New("钉钉图片上传成功但 media_id 为空")
	}
	return result.MediaID, nil
}

func uploadDingtalkPDFFile(
	ctx context.Context,
	httpClient *http.Client,
	endpoint string,
	accessToken string,
	attachment adapter.Attachment,
) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if httpClient == nil {
		return "", errors.New("DingTalk file upload HTTP client is unavailable")
	}
	if strings.TrimSpace(accessToken) == "" {
		return "", errors.New("DingTalk file upload access token is required")
	}
	raw, err := dingTalkAttachmentBytes(attachment)
	if err != nil {
		return "", err
	}
	if !isDingTalkPDFAttachment(attachment) || !bytes.HasPrefix(raw, []byte("%PDF-")) {
		return "", errors.New("DingTalk file attachment must be a valid application/pdf document")
	}
	if len(raw) > dingtalkMaxOutboundPDFBytes {
		return "", fmt.Errorf("DingTalk PDF attachment exceeds the %d MiB limit", dingtalkMaxOutboundPDFBytes>>20)
	}

	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("DingTalk file upload endpoint must be a valid HTTPS URL")
	}
	query := u.Query()
	query.Set("access_token", accessToken)
	query.Set("type", "file")
	u.RawQuery = query.Encode()

	fileName := dingTalkPDFFileName(attachment)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("media", fileName)
	if err != nil {
		return "", fmt.Errorf("DingTalk file upload multipart creation failed: %w", err)
	}
	if _, err := part.Write(raw); err != nil {
		return "", fmt.Errorf("DingTalk file upload multipart write failed: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("DingTalk file upload multipart completion failed: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return "", fmt.Errorf("DingTalk file upload request creation failed: %w", err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	uploadClient := *httpClient
	uploadClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := uploadClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("DingTalk file media upload failed: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("DingTalk file media upload response read failed: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("DingTalk file media upload failed with HTTP %d", response.StatusCode)
	}
	var result dingtalkMediaUploadResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("DingTalk file media upload response is invalid: %w", err)
	}
	if result.ErrCode != 0 {
		return "", fmt.Errorf("DingTalk file media upload failed with errcode %d: %s", result.ErrCode, result.ErrMsg)
	}
	mediaID := strings.TrimSpace(result.MediaID)
	if !validDingTalkFileMediaID(mediaID) {
		return "", errors.New("DingTalk file media upload returned an invalid media ID")
	}
	return mediaID, nil
}

func dingtalkFileMessage(mediaID string, fileName string) dingtalkOutboundMessage {
	payload, _ := json.Marshal(struct {
		MediaID  string `json:"mediaId"`
		FileName string `json:"fileName"`
		FileType string `json:"fileType"`
	}{MediaID: mediaID, FileName: fileName, FileType: "pdf"})
	return dingtalkOutboundMessage{MsgKey: "sampleFile", MsgParam: string(payload)}
}

func dingTalkPDFFileName(attachment adapter.Attachment) string {
	if strings.TrimSpace(attachment.Name) == "" {
		return "document.pdf"
	}
	return safeDingTalkAttachmentName(attachment.Name)
}

func validDingTalkFileMediaID(mediaID string) bool {
	trimmed := strings.TrimSpace(mediaID)
	return strings.HasPrefix(trimmed, "@") && len(trimmed) > 1 && len(trimmed) <= 512 &&
		!strings.ContainsAny(trimmed, "\r\n\t /\\:[]()")
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
		oauth:    oauthClient,
		robot:    robotClient,
		http:     &http.Client{Timeout: 20 * time.Second},
		mediaURL: dingtalkMediaUploadURL,
		runtime: &util.RuntimeOptions{
			Autoretry:      tea.Bool(false),
			ConnectTimeout: tea.Int(10_000),
			ReadTimeout:    tea.Int(10_000),
		},
	}, nil
}

func (c *officialDingtalkOpenAPI) SendGroup(_ context.Context, accessToken, robotCode, conversationID string, msg dingtalkOutboundMessage) (string, error) {
	resp, err := c.robot.OrgGroupSendWithOptions(
		(&dtrobot.OrgGroupSendRequest{}).
			SetRobotCode(robotCode).
			SetOpenConversationId(conversationID).
			SetMsgKey(msg.MsgKey).
			SetMsgParam(msg.MsgParam),
		(&dtrobot.OrgGroupSendHeaders{}).SetXAcsDingtalkAccessToken(accessToken),
		c.runtime,
	)
	if err != nil {
		return "", err
	}
	if resp != nil && resp.StatusCode != nil && *resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("钉钉机器人群发送返回 %d", *resp.StatusCode)
	}
	if resp != nil && resp.Body != nil && resp.Body.ProcessQueryKey != nil {
		return *resp.Body.ProcessQueryKey, nil
	}
	return "", nil
}

func (c *officialDingtalkOpenAPI) RecallGroup(_ context.Context, accessToken, robotCode, conversationID string, processQueryKeys []string) error {
	keys := make([]*string, 0, len(processQueryKeys))
	for _, key := range processQueryKeys {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, tea.String(key))
		}
	}
	if len(keys) == 0 {
		return nil
	}
	resp, err := c.robot.OrgGroupRecallWithOptions(
		(&dtrobot.OrgGroupRecallRequest{}).
			SetRobotCode(robotCode).
			SetOpenConversationId(conversationID).
			SetProcessQueryKeys(keys),
		(&dtrobot.OrgGroupRecallHeaders{}).SetXAcsDingtalkAccessToken(accessToken),
		c.runtime,
	)
	if err != nil {
		return err
	}
	if resp != nil && resp.StatusCode != nil && *resp.StatusCode != http.StatusOK {
		return fmt.Errorf("钉钉机器人群撤回返回 %d", *resp.StatusCode)
	}
	if resp != nil && resp.Body != nil && len(resp.Body.FailedResult) > 0 {
		return fmt.Errorf("钉钉机器人群撤回部分失败: %v", resp.Body.FailedResult)
	}
	return nil
}

func (c *officialDingtalkOpenAPI) UploadImage(ctx context.Context, accessToken string, attachment adapter.Attachment) (string, error) {
	return uploadDingtalkImage(ctx, c.http, c.mediaURL, accessToken, attachment)
}

func (c *officialDingtalkOpenAPI) UploadFile(ctx context.Context, accessToken string, attachment adapter.Attachment) (string, error) {
	return uploadDingtalkPDFFile(ctx, c.http, c.mediaURL, accessToken, attachment)
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
		var sdkErr *tea.SDKError
		if errors.As(err, &sdkErr) && sdkErr.StatusCode != nil &&
			isDingTalkDefiniteProviderRejectionStatus(*sdkErr.StatusCode) {
			return "", markDingTalkDefiniteProviderRejection(err)
		}
		return "", err
	}
	if resp != nil && resp.StatusCode != nil &&
		isDingTalkDefiniteProviderRejectionStatus(int(*resp.StatusCode)) {
		return "", markDingTalkDefiniteProviderRejection(
			fmt.Errorf("钉钉机器人发送返回 %d", *resp.StatusCode),
		)
	}
	if resp != nil && resp.Body != nil {
		if len(resp.Body.InvalidStaffIdList) > 0 {
			return "", markDingTalkDefiniteProviderRejection(
				fmt.Errorf("钉钉机器人发送失败，无效用户: %s", joinStringPtrs(resp.Body.InvalidStaffIdList)),
			)
		}
		if len(resp.Body.FilteredStaffIdList) > 0 {
			return "", markDingTalkDefiniteProviderRejection(
				fmt.Errorf("钉钉机器人发送被过滤: %s", joinStringPtrs(resp.Body.FilteredStaffIdList)),
			)
		}
		if len(resp.Body.FlowControlledStaffIdList) > 0 {
			return "", markDingTalkDefiniteProviderRejection(
				fmt.Errorf("钉钉机器人发送被限流: %s", joinStringPtrs(resp.Body.FlowControlledStaffIdList)),
			)
		}
		if resp.Body.ProcessQueryKey != nil {
			return *resp.Body.ProcessQueryKey, nil
		}
	}
	return "", nil
}

func (c *officialDingtalkOpenAPI) QueryOTO(_ context.Context, accessToken, robotCode, processQueryKey string) (string, error) {
	resp, err := c.robot.BatchOTOQueryWithOptions(
		(&dtrobot.BatchOTOQueryRequest{}).
			SetRobotCode(robotCode).
			SetProcessQueryKey(processQueryKey),
		(&dtrobot.BatchOTOQueryHeaders{}).SetXAcsDingtalkAccessToken(accessToken),
		c.runtime,
	)
	if err != nil {
		return "", err
	}
	if resp != nil && resp.StatusCode != nil && *resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("钉钉机器人回执查询返回 %d", *resp.StatusCode)
	}
	if resp == nil || resp.Body == nil || resp.Body.SendStatus == nil || strings.TrimSpace(*resp.Body.SendStatus) == "" {
		return "", fmt.Errorf("钉钉机器人回执查询未返回 sendStatus")
	}
	return *resp.Body.SendStatus, nil
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
// 普通消息在启动异步处理后立即返回成功 ACK（空串）。注入图片耐久接纳端口时，direct 图片只在
// 下载和耐久接纳成功后 ACK；完整 LLM 往返仍由恢复型 worker 执行，绝不进入 ACK 同步路径。
func (a *DingtalkAdapter) onChatBotMessage(ctx context.Context, data *dtchatbot.BotCallbackDataModel) ([]byte, error) {
	if data == nil {
		return []byte(""), nil
	}
	if data.ConversationType == "2" {
		logger.Info("DingTalk group message ignored because v0.5 supports direct messages only")
		return []byte(""), nil
	}
	if strings.TrimSpace(data.MsgId) == "" {
		return nil, errors.New("dingtalk inbound message is missing provider message id")
	}

	event := dtEvent{
		MsgID:            data.MsgId,
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
		handled, err := a.admitInboundPhotoBeforeACK(ctx, &event)
		if err != nil {
			return nil, err
		}
		if handled {
			return []byte(""), nil
		}
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
	if event.ConversationType == "2" {
		logger.Info("DingTalk group message ignored because v0.5 supports direct messages only")
		w.WriteHeader(http.StatusOK)
		return
	}
	if strings.TrimSpace(event.MsgID) == "" {
		http.Error(w, "missing provider message id", http.StatusBadRequest)
		return
	}

	// BUG-20260709：picture 消息正文为空但带 downloadCode，同样要进管道
	if event.Text.Content != "" || event.Content.DownloadCode != "" {
		handled, err := a.admitInboundPhotoBeforeACK(r.Context(), &event)
		if err != nil {
			http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
			return
		}
		if handled {
			w.WriteHeader(http.StatusOK)
			return
		}
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
	if _, isGroup := parseGroupQueueTarget(chatID); isGroup {
		return errors.New("DingTalk v0.5 supports direct messages only")
	}
	adapter.MaybeApplyTextFallback(ctx, reply)
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		return fmt.Errorf("钉钉渲染协议校验失败: %w", err)
	}
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

// SendWithReceipt sends through the same per-adapter queue as ordinary
// messages and returns DingTalk's processQueryKey. A key is only provider
// acceptance evidence; delivered is established later by QueryReceipt.
func (a *DingtalkAdapter) SendWithReceipt(ctx context.Context, chatID string, reply *adapter.Reply) (adapter.DeliveryAck, error) {
	if _, isGroup := parseGroupQueueTarget(chatID); isGroup {
		return adapter.DeliveryAck{Status: adapter.DeliveryFailed}, fmt.Errorf("钉钉投递回执只支持一对一私聊")
	}
	if reply == nil {
		return adapter.DeliveryAck{Status: adapter.DeliveryFailed}, errors.New("dingtalk: reply is required")
	}
	adapter.MaybeApplyTextFallback(ctx, reply)
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		return adapter.DeliveryAck{Status: adapter.DeliveryFailed}, fmt.Errorf("钉钉渲染协议校验失败: %w", err)
	}
	var externalID string
	var providerSendStarted atomic.Bool
	send := func(sendCtx context.Context, target string, candidate *adapter.Reply) error {
		var err error
		externalID, err = a.sendReplyNowWithReceipt(
			sendCtx,
			target,
			candidate,
			func() { providerSendStarted.Store(true) },
		)
		return err
	}
	var err error
	if a.queue == nil {
		err = send(ctx, chatID, reply)
	} else {
		err = a.queue.SendWith(ctx, chatID, reply, send)
	}
	if err != nil {
		return adapter.DeliveryAck{
			Status: dingTalkReceiptFailureStatus(providerSendStarted.Load(), err),
		}, err
	}
	if strings.TrimSpace(externalID) == "" {
		return adapter.DeliveryAck{Status: adapter.DeliveryOutcomeUnknown}, fmt.Errorf("钉钉已受理发送但未返回 processQueryKey，结果待核实")
	}
	return adapter.DeliveryAck{ExternalMessageID: externalID, Status: adapter.DeliveryAccepted}, nil
}

// PrepareDeliveryPartResource 只准备单个媒体 part 的平台资源，不发送用户可见消息。
func (a *DingtalkAdapter) PrepareDeliveryPartResource(ctx context.Context, part adapter.DeliveryPart) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateDingTalkDeliveryPartCanonicalEvidence(part, true); err != nil {
		return "", err
	}
	switch part.Kind {
	case messagecontent.PartMarkdown:
		if part.Attachment != nil || strings.TrimSpace(part.PreparedResourceID) != "" || strings.TrimSpace(part.Text) == "" {
			return "", errors.New("DingTalk markdown delivery part has an invalid shape")
		}
		return "", nil
	case messagecontent.PartArtifact:
		if strings.TrimSpace(part.Text) != "" || part.Attachment == nil || strings.TrimSpace(part.PreparedResourceID) != "" {
			return "", errors.New("DingTalk artifact delivery part has an invalid shape")
		}
	default:
		return "", fmt.Errorf("DingTalk delivery part kind %q is unsupported", part.Kind)
	}

	attachment := *part.Attachment
	if err := validateDingTalkDeliveryPartAttachment(part, attachment, true); err != nil {
		return "", err
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("DingTalk delivery part access token failed: %w", err)
	}
	api, err := a.apiClient()
	if err != nil {
		return "", fmt.Errorf("DingTalk delivery part API initialization failed: %w", err)
	}

	var mediaID string
	if isDingTalkPDFAttachment(attachment) {
		fileAPI, ok := api.(dingtalkFileMediaOpenAPI)
		if !ok {
			return "", errors.New("DingTalk file media upload capability is unavailable")
		}
		mediaID, err = fileAPI.UploadFile(ctx, token, attachment)
	} else {
		imageAPI, ok := api.(dingtalkMediaOpenAPI)
		if !ok {
			return "", errors.New("DingTalk image media upload capability is unavailable")
		}
		mediaID, err = imageAPI.UploadImage(ctx, token, attachment)
	}
	if err != nil {
		return "", fmt.Errorf("DingTalk delivery part media upload failed: %w", err)
	}
	if !validDingTalkFileMediaID(mediaID) {
		return "", errors.New("DingTalk delivery part media upload returned an invalid media ID")
	}
	return strings.TrimSpace(mediaID), nil
}

// SendPreparedPartWithReceipt 只消费已经准备好的媒体引用，并返回钉钉 processQueryKey。
func (a *DingtalkAdapter) SendPreparedPartWithReceipt(ctx context.Context, chatID string, part adapter.DeliveryPart) (adapter.DeliveryAck, error) {
	failed := adapter.DeliveryAck{Status: adapter.DeliveryFailed}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(chatID) == "" {
		return failed, errors.New("DingTalk delivery part chat ID is required")
	}
	if _, isGroup := parseGroupQueueTarget(chatID); isGroup {
		return failed, errors.New("DingTalk delivery parts support direct messages only")
	}
	if err := validateDingTalkDeliveryPartCanonicalEvidence(part, true); err != nil {
		return failed, err
	}
	message, err := dingTalkPreparedPartMessage(part)
	if err != nil {
		return failed, err
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return failed, fmt.Errorf("DingTalk delivery part access token failed: %w", err)
	}
	api, err := a.apiClient()
	if err != nil {
		return failed, fmt.Errorf("DingTalk delivery part API initialization failed: %w", err)
	}

	var externalID string
	var providerSendStarted atomic.Bool
	send := func(sendCtx context.Context, target string, _ *adapter.Reply) error {
		if err := sendCtx.Err(); err != nil {
			return err
		}
		providerSendStarted.Store(true)
		var sendErr error
		externalID, sendErr = api.SendOTO(sendCtx, token, a.cfg.RobotCode, target, message)
		return sendErr
	}
	if a.queue == nil {
		err = send(ctx, chatID, &adapter.Reply{})
	} else {
		err = a.queue.SendWith(ctx, chatID, &adapter.Reply{}, send)
	}
	if err != nil {
		return adapter.DeliveryAck{
			Status: dingTalkReceiptFailureStatus(providerSendStarted.Load(), err),
		}, err
	}
	if strings.TrimSpace(externalID) == "" {
		return adapter.DeliveryAck{Status: adapter.DeliveryOutcomeUnknown}, errors.New("DingTalk accepted the delivery part without a processQueryKey")
	}
	return adapter.DeliveryAck{ExternalMessageID: externalID, Status: adapter.DeliveryAccepted}, nil
}

// SendPreparedEnvelopeWithReceipt 把同一规范内容的 Markdown 与已准备图片引用合成为一条消息。
// 所有校验都在获取凭证和调用 provider 之前完成，发送阶段不会重新上传图片。
func (a *DingtalkAdapter) SendPreparedEnvelopeWithReceipt(
	ctx context.Context,
	chatID string,
	envelope adapter.PreparedEnvelope,
) (adapter.DeliveryAck, error) {
	failed := adapter.DeliveryAck{Status: adapter.DeliveryFailed}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(chatID) == "" {
		return failed, errors.New("DingTalk prepared envelope chat ID is required")
	}
	if _, isGroup := parseGroupQueueTarget(chatID); isGroup {
		return failed, errors.New("DingTalk prepared envelopes support direct messages only")
	}
	message, err := dingTalkPreparedEnvelopeMessage(envelope)
	if err != nil {
		return failed, err
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return failed, fmt.Errorf("DingTalk prepared envelope access token failed: %w", err)
	}
	api, err := a.apiClient()
	if err != nil {
		return failed, fmt.Errorf("DingTalk prepared envelope API initialization failed: %w", err)
	}

	var externalID string
	var providerSendStarted atomic.Bool
	send := func(sendCtx context.Context, target string, _ *adapter.Reply) error {
		if err := sendCtx.Err(); err != nil {
			return err
		}
		providerSendStarted.Store(true)
		var sendErr error
		externalID, sendErr = api.SendOTO(sendCtx, token, a.cfg.RobotCode, target, message)
		return sendErr
	}
	if a.queue == nil {
		err = send(ctx, chatID, &adapter.Reply{})
	} else {
		err = a.queue.SendWith(ctx, chatID, &adapter.Reply{}, send)
	}
	if err != nil {
		return adapter.DeliveryAck{
			Status: dingTalkReceiptFailureStatus(providerSendStarted.Load(), err),
		}, err
	}
	if strings.TrimSpace(externalID) == "" {
		return adapter.DeliveryAck{Status: adapter.DeliveryOutcomeUnknown}, errors.New("DingTalk accepted the prepared envelope without a processQueryKey")
	}
	return adapter.DeliveryAck{ExternalMessageID: externalID, Status: adapter.DeliveryAccepted}, nil
}

// ValidatePreparedEnvelope 只构造并校验钉钉组合消息，不取凭证、不上传、不发送。
func (a *DingtalkAdapter) ValidatePreparedEnvelope(envelope adapter.PreparedEnvelope) error {
	_, err := dingTalkPreparedEnvelopeMessage(envelope)
	return err
}

func validateDingTalkDeliveryPartAttachment(part adapter.DeliveryPart, attachment adapter.Attachment, requireBytes bool) error {
	partMIME := strings.TrimSpace(part.MIME)
	attachmentMIME := strings.TrimSpace(attachment.Mime)
	if partMIME == "" || attachmentMIME == "" || !strings.EqualFold(partMIME, attachmentMIME) {
		return errors.New("DingTalk artifact delivery part MIME does not match its attachment")
	}
	if !isDingTalkPDFAttachment(attachment) && !strings.HasPrefix(strings.ToLower(attachmentMIME), "image/") {
		return errors.New("DingTalk artifact delivery part type is unsupported")
	}
	if err := validateDingTalkAttachmentName(attachment.Name); err != nil {
		return err
	}
	if !requireBytes {
		return nil
	}
	digest, _, err := dingTalkAttachmentIdentity(attachment)
	if err != nil {
		return err
	}
	if strings.TrimSpace(part.Digest) == "" || part.Digest != digest {
		return errors.New("DingTalk artifact delivery part digest does not match its attachment bytes")
	}
	return nil
}

func dingTalkPreparedPartMessage(part adapter.DeliveryPart) (dingtalkOutboundMessage, error) {
	switch part.Kind {
	case messagecontent.PartMarkdown:
		if part.Attachment != nil || strings.TrimSpace(part.PreparedResourceID) != "" || strings.TrimSpace(part.Text) == "" {
			return dingtalkOutboundMessage{}, errors.New("DingTalk markdown delivery part has an invalid shape")
		}
		if err := validateDingTalkVisibleContent(part.Text); err != nil {
			return dingtalkOutboundMessage{}, err
		}
		return dingtalkMarkdownMessage(part.Text), nil
	case messagecontent.PartArtifact:
		if strings.TrimSpace(part.Text) != "" || part.Attachment == nil || !validDingTalkFileMediaID(part.PreparedResourceID) {
			return dingtalkOutboundMessage{}, errors.New("DingTalk artifact delivery part has an invalid shape")
		}
		attachment := *part.Attachment
		if err := validateDingTalkDeliveryPartAttachment(part, attachment, false); err != nil {
			return dingtalkOutboundMessage{}, err
		}
		mediaID := strings.TrimSpace(part.PreparedResourceID)
		if isDingTalkPDFAttachment(attachment) {
			return dingtalkFileMessage(mediaID, dingTalkPDFFileName(attachment)), nil
		}
		alt := strings.TrimSpace(attachment.Name)
		if alt == "" {
			alt = "image"
		}
		return dingtalkMarkdownMessage("![" + alt + "](" + mediaID + ")"), nil
	default:
		return dingtalkOutboundMessage{}, fmt.Errorf("DingTalk delivery part kind %q is unsupported", part.Kind)
	}
}

func dingTalkPreparedEnvelopeMessage(envelope adapter.PreparedEnvelope) (dingtalkOutboundMessage, error) {
	if len(envelope.Parts) < 2 {
		return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope requires Markdown and at least one image")
	}

	first := envelope.Parts[0]
	if err := validateDingTalkDeliveryPartCanonicalEvidence(first, true); err != nil {
		return dingtalkOutboundMessage{}, err
	}
	if first.Kind != messagecontent.PartMarkdown || first.Ordinal != 1 || first.Attachment != nil ||
		strings.TrimSpace(first.PreparedResourceID) != "" || strings.TrimSpace(first.Text) == "" {
		return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope must start with one Markdown part")
	}
	canonical := first.MessageContent
	manifest := first.RenderManifest
	if len(manifest.Parts) != len(canonical.Attachments)+1 {
		return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope canonical attachment projection is incomplete")
	}
	imageCount := 0
	seenPDF := false
	for index := 1; index < len(manifest.Parts); index++ {
		projected := manifest.Parts[index]
		attachment := canonical.Attachments[index-1]
		if projected.Kind != messagecontent.PartArtifact ||
			projected.ArtifactRef != attachment.AssetID ||
			projected.ArtifactDigest != attachment.Digest ||
			projected.AltText != attachment.AltText {
			return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope canonical artifact projection is invalid")
		}
		switch mime := strings.ToLower(strings.TrimSpace(attachment.MIME)); {
		case strings.HasPrefix(mime, "image/"):
			if seenPDF {
				return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope images must precede trailing PDF artifacts")
			}
			imageCount++
		case mime == "application/pdf":
			seenPDF = true
		default:
			return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope canonical artifact type is unsupported")
		}
	}
	if imageCount == 0 {
		return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope requires at least one canonical image")
	}
	if len(envelope.Parts) != imageCount+1 {
		return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope must cover the complete canonical image prefix")
	}
	contentID := first.MessageContent.ContentID
	renderID := first.RenderManifest.RenderID
	content := first.Text

	for index := 1; index < len(envelope.Parts); index++ {
		part := envelope.Parts[index]
		if err := validateDingTalkDeliveryPartCanonicalEvidence(part, true); err != nil {
			return dingtalkOutboundMessage{}, err
		}
		if part.MessageContent.ContentID != contentID || part.RenderManifest.RenderID != renderID {
			return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope parts must share canonical render evidence")
		}
		if part.Ordinal != index+1 || part.Kind != messagecontent.PartArtifact || part.Attachment == nil ||
			strings.TrimSpace(part.Text) != "" {
			return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope image parts are out of order")
		}
		attachment := *part.Attachment
		if isDingTalkPDFAttachment(attachment) || !adapter.IsImageAttachment(attachment) {
			return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope supports image artifacts only")
		}
		if err := validateDingTalkDeliveryPartAttachment(part, attachment, false); err != nil {
			return dingtalkOutboundMessage{}, err
		}
		imageRef := strings.TrimSpace(part.PreparedResourceID)
		if !validDingtalkImageReference(imageRef) {
			return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope image reference is invalid")
		}
		alt := strings.TrimSpace(manifest.Parts[index].AltText)
		if alt == "" {
			alt = "作品原图"
		}
		if hasUnsafeDingTalkMarkdownLabelCharacters(alt) {
			return dingtalkOutboundMessage{}, errors.New("DingTalk prepared envelope image alt text contains unsafe Markdown characters")
		}
		content += "\n\n![" + alt + "](" + imageRef + ")"
	}
	if err := validateDingTalkVisibleContent(content); err != nil {
		return dingtalkOutboundMessage{}, err
	}
	return dingtalkMarkdownMessage(content), nil
}

func (a *DingtalkAdapter) QueryReceipt(ctx context.Context, externalMessageID string) (adapter.DeliveryAck, error) {
	ack := adapter.DeliveryAck{ExternalMessageID: strings.TrimSpace(externalMessageID), Status: adapter.DeliveryOutcomeUnknown}
	if ack.ExternalMessageID == "" {
		return ack, fmt.Errorf("钉钉回执缺少 external_message_id")
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return ack, fmt.Errorf("获取 Access Token 失败: %w", err)
	}
	api, err := a.apiClient()
	if err != nil {
		return ack, fmt.Errorf("初始化钉钉官方 SDK 失败: %w", err)
	}
	query, ok := api.(dingtalkReceiptOpenAPI)
	if !ok {
		return ack, fmt.Errorf("钉钉回执查询能力不可用")
	}
	status, err := query.QueryOTO(ctx, token, a.cfg.RobotCode, ack.ExternalMessageID)
	if err != nil {
		return ack, fmt.Errorf("查询钉钉投递回执失败: %w", err)
	}
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "SUCCESS", "SUCESS", "DELIVERED":
		ack.Status = adapter.DeliveryDelivered
	case "SENDING", "PENDING", "ACCEPTED":
		ack.Status = adapter.DeliveryAccepted
	case "FAILED", "FAIL", "ERROR":
		ack.Status = adapter.DeliveryFailed
	default:
		ack.Status = adapter.DeliveryOutcomeUnknown
	}
	return ack, nil
}

func (a *DingtalkAdapter) sendReplyNow(ctx context.Context, chatID string, reply *adapter.Reply) error {
	_, err := a.sendReplyNowWithReceipt(ctx, chatID, reply, nil)
	return err
}

func (a *DingtalkAdapter) sendReplyNowWithReceipt(
	ctx context.Context,
	chatID string,
	reply *adapter.Reply,
	markProviderSendStarted func(),
) (string, error) {
	if reply == nil {
		return "", nil
	}
	if _, isGroup := parseGroupQueueTarget(chatID); isGroup {
		return "", errors.New("DingTalk v0.5 supports direct messages only")
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("获取 Access Token 失败: %w", err)
	}

	api, err := a.apiClient()
	if err != nil {
		return "", fmt.Errorf("初始化钉钉官方 SDK 失败: %w", err)
	}
	msg, err := a.replyMessageWithAttachments(ctx, api, token, reply)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if markProviderSendStarted != nil {
		markProviderSendStarted()
	}
	externalID, err := api.SendOTO(ctx, token, a.cfg.RobotCode, chatID, msg)
	if err != nil {
		return "", fmt.Errorf("发送消息失败: %w", err)
	}
	return externalID, nil
}

// sendReplyToEvent sends a terminal reply through the adapter's bounded,
// 3-per-second queue while retaining the original conversation type.
func (a *DingtalkAdapter) sendReplyToEvent(ctx context.Context, event dtEvent, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	if event.ConversationType == "2" {
		return nil
	}
	return a.Send(ctx, event.SenderStaffId, reply)
}

// sendReplyToEventNow 把入站消息回复到它原本所在的对话：普通单聊回 senderStaffId，
// 群聊回 openConversationId。它不能复用 Send(chatID)，因为后者缺 conversation type。
func (a *DingtalkAdapter) sendReplyToEventNow(ctx context.Context, event dtEvent, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	if event.ConversationType == "2" {
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
	msg, err := a.replyMessageWithAttachments(ctx, api, token, reply)
	if err != nil {
		return err
	}
	if _, err := api.SendOTO(ctx, token, a.cfg.RobotCode, event.SenderStaffId, msg); err != nil {
		return fmt.Errorf("发送单聊消息失败: %w", err)
	}
	return nil
}

func (a *DingtalkAdapter) replyMessageWithAttachments(
	ctx context.Context,
	api dingtalkOpenAPI,
	token string,
	reply *adapter.Reply,
) (dingtalkOutboundMessage, error) {
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		return dingtalkOutboundMessage{}, err
	}
	content, err := dingTalkManifestMarkdown(*reply.RenderManifest)
	if err != nil {
		return dingtalkOutboundMessage{}, err
	}
	for i, att := range reply.Attachments {
		if !adapter.IsImageAttachment(att) {
			return dingtalkOutboundMessage{}, errors.New("DingTalk only supports image attachments")
		}
		if strings.TrimSpace(att.Data) == "" {
			return dingtalkOutboundMessage{}, errors.New("DingTalk image attachment bytes are required")
		}
		mediaAPI, ok := api.(dingtalkMediaOpenAPI)
		if !ok {
			return dingtalkOutboundMessage{}, errors.New("DingTalk image upload capability is unavailable")
		}
		uploadAttachment := att
		uploadAttachment.Name = safeDingTalkAttachmentName(att.Name)
		imageRef, err := mediaAPI.UploadImage(ctx, token, uploadAttachment)
		if err != nil {
			logger.Error("[dingtalk] 上传回复图片失败", "name", uploadAttachment.Name, "error", err)
			return dingtalkOutboundMessage{}, fmt.Errorf("DingTalk image upload failed: %w", err)
		}
		if !validDingtalkImageReference(imageRef) {
			return dingtalkOutboundMessage{}, errors.New("DingTalk image upload returned an invalid media reference")
		}
		alt := "批改后的作业"
		if len(reply.Attachments) > 1 {
			alt = fmt.Sprintf("批改后的作业 %d", i+1)
		}
		content += "\n\n![" + alt + "](" + imageRef + ")"
	}
	return dingtalkMarkdownMessage(content), nil
}

func safeDingTalkAttachmentName(name string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	base := strings.TrimSpace(path.Base(normalized))
	if base == "" || base == "." || base == "/" {
		return "graded-homework.png"
	}
	return base
}

func validateDingTalkAttachmentName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil
	}
	if safeDingTalkAttachmentName(trimmed) != trimmed {
		return errors.New("DingTalk attachment name must not contain a path or URL")
	}
	if hasDingTalkAttachmentNameScheme(trimmed) {
		return errors.New("DingTalk attachment name must not contain a URI scheme or drive prefix")
	}
	if hasUnsafeDingTalkMarkdownLabelCharacters(trimmed) {
		return errors.New("DingTalk attachment name contains unsafe Markdown characters")
	}
	return nil
}

func hasUnsafeDingTalkMarkdownLabelCharacters(value string) bool {
	return strings.ContainsAny(value, "\r\n[]()!`<>")
}

func hasDingTalkAttachmentNameScheme(name string) bool {
	colon := strings.IndexByte(name, ':')
	if colon <= 0 {
		return false
	}
	for index := 0; index < colon; index++ {
		char := name[index]
		if index == 0 {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') {
				return false
			}
			continue
		}
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '+' && char != '-' && char != '.' {
			return false
		}
	}
	return true
}

func validDingtalkImageReference(ref string) bool {
	return validDingTalkFileMediaID(ref)
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
	reply := &adapter.Reply{Content: dingtalkThinkingFeedback}
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		logger.Error("[dingtalk] processing feedback canonicalization failed", "error", err)
		return ""
	}
	content, err := dingTalkManifestMarkdown(*reply.RenderManifest)
	if err != nil {
		logger.Error("[dingtalk] processing feedback projection failed", "error", err)
		return ""
	}
	key, err := api.SendOTO(ctx, token, a.cfg.RobotCode, chatID, dingtalkMarkdownMessage(content))
	if err != nil {
		logger.Error("[dingtalk] 发送思考占位失败", "error", err)
		return ""
	}
	return key
}

func (a *DingtalkAdapter) sendThinkingFeedbackForEvent(ctx context.Context, event dtEvent) string {
	if event.ConversationType == "2" {
		return ""
	}
	content := thinkingFeedbackForEvent(event)
	if strings.TrimSpace(event.SenderStaffId) == "" && strings.TrimSpace(event.ConversationId) == "" {
		return ""
	}
	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" || a.cfg.RobotCode == "" {
		return ""
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		logger.Error("[dingtalk] 处理进度取 Access Token 失败", "error", err)
		return ""
	}
	api, err := a.apiClient()
	if err != nil {
		logger.Error("[dingtalk] 处理进度初始化 SDK 失败", "error", err)
		return ""
	}
	reply := &adapter.Reply{Content: content}
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		logger.Error("[dingtalk] processing feedback canonicalization failed", "error", err)
		return ""
	}
	content, err = dingTalkManifestMarkdown(*reply.RenderManifest)
	if err != nil {
		logger.Error("[dingtalk] processing feedback projection failed", "error", err)
		return ""
	}
	msg := dingtalkMarkdownMessage(content)
	key, sendErr := api.SendOTO(ctx, token, a.cfg.RobotCode, event.SenderStaffId, msg)
	if sendErr != nil {
		logger.Error("[dingtalk] 发送单聊处理进度失败", "error", sendErr)
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

func (a *DingtalkAdapter) recallThinkingFeedbackForEvent(ctx context.Context, event dtEvent, processQueryKey string) {
	if strings.TrimSpace(processQueryKey) == "" {
		return
	}
	if event.ConversationType == "2" {
		return
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		logger.Error("[dingtalk] 撤回处理进度取 Access Token 失败", "error", err)
		return
	}
	api, err := a.apiClient()
	if err != nil {
		logger.Error("[dingtalk] 撤回处理进度初始化 SDK 失败", "error", err)
		return
	}
	if err := api.RecallOTO(ctx, token, a.cfg.RobotCode, []string{processQueryKey}); err != nil {
		logger.Error("[dingtalk] 撤回单聊处理进度失败", "error", err)
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
	text := content
	if strings.TrimSpace(text) == "" {
		text = dingtalkEmptyReplyFallback
	}
	return dingtalkOutboundMessage{
		MsgKey:   "sampleMarkdown",
		MsgParam: marshalMarkdownContent(dingtalkMessageTitle(text), text),
	}
}

// restoreEscapedMarkdownNewlines 兼容被上游 JSON/自动化脚本二次转义的整篇 Markdown。
// 只在“正文没有任何真实换行且至少出现两个字面量 \\n”时恢复，避免把用户讨论
// `a\nb`、Windows 路径或代码里的单个转义序列擅自改写。
func restoreEscapedMarkdownNewlines(content string) string {
	if strings.ContainsAny(content, "\r\n") || strings.Count(content, `\n`) < 2 {
		return content
	}
	content = strings.ReplaceAll(content, `\r\n`, "\n")
	return strings.ReplaceAll(content, `\n`, "\n")
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
	var terminal *adapter.ReplyChunk
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
		if chunk.Done {
			terminal = chunk
		}
	}
	reply := &adapter.Reply{Content: adapter.StripThinking(sb.String())}
	if terminal != nil {
		reply.Metadata = terminal.Metadata
		reply.MessageContent = terminal.MessageContent
		reply.RenderManifest = terminal.RenderManifest
		reply.ToolCalls = terminal.ToolCalls
	}
	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给终端用户
	return a.Send(ctx, chatID, reply)
}

// handleMessage 处理消息
func (a *DingtalkAdapter) handleMessage(event dtEvent) {
	a.handleMessageContext(context.Background(), event)
}

// admitInboundPhotoBeforeACK 只在注入耐久端口时接管 direct 图片：先下载一次并构造
// canonical Message，再同步完成耐久接纳。未匹配消息保留预下载附件交给旧 worker。
func (a *DingtalkAdapter) admitInboundPhotoBeforeACK(
	ctx context.Context, event *dtEvent,
) (bool, error) {
	port := a.currentInboundPhotoAdmissionPort()
	if port == nil || event == nil || event.MsgType != dtMsgTypePicture ||
		strings.TrimSpace(event.Content.DownloadCode) == "" {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	attachment, err := a.downloadPictureAttachment(ctx, event.Content.DownloadCode)
	if err != nil {
		return false, fmt.Errorf("DingTalk inbound photo download before durable admission: %w", err)
	}
	event.Attachments = []adapter.Attachment{attachment}
	handled, err := port.AdmitInboundPhoto(ctx, a.messageFromEvent(*event, event.Attachments))
	if err != nil {
		return false, fmt.Errorf("DingTalk inbound photo durable admission failed: %w", err)
	}
	return handled, nil
}

func (a *DingtalkAdapter) messageFromEvent(
	event dtEvent, attachments []adapter.Attachment,
) *adapter.Message {
	return &adapter.Message{
		ID:          event.MsgID,
		Platform:    adapter.PlatformDingtalk,
		InstanceID:  a.Name(),
		ChatID:      event.SenderStaffId,
		UserID:      event.SenderStaffId,
		UserName:    event.SenderNick,
		Content:     strings.TrimSpace(event.Text.Content),
		Attachments: append([]adapter.Attachment(nil), attachments...),
		Timestamp:   time.Now(),
		Metadata: map[string]string{
			"conversation_id":   event.ConversationId,
			"conversation_type": event.ConversationType,
		},
	}
}

func (a *DingtalkAdapter) handleMessageContext(baseCtx context.Context, event dtEvent) {
	if a.handler == nil {
		return
	}
	if event.ConversationType == "2" {
		logger.Info("DingTalk group message ignored because v0.5 supports direct messages only")
		return
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	content := strings.TrimSpace(event.Text.Content)

	ctx, cancel := context.WithTimeout(baseCtx, a.messageHandlerTimeoutFor(event))
	defer cancel()

	// 图片批改通常要经历下载、整页识题和逐题校验。先于图片下载发送 ETA，确保即使
	// 下载或后续模型调用较慢，用户也能立即知道任务已经进入处理队列。所有退出路径
	// 都在独立终态 ctx 中撤回这条临时进度，避免长期残留。
	thinkingKey := ""
	thinkingSent := false
	if event.MsgType == dtMsgTypePicture && event.Content.DownloadCode != "" {
		thinkingSent = true
		thinkingKey = a.sendThinkingFeedbackForEvent(ctx, event)
	}
	defer func() {
		rc, recallCancel := terminalNotifyCtx()
		defer recallCancel()
		a.recallThinkingFeedbackForEvent(rc, event, thinkingKey)
	}()

	// picture 消息（BUG-20260709）：downloadCode → 临时下载 URL → 图片字节 → image 附件，
	// 与桌面/web 同走 BuildMultimodalUserMessage 多模态管道（无 vision 模型时引擎有友好拒绝）。
	// 下载失败给用户明确提示，绝不静默丢弃。
	attachments := append([]adapter.Attachment(nil), event.Attachments...)
	if event.Content.DownloadCode != "" {
		// BUG-20260710：钉钉的语音/视频/文件回调同样以 content.downloadCode 承载。
		// 只有 msgtype=picture 才能走图片下载进多模态管道；其余类型硬贴 image 附件
		// 会让 provider 400、用户收到不可归因报错——此处给出明确"暂不支持"提示后返回。
		if event.MsgType != dtMsgTypePicture {
			logger.Warn("钉钉: 收到暂不支持的富媒体消息", "msgtype", event.MsgType)
			ntCtx, ntCancel := terminalNotifyCtx()
			defer ntCancel()
			_ = a.sendReplyToEvent(ntCtx, event, &adapter.Reply{Content: dingtalkUnsupportedMediaFeedback})
			return
		}
		if len(attachments) == 0 {
			att, err := a.downloadPictureAttachment(ctx, event.Content.DownloadCode)
			if err != nil {
				logger.Error("钉钉: 下载图片消息失败", "error", err)
				errCtx, errCancel := terminalNotifyCtx()
				defer errCancel()
				_ = a.sendReplyToEvent(errCtx, event, &adapter.Reply{Content: "⚠️ 图片获取失败，请重新发送一次。"})
				return
			}
			attachments = append(attachments, att)
		}
	}
	if !adapter.HasMessageInput(content, attachments) {
		return
	}

	chatID := event.SenderStaffId
	if event.ConversationType == "2" && strings.TrimSpace(event.ConversationId) != "" {
		chatID = event.ConversationId
	}
	msg := a.messageFromEvent(event, attachments)
	msg.ChatID = chatID

	// 普通文本在完成输入校验后发送原有思考占位；图片占位已在下载前发出。
	if !thinkingSent {
		thinkingSent = true
		thinkingKey = a.sendThinkingFeedbackForEvent(ctx, event)
	}

	reply, err := a.handler(ctx, msg)
	if err != nil {
		// 不因 ctx 已超时而提前 return：超时正是用户最需要反馈之时（BUG-20260713）。终态错误
		// 提示用独立兜底 ctx 发送，绝不静默放弃；占位由上面的 defer 撤回。
		logger.Error("钉钉: 处理消息失败", "error", err)
		nc, notifyCancel := terminalNotifyCtx()
		defer notifyCancel()
		_ = a.sendReplyToEvent(nc, event, &adapter.Reply{Content: "处理消息时出现错误，请稍后重试。"})
		return
	}
	if reply == nil {
		return // 占位由 defer 撤回
	}

	// 最终答案同为「必达」终态通知：用独立兜底 ctx，避免继承可能已耗尽的 handler ctx。
	sendCtx, finalCancel := terminalNotifyCtx()
	defer finalCancel()
	if err := a.sendReplyToEvent(sendCtx, event, reply); err != nil {
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
	MsgID            string `json:"msgId"`
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
	Attachments []adapter.Attachment `json:"-"`
}

// marshalMarkdownContent 生成 sampleMarkdown 的 {"title","text"} 载荷
// （与 sampleText 的 {"content"} 结构不同，两者不可混用）。
func marshalMarkdownContent(title, text string) string {
	b, _ := json.Marshal(map[string]string{"title": title, "text": text})
	return string(b)
}
