package dingtalk

// 钉钉真机实发门（env-gated，默认 skip，用户显式触发）。
//
// 为什么必须有：B7 的契约锁/行为锁证明「代码构造 sampleMarkdown 载荷正确」，但
// 只有真调钉钉 OpenAPI BatchSendOTO 才能证明「钉钉真实端点接受这个新载荷并渲染」
// ——SDK 版本漂移、载荷字段名（title/text）与钉钉服务端契约的对齐，桩测不出。
//
// 运行（用户在自己会话里亲自跑，凭证只在进程内存、不落盘）：
//
//	DINGTALK_LIVE_SEND=1 go test ./adapter/dingtalk/ -run TestLiveSend_RealMarkdown -v
//
// 凭证来源：应用自己的 secret.Box 解密 ~/.hexclaw/data.db 里的钉钉实例 config_json
//（与运行中的 sidecar 同一把主密钥），全程 in-memory，绝不写任何文件。
// 目标 userId：默认取 data.db 里最近的钉钉会话 chat_id，可用 DINGTALK_LIVE_USERID 覆盖。

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/secret"
	_ "modernc.org/sqlite"
)

func TestLiveSend_RealMarkdown(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") == "" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机实发（会真的往你的钉钉发一条消息）")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("获取 HOME 失败: %v", err)
	}
	dataDir := filepath.Join(home, ".hexclaw")

	// 1. 用应用自己的主密钥解密钉钉实例配置（in-memory，不落盘）。
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
	if err := db.QueryRow(`SELECT config_json FROM platform_instances WHERE provider='dingtalk' LIMIT 1`).Scan(&stored); err != nil {
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
	if cfg.AppKey == "" || cfg.AppSecret == "" || cfg.RobotCode == "" {
		t.Fatalf("钉钉配置缺 app_key/app_secret/robot_code（app_key<len %d> secret<len %d> robot<len %d>）",
			len(cfg.AppKey), len(cfg.AppSecret), len(cfg.RobotCode))
	}

	// 2. 目标 userId：env 覆盖 > 最近钉钉会话 chat_id。
	userID := os.Getenv("DINGTALK_LIVE_USERID")
	if userID == "" {
		_ = db.QueryRow(`SELECT chat_id FROM sessions WHERE platform='dingtalk' AND chat_id != '' ORDER BY updated_at DESC LIMIT 1`).Scan(&userID)
	}
	if userID == "" {
		t.Fatalf("找不到目标 userId（无钉钉历史会话），请设 DINGTALK_LIVE_USERID=<你的钉钉 userid>")
	}

	// 3. 走生产完全相同的发送路径：dingtalk.New → Send → sendReplyNow →
	//    officialDingtalkOpenAPI.SendOTO → dingtalkMarkdownMessage → BatchSendOTO(sampleMarkdown)。
	adp := New(cfg)
	markdown := "### 🧠 B7 真机实发验证\n" +
		"**加粗生效**、`行内代码`、[链接](https://hexclaw.net)：\n\n" +
		"1. 第一点\n2. 第二点\n\n" +
		"> 若你看到的是渲染后的富文本（而非裸 `###`），说明 sampleMarkdown 修复在真实钉钉端生效。"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adp.Send(ctx, userID, &adapter.Reply{Content: markdown}); err != nil {
		t.Fatalf("钉钉真机实发失败: %v", err)
	}
	t.Logf("✅ 已向钉钉 userId=%s 真实发送 sampleMarkdown 消息——请在钉钉里肉眼确认 ### 标题/加粗渲染为富文本（非裸 markdown）", userID)
}
