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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestLiveSend_RealMarkdown(t *testing.T) {
	if os.Getenv("DINGTALK_LIVE_SEND") != "1" {
		t.Skip("设 DINGTALK_LIVE_SEND=1 跑真机实发（会真的往你的钉钉发一条消息）")
	}

	// 1-2. 用共用 helper 精确选择真实实例与其用户，凭证仅在内存解密。
	cfg, userID := loadLiveDingtalkConfig(t)

	// 3. 走生产完全相同的发送路径：dingtalk.New → Send → sendReplyNow →
	//    officialDingtalkOpenAPI.SendOTO → dingtalkMarkdownMessage → BatchSendOTO(sampleMarkdown)。
	adp := New(cfg)
	markdown := "### 🧠 B7 真机实发验证\n" +
		"**加粗生效**、`行内代码`、[链接](https://hexclaw.net)：\n\n" +
		"1. 第一点\n2. 第二点\n\n" +
		"> 若你看到的是渲染后的富文本（而非裸 `###`），说明 sampleMarkdown 修复在真实钉钉端生效。"
	if markdownFile := strings.TrimSpace(os.Getenv("DINGTALK_LIVE_MARKDOWN_FILE")); markdownFile != "" {
		raw, err := os.ReadFile(markdownFile)
		if err != nil {
			t.Fatalf("读取 DINGTALK_LIVE_MARKDOWN_FILE: %v", err)
		}
		markdown = strings.TrimSpace(string(raw))
		if markdown == "" {
			t.Fatal("DINGTALK_LIVE_MARKDOWN_FILE 内容为空")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adp.Send(ctx, userID, &adapter.Reply{Content: markdown}); err != nil {
		t.Fatalf("钉钉真机实发失败: %v", err)
	}
	t.Logf("✅ 已向钉钉 userId=%s 真实发送 sampleMarkdown 消息——请在钉钉里肉眼确认 ### 标题/加粗渲染为富文本（非裸 markdown）", userID)
}
