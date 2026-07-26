package k12

import (
	"strings"
	"testing"
)

func TestCompileTutorIdentityDirective_ExactAssistantIdentity(t *testing.T) {
	directive, err := CompileTutorIdentityDirective(map[string]string{
		MetaKeyChildName: " 小明 ",
	})
	if err != nil {
		t.Fatalf("CompileTutorIdentityDirective: %v", err)
	}
	const exactReply = "你好，我是小明的辅导助手。"
	if got := strings.Count(directive, exactReply); got != 1 {
		t.Fatalf("精确回复必须在 directive 中恰好出现一次，实际 %d：%q", got, directive)
	}
	for _, required := range []string{
		"回复全文必须且只能是“" + exactReply + "”",
		"不得把自己称为老师、辅导老师或教师",
		"不改写用户内容、历史消息、引用材料或现实人物称谓",
	} {
		if !strings.Contains(directive, required) {
			t.Fatalf("directive 缺少合同 %q：%q", required, directive)
		}
	}
}

func TestCompileTutorIdentityDirective_MissingChildFailsExplicitly(t *testing.T) {
	for _, meta := range []map[string]string{nil, {}, {MetaKeyChildName: " \t "}} {
		if _, err := CompileTutorIdentityDirective(meta); err == nil ||
			!strings.Contains(err.Error(), "缺少孩子姓名") {
			t.Fatalf("缺少 child_name 必须明确失败，meta=%#v err=%v", meta, err)
		}
	}
}

