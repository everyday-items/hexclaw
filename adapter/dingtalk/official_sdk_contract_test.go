package dingtalk

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestDingtalkOutboundUsesOfficialSDK(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(strings.Replace(file, "official_sdk_contract_test.go", "dingtalk.go", 1))
	if err != nil {
		t.Fatalf("read dingtalk.go: %v", err)
	}
	text := string(src)

	required := []string{
		`dtoauth "github.com/alibabacloud-go/dingtalk/oauth2_1_0"`,
		`dtrobot "github.com/alibabacloud-go/dingtalk/robot_1_0"`,
		"GetAccessTokenWithOptions",
		"BatchSendOTOWithOptions",
		// BUG-20260709：picture 消息 downloadCode 换下载 URL 也必须走官方 SDK
		"RobotMessageFileDownloadWithOptions",
		"SetXAcsDingtalkAccessToken",
	}
	for _, marker := range required {
		if !strings.Contains(text, marker) {
			t.Fatalf("dingtalk outbound must use official SDK marker %q", marker)
		}
	}

	// 禁令收敛到「手写 OpenAPI/auth 端点」的精确粒度（BUG-20260709 调整）：
	// 原 blanket 禁 http.NewRequestWithContext 会误伤「预签名媒体 URL 拉字节」——那不是
	// OpenAPI 调用（SDK 只返回临时 URL，无下载封装），必然是普通 HTTP GET。
	// OpenAPI/auth 的保护改为端点路径级禁令 + 上方 required SDK 标记双向锁定。
	banned := []string{
		"/v1.0/oauth2/accessToken",
		"/v1.0/robot/oToMessages/batchSend",
		"/v1.0/robot/messageFiles/download",
		"api.dingtalk.com/v1.0",
		"apiBase",
		"bytes.NewReader",
	}
	for _, marker := range banned {
		if strings.Contains(text, marker) {
			t.Fatalf("dingtalk must not hand-roll OpenAPI/auth HTTP paths; found %q", marker)
		}
	}
}
