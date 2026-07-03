package feishu

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestFeishuOutboundUsesOfficialSDK(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(strings.Replace(file, "official_sdk_contract_test.go", "feishu.go", 1))
	if err != nil {
		t.Fatalf("read feishu.go: %v", err)
	}
	text := string(src)

	required := []string{
		`lark "github.com/larksuite/oapi-sdk-go/v3"`,
		"openAPIClient().Im.V1.Message.Create",
		"openAPIClient().Im.V1.Message.Reply",
		"openAPIClient().Im.V1.Message.Update",
		"openAPIClient().Im.V1.MessageReaction.Create",
		"openAPIClient().Im.V1.MessageReaction.Delete",
		"openAPIClient().GetTenantAccessTokenBySelfBuiltApp",
	}
	for _, marker := range required {
		if !strings.Contains(text, marker) {
			t.Fatalf("feishu outbound must use official SDK marker %q", marker)
		}
	}

	banned := []string{
		"http.NewRequestWithContext",
		"tenant_access_token/internal",
		"getAccessToken(",
		"baseURL",
	}
	for _, marker := range banned {
		if strings.Contains(text, marker) {
			t.Fatalf("feishu must not hand-roll OpenAPI/auth HTTP paths; found %q", marker)
		}
	}
}
