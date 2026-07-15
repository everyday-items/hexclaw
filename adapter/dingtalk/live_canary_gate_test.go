package dingtalk

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const liveGateChildEnv = "HEXCLAW_DINGTALK_GATE_CHILD"

func TestLiveDingtalkCanary_RequiresExplicitSendTargetAndConfirmation(t *testing.T) {
	if os.Getenv(liveGateChildEnv) == "1" {
		loadLiveDingtalkConfig(t)
		return
	}

	tests := []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "send gate",
			want: "DINGTALK_LIVE_SEND=1",
		},
		{
			name: "confirmation gate",
			env:  []string{"DINGTALK_LIVE_SEND=1"},
			want: "DINGTALK_LIVE_CONFIRM=SEND_TO_EXPLICIT_DINGTALK_USER",
		},
		{
			name: "instance gate",
			env: []string{
				"DINGTALK_LIVE_SEND=1",
				"DINGTALK_LIVE_CONFIRM=SEND_TO_EXPLICIT_DINGTALK_USER",
			},
			want: "DINGTALK_LIVE_INSTANCE",
		},
		{
			name: "user gate",
			env: []string{
				"DINGTALK_LIVE_SEND=1",
				"DINGTALK_LIVE_CONFIRM=SEND_TO_EXPLICIT_DINGTALK_USER",
				"DINGTALK_LIVE_INSTANCE=explicit-test-instance",
			},
			want: "DINGTALK_LIVE_USERID",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestLiveDingtalkCanary_RequiresExplicitSendTargetAndConfirmation$")
			cmd.Env = append(withoutLiveDingtalkEnv(os.Environ()), liveGateChildEnv+"=1")
			cmd.Env = append(cmd.Env, "HOME="+t.TempDir())
			cmd.Env = append(cmd.Env, tc.env...)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("child unexpectedly passed; output=%s", output)
			}
			if !strings.Contains(string(output), tc.want) {
				t.Fatalf("child output = %q, want prerequisite %q", output, tc.want)
			}
		})
	}
}

func withoutLiveDingtalkEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, item := range env {
		if strings.HasPrefix(item, "DINGTALK_LIVE_") || strings.HasPrefix(item, liveGateChildEnv+"=") || strings.HasPrefix(item, "HOME=") {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}
