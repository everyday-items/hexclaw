package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// 回归测试 — R4-1（2026-06-22 hex-test 审计）：
// ReplyChunk 经 SSE (api/server.go json.Marshal(chunk)) 写到桌面端，
// 桌面 Rust (src-tauri/src/commands.rs) 按小写键 content/reasoning/done/error 读取。
// 若 ReplyChunk 字段缺 json tag，json.Marshal 输出 PascalCase（Content/Reasoning/...），
// Rust 永不命中 → SSE 聊天正文恒空。
//
// 本测试直接断言 *wire JSON 字符串* 含小写键 —— 不能用同结构体 round-trip 验证，
// 因为 Go↔Go 大小写自洽，抓不到跨语言契约错位（既有 sse_characterization_test 的盲区）。
func TestReplyChunkSSEKeys_R4_1_Lowercase(t *testing.T) {
	b, err := json.Marshal(ReplyChunk{Content: "hello", Reasoning: "think", Done: true})
	if err != nil {
		t.Fatalf("marshal ReplyChunk: %v", err)
	}
	out := string(b)

	for _, key := range []string{`"content"`, `"reasoning"`, `"done"`} {
		if !strings.Contains(out, key) {
			t.Errorf("SSE wire JSON 缺小写键 %s（桌面 commands.rs 读不到）；实际=%s", key, out)
		}
	}
	// 反向：不应出现 PascalCase 键（桌面端不读）
	for _, bad := range []string{`"Content"`, `"Reasoning"`, `"Done"`} {
		if strings.Contains(out, bad) {
			t.Errorf("SSE wire JSON 出现 PascalCase 键 %s（桌面端不读 → 正文丢失）；实际=%s", bad, out)
		}
	}
}
