package dingtalk

// 钉钉图片消息真机门（BUG-20260709，env-gated 默认 skip）。
//
// 单测已用 fake openAPI + httptest 锁定 picture 链路；本文件补真机验证两件桩测不出的事：
//  1. DownloadMessageFile 的官方 SDK 封装（RobotMessageFileDownloadWithOptions 的
//     robotCode/downloadCode/headers 用法）真实端点是否接受——用无效 downloadCode 探测：
//     期望返回「downloadCode 无效/过期」类**业务错误**；若是「参数缺失/签名错」类用法错误
//     则说明封装写错了。
//  2. 下载失败路径真实发送「⚠️ 图片获取失败，请重新发送一次。」到用户钉钉——
//     修复前图片消息是**彻底沉默**，失败路径可见性是本修复的底线承诺。
//
// 运行：DINGTALK_LIVE_SEND=1 go test ./adapter/dingtalk/ -run TestLivePicture -v
// 凭证：与 live_send_real_test.go 同源（~/.hexclaw/data.db in-memory 解密，不落盘）。
// happy path（真图片→识别成功）无法在测试内构造——downloadCode 只能来自用户真实发图，
// 验证方式=装上新 sidecar 后往机器人真发一张作业照片。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
)

func TestLivePicture_InvalidDownloadCode_BusinessErrorAndUserNotice(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") != "1" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机（会真的往你的钉钉发一条「图片获取失败」提示）")
	}
	cfg, userID := loadLiveDingtalkConfig(t)
	adp := New(cfg)

	// ① 直接探测 SDK 封装：无效 downloadCode 应报业务错（不是签名/参数用法错）。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := adp.downloadPictureAttachment(ctx, "invalid-download-code-live-probe")
	if err == nil {
		t.Fatal("无效 downloadCode 竟然下载成功——探测无效")
	}
	t.Logf("① 真机下载探测返回错误（期望是 downloadCode 无效类业务错，非签名/凭证用法错）：%v", err)
	low := strings.ToLower(err.Error())
	// 期望的业务错：钉钉实测返回 code=invalidParameter.robotCode.downloadCode
	//（「下载码有误或者已经过期」）——证明 SDK 封装/凭证/robotCode 全对，只有探测用的假码无效。
	if !strings.Contains(low, "downloadcode") {
		t.Fatalf("错误里没提 downloadCode，形态像 SDK 用法/凭证错，说明 DownloadMessageFile 封装有问题: %v", err)
	}
	for _, usageErr := range []string{"sign", "notfound.suitekey", "invalidauthentication", "forbidden"} {
		if strings.Contains(low, usageErr) {
			t.Fatalf("错误形态像凭证/权限错（%q）: %v", usageErr, err)
		}
	}

	// ② 生产 handleMessage 全路径：picture 事件 + 无效 code → 用户真实收到失败提示（修复前=沉默）。
	var event dtEvent
	event.SenderStaffId = userID
	event.SenderNick = "live-probe"
	event.MsgType = "picture"
	event.Content.DownloadCode = "invalid-download-code-live-probe"
	adp.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		t.Error("下载失败时不应到达 handler（应先回失败提示）")
		return nil, nil
	}
	adp.handleMessage(event) // 同步：返回时失败提示已真实发出
	t.Logf("② 已向钉钉 userId=%s 真实发送「⚠️ 图片获取失败」提示——请在钉钉里肉眼确认收到（修复前此场景零回复）", userID)
}

// livePictureOpenAPI 包装真实 officialDingtalkOpenAPI：token/发送/撤回全真实，
// 仅 DownloadMessageFile 指向本地媒体服务器供给真实图片文件——downloadCode 兑换端点
// 已由上面的无效码探针真机验证，happy path 缺的只是「一张真图进管道」。
type livePictureOpenAPI struct {
	dingtalkOpenAPI
	mediaURL string
}

func (l *livePictureOpenAPI) DownloadMessageFile(_ context.Context, _, _, _ string) (string, error) {
	return l.mediaURL, nil
}

// TestLivePicture_RealImageSolve_SendToDingtalk 用一张**真实作业照片**走生产 picture 全链路：
// dtEvent(picture) → 下载图片字节 → image 附件 → 真实多模态 LLM 识题解答 → 真实发送到你的钉钉。
//
//	DINGTALK_LIVE_SEND=1 DINGTALK_LIVE_IMAGE=/path/to/作业.jpg \
//	  go test ./adapter/dingtalk/ -run TestLivePicture_RealImageSolve -v -timeout 5m
//
// 模型：DINGTALK_LIVE_PROVIDER 指定（需 vision，如 openrouter 的 omni 模型），仍是用户真实配置。
func TestLivePicture_RealImageSolve_SendToDingtalk(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") != "1" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机（真实模型解题并发到你的钉钉）")
	}
	imgPath := os.Getenv("DINGTALK_LIVE_IMAGE")
	if imgPath == "" {
		t.Skip("设 DINGTALK_LIVE_IMAGE=<作业照片路径> 跑真图解题")
	}
	imgBytes, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("读取图片失败: %v", err)
	}
	t.Logf("图片 %s（%d 字节）", imgPath, len(imgBytes))

	cfg, userID := loadLiveDingtalkConfig(t)

	// 真实 LLM 路由（用户 hexclaw.yaml），固定到指定 vision provider。
	appCfg, err := config.Load("")
	if err != nil {
		t.Fatalf("加载应用配置失败: %v", err)
	}
	provider := os.Getenv("DINGTALK_LIVE_PROVIDER")
	if provider == "" {
		provider = "openrouter" // 默认 omni 多模态模型
	}
	if _, ok := appCfg.LLM.Providers[provider]; !ok {
		t.Fatalf("配置里没有 provider %q", provider)
	}
	appCfg.LLM.Default = provider
	appCfg.LLM.Routing.Enabled = false
	sel, err := llmrouter.New(appCfg.LLM)
	if err != nil {
		t.Fatalf("初始化 LLM 路由失败: %v", err)
	}

	// 本地媒体服务器供给真图（替身仅此一环，兑换端点已由探针真机验证）。
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(imgBytes)
	}))
	defer mediaSrv.Close()

	adp := New(cfg)
	realAPI, err := adp.apiClient()
	if err != nil {
		t.Fatalf("初始化真实钉钉 SDK 失败: %v", err)
	}
	adp.openAPI = &livePictureOpenAPI{dingtalkOpenAPI: realAPI, mediaURL: mediaSrv.URL}

	// 真实多模态 handler：图片附件 → MultiContent(image_url data URI) → 真调 vision 模型解题。
	var answered atomic.Bool
	adp.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		if len(msg.Attachments) != 1 {
			t.Fatalf("handler 应收到 1 个图片附件，实际 %d", len(msg.Attachments))
		}
		att := msg.Attachments[0]
		t.Logf("   → handler 收到图片附件 mime=%s data=%d 字节(base64)", att.Mime, len(att.Data))
		p, model, rerr := sel.RouteModel(ctx)
		if rerr != nil {
			return nil, rerr
		}
		// v4：优先非推理型视觉模型——nemotron omni(reasoning) 遇到难题会把大段英文推理
		// 写进 content（v3 真机取证：思维题触发长篇 GCD/LCM 英文纠结原样送达用户），
		// 提示词不可控；推理模型出站剥离为 P2。免费池常 429 → 候选顺序尝试；
		// DINGTALK_LIVE_VISION_MODEL 可插队首选。
		candidates := []string{"google/gemma-4-26b-a4b-it:free", "google/gemma-4-31b-it:free", model}
		if vm := os.Getenv("DINGTALK_LIVE_VISION_MODEL"); vm != "" {
			candidates = append([]string{vm}, candidates...)
		}
		_ = model
		temp := 0.4
		var resp *llm.CompletionResponse
		var cerr error
		for _, cand := range candidates {
			t.Logf("   → 尝试真实模型 model=%s", cand)
			resp, cerr = p.Complete(ctx, llm.CompletionRequest{
				Model: cand,
				Messages: []llm.Message{
					// 用户反馈迭代：v1 混英文推理 → v2 收太死只答 3 题无文字说明 →
					// v3：整页逐题作答 + 题目原文 + 简要过程，仍禁思考过程/英文。
					// v5：结构化分隔标记——推理模型的内部思考压不住就让它随便想，
					// 但最终解答必须跟在标记之后，发送前只截取标记后内容（工程化剥离）。
					// v6（BUG-20260712 #9 内联回显）：分隔行之前先输出「📷 我读到的题目」回显抬头，
					// 家长一眼可核对 OCR 有无读错（手写拿不准的字符标 [?] 不猜）；不阻塞等确认。
					{Role: "system", Content: "你是小明的五年级辅导助手。家长发来孩子的整页作业照片，请识别并解答**整页所有题目**。输出格式硬性要求：①先输出「📷 我读到的题目（读错请直接回复纠正）：」一行，紧接着按题号逐条列出识别到的题目原文（手写拿不准的字符标 [?] 绝不猜）作为回显抬头；②然后输出一行只含「===最终解答===」的分隔行，紧接着给出完整解答——按大题分节（如「一、直接写得数」），每小题一行：题目原文 + 「= 答案」；计算/简算与解方程给一行简要过程；应用题、思维题写清算式与最终答案。全文**只能用中文**，不得出现英文或思考过程。"},
					{
						Role: "user",
						MultiContent: []llm.ContentPart{
							llm.NewTextPart("这是孩子的作业照片，请按要求识别并解答。"),
							llm.NewImageURLPart("data:"+att.Mime+";base64,"+att.Data, "auto"),
						},
					},
				},
				MaxTokens:   4000, // 整页 ~25 小题逐题作答，1200 不够截断
				Temperature: &temp,
			})
			if cerr == nil {
				break
			}
			t.Logf("   ✗ 模型 %s 调用失败（试下一候选）：%v", cand, cerr)
		}
		if cerr != nil {
			t.Log("   ✗ 全部候选模型失败")
			return nil, cerr
		}
		answer := strings.TrimSpace(adapter.StripThinking(resp.Content))
		if answer == "" {
			answer = strings.TrimSpace(resp.Content)
		}
		// v5 工程化剥离：只取「===最终解答===」标记之后的内容（推理模型的英文思考在标记前）。
		const marker = "===最终解答==="
		if idx := strings.LastIndex(answer, marker); idx >= 0 {
			answer = strings.TrimSpace(answer[idx+len(marker):])
		}
		if answer == "" {
			t.Log("   ✗ 模型返回空正文/标记后无内容")
			return nil, context.DeadlineExceeded
		}
		// 英文占比守卫：解答段仍以英文为主 → 判失败不发垃圾给用户
		letters, han := 0, 0
		for _, r := range answer {
			switch {
			case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
				letters++
			case r >= 0x4E00 && r <= 0x9FFF:
				han++
			}
		}
		if letters > han {
			t.Logf("   ✗ 解答段英文占比过高（en=%d han=%d），拒发。前 300 字：%s", letters, han, string([]rune(answer))[:min(300, len([]rune(answer)))])
			return nil, context.DeadlineExceeded
		}
		t.Logf("   → 真实解题答案(%d 字)：\n%s", len([]rune(answer)), answer)
		answered.Store(true)
		return &adapter.Reply{Content: "📷 已识别你发来的作业照片：\n\n" + answer}, nil
	}

	var event dtEvent
	event.SenderStaffId = userID
	event.SenderNick = "live-real-image"
	event.MsgType = "picture"
	event.Content.DownloadCode = "live-real-image-code"
	adp.handleMessage(event) // 生产路径：占位→下载真图→vision 解题→真实发送→撤占位
	if !answered.Load() {
		t.Fatal("解题未成功（模型失败/空正文），用户收到的是错误提示而非答案——见上方 ✗ 日志")
	}
	t.Logf("✅ 真图解题结果已真实发送到钉钉 userId=%s——请在钉钉里确认收到解题消息", userID)
}
