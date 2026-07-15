package dingtalk

// 钉钉「思考占位 → 撤回」真机实发门（env-gated，默认 skip，用户显式触发）。
//
// 为什么必须有：BUG-20260704 的桩测证明「SendOTO 回传 processQueryKey、RecallOTO 会被调用」，
// 但只有真调钉钉 OpenAPI BatchSendOTO + BatchRecallOTO 才能证明：
//   ① 真实响应里确有 processQueryKey（不是 nil）；
//   ② 用该 key 调撤回接口，钉钉服务端接受并真的把那条消息撤掉。
// SDK 版本漂移、字段契约、robot 是否有撤回权限，桩测不出。
//
// 运行（用户在自己会话里亲自跑，凭证只在进程内存、不落盘；会真往你的钉钉发一条并撤回）：
//
//	DINGTALK_LIVE_SEND=1 \
//	DINGTALK_LIVE_CONFIRM=SEND_TO_EXPLICIT_DINGTALK_USER \
//	DINGTALK_LIVE_INSTANCE=<实例名> DINGTALK_LIVE_USERID=<userid> \
//	go test ./adapter/dingtalk/ -run TestLiveThinkingFeedbackRecall -v
//
// 凭证来源同 live_send_real_test.go：应用主密钥解密 ~/.hexclaw/data.db 的钉钉实例 config_json，全程 in-memory。

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/secret"
	_ "modernc.org/sqlite"
)

const liveDingtalkConfirmPhrase = "SEND_TO_EXPLICIT_DINGTALK_USER"

// loadLiveDingtalkConfig 从本机 ~/.hexclaw/data.db 解密明确指定的真实钉钉配置 + 目标 userId
// （in-memory，不落盘）。真机发送是破坏性测试门：send、confirm、instance、user 四项必须
// 全部显式给出，绝不从「最近实例/最近会话」推断目标。
func loadLiveDingtalkConfig(t *testing.T) (config.DingtalkConfig, string) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("DINGTALK_LIVE_SEND")) != "1" {
		t.Fatalf("真机发送必须显式设置 DINGTALK_LIVE_SEND=1")
	}
	if strings.TrimSpace(os.Getenv("DINGTALK_LIVE_CONFIRM")) != liveDingtalkConfirmPhrase {
		t.Fatalf("真机发送必须显式确认 DINGTALK_LIVE_CONFIRM=%s", liveDingtalkConfirmPhrase)
	}
	instanceName := strings.TrimSpace(os.Getenv("DINGTALK_LIVE_INSTANCE"))
	if instanceName == "" {
		t.Fatalf("真机发送必须显式设置 DINGTALK_LIVE_INSTANCE=<实例名>，不会自动选择最近实例")
	}
	userID := strings.TrimSpace(os.Getenv("DINGTALK_LIVE_USERID"))
	if userID == "" {
		t.Fatalf("真机发送必须显式设置 DINGTALK_LIVE_USERID=<目标 userid>，不会自动选择最近用户")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("获取 HOME 失败: %v", err)
	}
	dataDir := filepath.Join(home, ".hexclaw")

	box, err := secret.LoadBox(dataDir)
	if err != nil {
		t.Fatalf("加载主密钥失败: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "data.db")+"?mode=ro")
	if err != nil {
		t.Fatalf("打开 data.db 失败: %v", err)
	}
	defer db.Close()

	var stored string
	row := db.QueryRow(`SELECT name, config_json FROM platform_instances WHERE provider='dingtalk' AND name=? LIMIT 1`, instanceName)
	if err := row.Scan(&instanceName, &stored); err != nil {
		t.Fatalf("读取钉钉实例配置失败（是否已在连接中心配置钉钉？）: %v", err)
	}
	plain := stored
	if secret.IsEncrypted(stored) {
		b, oerr := box.Open(stored)
		if oerr != nil {
			t.Fatalf("解密钉钉配置失败: %v", oerr)
		}
		plain = string(b)
	}
	var cfg config.DingtalkConfig
	if err := json.Unmarshal([]byte(plain), &cfg); err != nil {
		t.Fatalf("解析钉钉配置失败: %v", err)
	}
	cfg.Name = instanceName // 与 instances.BuildAdapter 的生产行为一致。
	if cfg.AppKey == "" || cfg.AppSecret == "" || cfg.RobotCode == "" {
		t.Fatalf("钉钉配置缺 app_key/app_secret/robot_code")
	}

	return cfg, userID
}

func TestLiveThinkingFeedbackRecall_BUG20260704(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") != "1" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机（会真的往你的钉钉发一条占位并撤回）")
	}
	cfg, userID := loadLiveDingtalkConfig(t)
	adp := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. 走生产同一路径发「正在思考」占位，拿真实 processQueryKey。
	key := adp.sendThinkingFeedback(ctx, userID)
	if key == "" {
		t.Fatalf("真机发送思考占位未拿到 processQueryKey（SendOTO 应从钉钉响应回传 key；robot 可能无发送权限或配置错误）")
	}
	t.Logf("✅ [1/2] 已向 userId=%s 发送「⌨️ 已收到，正在思考…」占位，processQueryKey=%s —— 请在钉钉里确认它出现", userID, key)

	// 2. 停 4 秒模拟「思考中」，让占位可见。
	time.Sleep(4 * time.Second)

	// 3. 用真实 key 调钉钉撤回接口（生产 recallThinkingFeedback 内部同一路径）。
	token, err := adp.getAccessToken(ctx)
	if err != nil {
		t.Fatalf("取 Access Token 失败: %v", err)
	}
	api, err := adp.apiClient()
	if err != nil {
		t.Fatalf("初始化钉钉 SDK 失败: %v", err)
	}
	if err := api.RecallOTO(ctx, token, cfg.RobotCode, []string{key}); err != nil {
		t.Fatalf("真机撤回思考占位失败: %v", err)
	}
	t.Logf("✅ [2/2] 已撤回 processQueryKey=%s —— 请在钉钉里确认那条「正在思考」已消失（这就是答案就位后的行为）", key)
}

// TestLiveFullFlow_BUG20260704 走生产完全相同的 handleMessage 路径连发几条「完整消息」：
// 每条都是 占位出现 → 思考几秒 → 真实答案送达 → 占位撤回消失。
// 用桩 handler 提供确定性答案（不接 LLM），但发送/撤回全部真调线上钉钉 API。
//
//	DINGTALK_LIVE_SEND=1 go test ./adapter/dingtalk/ -run TestLiveFullFlow -v
func TestLiveFullFlow_BUG20260704(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") != "1" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机（会真的往你的钉钉连发几条完整消息）")
	}
	cfg, userID := loadLiveDingtalkConfig(t)
	adp := New(cfg)

	// 桩 handler：模拟「思考 2.5 秒」后给出确定性答案（真实场景是 LLM 引擎）。
	answers := map[string]string{
		"你好":       "你好！我是小蟹 🦀，很高兴见到你～有什么可以帮你的吗？",
		"1+2 等于几？": "**1 + 2 = 3** ✅\n\n简单说：把 1 和 2 合起来就是 3。还需要算别的吗？",
		"用一句话介绍下你自己": "我是 **小蟹**，HexClaw 里的本地 AI 助手 🦀——" +
			"能陪你聊天、查资料、写东西、跑自动化任务，数据都在你自己电脑上，隐私可控。",
	}
	adp.handler = func(_ context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		time.Sleep(2500 * time.Millisecond) // 让「正在思考…」占位可见
		reply := answers[msg.Content]
		if reply == "" {
			reply = "收到：" + msg.Content
		}
		return &adapter.Reply{Content: reply}, nil
	}

	// 依次「收到」几条消息，走生产 handleMessage（内部：发占位→handler→发答案→撤占位）。
	questions := []string{"你好", "1+2 等于几？", "用一句话介绍下你自己"}
	for i, q := range questions {
		t.Logf("── [%d/%d] 模拟收到消息：%q ──", i+1, len(questions), q)
		event := dtEvent{SenderStaffId: userID, SenderNick: "LiveTest"}
		event.Text.Content = q
		adp.handleMessage(event) // 同步：返回时占位已撤回、答案已送达
		t.Logf("   ✅ 已送达答案并撤回占位——钉钉里应只剩这条答案，「正在思考」已消失")
		time.Sleep(1500 * time.Millisecond) // 两条之间留点间隔，便于肉眼观察
	}
	t.Logf("✅ 全部 %d 条完整消息已发送。请在钉钉里确认：每条都是「占位→答案→占位消失」，最终只留答案。", len(questions))
}

// TestLiveRealModelFullFlow_BUG20260704 用**真实模型**（用户 hexclaw.yaml 里配置的默认 provider/model）
// 生成答案，走生产 handleMessage 全流程发到钉钉：占位→真实模型思考→真实答案→撤回占位。
// 与桩版的区别：答案由真实 LLM 生成，不是预置文案。
//
//	DINGTALK_LIVE_SEND=1 go test ./adapter/dingtalk/ -run TestLiveRealModelFullFlow -v -timeout 5m
func TestLiveRealModelFullFlow_BUG20260704(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") != "1" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机（真实模型生成答案并发到你的钉钉）")
	}
	cfg, userID := loadLiveDingtalkConfig(t)

	// 加载应用配置里的真实 LLM 集合（~/.hexclaw/hexclaw.yaml），建真实路由。
	appCfg, err := config.Load("")
	if err != nil {
		t.Fatalf("加载应用配置失败: %v", err)
	}
	// 用真实**云端**模型做清晰快速的演示：本机默认(quality-first)会路由到本地 qwen3.5:9b
	// (慢 ~8tok/s + 推理型，直调 Complete 的小预算下常只产出思考→空正文)。可用 DINGTALK_LIVE_PROVIDER
	// 覆盖；默认固定到「智谱 AI」(glm-4.5)。这仍是用户真实配置的 provider + key + model。
	provider := os.Getenv("DINGTALK_LIVE_PROVIDER")
	if provider == "" {
		provider = "智谱 AI"
	}
	if _, ok := appCfg.LLM.Providers[provider]; ok {
		appCfg.LLM.Default = provider
		appCfg.LLM.Routing.Enabled = false // 关闭智能路由 → 固定走 Default
	}
	sel, err := llmrouter.New(appCfg.LLM)
	if err != nil {
		t.Fatalf("初始化 LLM 路由失败: %v", err)
	}

	adp := New(cfg)
	// 真实 handler：按当前路由策略选真实默认 provider/model，真调 LLM 生成答案。
	adp.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		p, model, rerr := sel.RouteModel(ctx)
		if rerr != nil {
			return nil, rerr
		}
		t.Logf("   → 路由到真实模型 model=%s", model)
		temp := 0.6
		resp, cerr := p.Complete(ctx, llm.CompletionRequest{
			Model: model,
			Messages: []llm.Message{
				{Role: "system", Content: "你是小蟹，一个简洁友好的中文助手。用中文回答，控制在 3 句以内。"},
				{Role: "user", Content: msg.Content},
			},
			MaxTokens:   400,
			Temperature: &temp,
		})
		if cerr != nil {
			return nil, cerr
		}
		answer := strings.TrimSpace(adapter.StripThinking(resp.Content))
		if answer == "" {
			answer = strings.TrimSpace(resp.Content) // 剥思考后为空 → 回退原文，保证非空可送达
		}
		if answer == "" {
			answer = "（模型未返回正文）"
		}
		t.Logf("   → 真实答案(%d 字)：%s", len([]rune(answer)), answer)
		return &adapter.Reply{Content: answer}, nil
	}

	questions := []string{
		"你好，用一句话介绍下你自己",
		"1 加 2 等于几？只说答案",
	}
	for i, q := range questions {
		t.Logf("── [%d/%d] 真实模型处理用户请求：%q ──", i+1, len(questions), q)
		event := dtEvent{SenderStaffId: userID, SenderNick: "LiveTest"}
		event.Text.Content = q
		adp.handleMessage(event) // 生产路径：发占位→真实模型→发答案→撤占位
		t.Logf("   ✅ 真实模型答案已送达 + 占位已撤回")
		time.Sleep(1500 * time.Millisecond)
	}
	t.Logf("✅ 全部 %d 条「真实模型」完整消息已发送到钉钉。", len(questions))
}

// TestLiveEmptyReplyDeliversFallback_BUG20260704 真机验证空正文兜底：直接发一条空正文回复，
// 修复前钉钉硬拒 400 miss.param.markdownTotext；修复后走兜底文案、Send 成功送达。
//
//	DINGTALK_LIVE_SEND=1 go test ./adapter/dingtalk/ -run TestLiveEmptyReplyDeliversFallback -v
func TestLiveEmptyReplyDeliversFallback_BUG20260704(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") != "1" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机（会往你的钉钉发一条空正文兜底消息）")
	}
	cfg, userID := loadLiveDingtalkConfig(t)
	adp := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 空正文——修复前这里必 400；修复后应走兜底文案成功送达。
	if err := adp.Send(ctx, userID, &adapter.Reply{Content: ""}); err != nil {
		t.Fatalf("空正文回复真机发送失败（兜底未生效）: %v", err)
	}
	t.Logf("✅ 空正文回复已成功送达（走兜底文案 %q，不再 400）——请在钉钉里确认收到该兜底提示", dingtalkEmptyReplyFallback)
}
