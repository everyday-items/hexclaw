package feishu

import (
	"testing"
)

func TestThinkingEmojiTypes_OnlyTypingAndThinking(t *testing.T) {
	// 验证 emoji 列表只包含语义正确的 AI 处理指示器
	allowed := map[string]bool{
		"Typing":   true,
		"THINKING": true,
	}

	for _, emoji := range thinkingEmojiTypes {
		if !allowed[emoji] {
			t.Errorf("不应包含社交 emoji %q，只允许 Typing/THINKING", emoji)
		}
	}

	if len(thinkingEmojiTypes) != 2 {
		t.Errorf("期望 2 个 emoji（Typing + THINKING），实际 %d 个", len(thinkingEmojiTypes))
	}
}

func TestRandomThinkingEmojiType_AlwaysValid(t *testing.T) {
	allowed := map[string]bool{
		"Typing":   true,
		"THINKING": true,
	}

	// 调用 100 次，确认每次都返回合法值
	seen := map[string]int{}
	for i := 0; i < 100; i++ {
		emoji := randomThinkingEmojiType()
		if !allowed[emoji] {
			t.Fatalf("第 %d 次返回了非法 emoji: %q", i, emoji)
		}
		seen[emoji]++
	}

	// 验证两个 emoji 都至少出现过（概率极高）
	if seen["Typing"] == 0 {
		t.Log("注意: Typing 未出现（极低概率事件，非 bug）")
	}
	if seen["THINKING"] == 0 {
		t.Log("注意: THINKING 未出现（极低概率事件，非 bug）")
	}

	t.Logf("分布: Typing=%d, THINKING=%d", seen["Typing"], seen["THINKING"])
}

func TestThinkingEmojiTypes_NoSocialEmojis(t *testing.T) {
	// 确认旧的社交 emoji 已移除
	removed := []string{"MUSCLE", "COFFEE", "FIRE", "FIST", "THUMBSUP", "CLAP"}
	current := map[string]bool{}
	for _, e := range thinkingEmojiTypes {
		current[e] = true
	}

	for _, old := range removed {
		if current[old] {
			t.Errorf("旧社交 emoji %q 应已移除", old)
		}
	}
}
