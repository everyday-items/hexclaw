package adapter

import (
	"os"
	"strings"
	"testing"
)

func TestAdapterStopPassesCallerContextToSendQueue(t *testing.T) {
	paths := []string{
		"dingtalk/dingtalk.go",
		"discord/discord.go",
		"feishu/feishu.go",
		"line/line.go",
		"matrix/matrix.go",
		"slack/slack.go",
		"telegram/telegram.go",
		"wechat/wechat.go",
		"wecom/wecom.go",
		"whatsapp/whatsapp.go",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			source := string(body)
			if strings.Contains(source, "queue.Stop(context.Background())") {
				t.Fatal("adapter Stop replaces the caller deadline with context.Background")
			}
			if !strings.Contains(source, "queue.Stop(ctx)") {
				t.Fatal("adapter Stop does not pass its caller context to SendQueue.Stop")
			}
		})
	}
}
