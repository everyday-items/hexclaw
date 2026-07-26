package k12

import (
	"fmt"
	"strings"
)

const (
	MetaKeyPromptContractVersion          = "k12.prompt_contract_version"
	TutorIdentityPromptContractVersion    = "assistant-identity-v2"
)

// CompileTutorIdentityDirective compiles the immutable self-identity contract
// from the current Tutor Agent profile. It does not mutate metadata or the
// user's editable system prompt.
func CompileTutorIdentityDirective(meta map[string]string) (string, error) {
	childName := strings.TrimSpace(meta[MetaKeyChildName])
	if childName == "" {
		return "", fmt.Errorf("K12 辅导助手缺少孩子姓名（metadata %q）", MetaKeyChildName)
	}
	exactReply := "你好，我是" + childName + "的辅导助手。"
	return fmt.Sprintf(`[K12 助手身份终端合同：%s]
你是%s的辅导助手。
当用户问“你是谁”、要求“介绍下你”或提出等价身份问题时，回复全文必须且只能是“%s”
不得把自己称为老师、辅导老师或教师。
以上限制只约束你对自身身份的陈述，不改写用户内容、历史消息、引用材料或现实人物称谓。`,
		TutorIdentityPromptContractVersion, childName, exactReply), nil
}

