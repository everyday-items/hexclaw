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
		"SetXAcsDingtalkAccessToken",
	}
	for _, marker := range required {
		if !strings.Contains(text, marker) {
			t.Fatalf("dingtalk outbound must use official SDK marker %q", marker)
		}
	}

	banned := []string{
		"http.NewRequestWithContext",
		"/v1.0/oauth2/accessToken",
		"/v1.0/robot/oToMessages/batchSend",
		"apiBase",
		"bytes.NewReader",
	}
	for _, marker := range banned {
		if strings.Contains(text, marker) {
			t.Fatalf("dingtalk must not hand-roll OpenAPI/auth HTTP paths; found %q", marker)
		}
	}
}
