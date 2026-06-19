package email

// audit_wave3_w3_6_7_test.go —— W3-6 / W3-7 审计测试
//
// W3-6 [逻辑]: fetchAndProcess 在 Stop 后仍处理在途轮询的回复。
//   正确行为: 处理每封邮件前检查 stopped 标志, 已停止则不再调用 handler。
// W3-7 [功能未闭环]: 出站 subject 未做 RFC2047/MIME 编码, 非 ASCII 乱码。
//   正确行为: subject 经 mime QEncoding/BEncoding 编码为 encoded-word, 纯 ASCII 不变。

import (
	"context"
	"errors"
	"mime"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// ---------------------------------------------------------------------------
// W3-6: Stop 后在途轮询不应继续处理
// ---------------------------------------------------------------------------

// TestFetchAndProcess_StoppedSkipsHandler 验证: 当适配器已 Stop 时,
// 在途的 fetchAndProcess 不应继续调用 handler 处理已拉取的邮件。
//
// 回归: W3-6
func TestFetchAndProcess_StoppedSkipsHandler(t *testing.T) {
	raw := "From: u@example.com\r\nSubject: S\r\n\r\nhi"
	fc := &fakeSession{ids: []string{"1", "2"}, raw: raw}
	a := New(EmailConfig{MaxFetch: 10})
	a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) { return fc, nil }

	called := 0
	a.handler = func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) {
		called++
		return nil, nil
	}

	// 先置位 stopped, 模拟 Stop 后仍有在途轮询执行。
	_ = a.Stop(context.Background())
	a.fetchAndProcess(context.Background())

	if called != 0 {
		t.Errorf("Stop 后在途轮询不应处理邮件, 但 handler 被调用 %d 次", called)
	}
}

// ---------------------------------------------------------------------------
// W3-7: 出站 subject RFC2047/MIME 编码
// ---------------------------------------------------------------------------

// TestEncodeSubject_NonASCII 非 ASCII subject 应被编码为 RFC2047 encoded-word,
// 不再以原始 UTF-8 字节出现在头部(否则收件端乱码)。
//
// 回归: W3-7
func TestEncodeSubject_NonASCII(t *testing.T) {
	subject := "你好世界"
	got := encodeSubject(subject)

	if got == subject {
		t.Fatalf("非 ASCII subject 未被编码, 仍为原始 UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "=?") || !strings.HasSuffix(got, "?=") {
		t.Errorf("编码结果应为 RFC2047 encoded-word(=?...?=), 得到 %q", got)
	}
	// encoded-word 必须是纯 ASCII。
	for _, r := range got {
		if r > 127 {
			t.Errorf("编码结果含非 ASCII 字符 %q, 完整结果 %q", r, got)
			break
		}
	}
	// 可被标准库 mime 解码还原。
	dec := new(mime.WordDecoder)
	back, err := dec.DecodeHeader(got)
	if err != nil {
		t.Fatalf("解码编码后的 subject 失败: %v", err)
	}
	if back != subject {
		t.Errorf("编码后解码 = %q, want %q", back, subject)
	}
}

// TestEncodeSubject_ASCIIUnchanged 纯 ASCII subject 应保持不变(避免无谓编码)。
//
// 回归: W3-7
func TestEncodeSubject_ASCIIUnchanged(t *testing.T) {
	subject := "Re: HexClaw normal subject 123"
	if got := encodeSubject(subject); got != subject {
		t.Errorf("纯 ASCII subject 应不变, 得到 %q, want %q", got, subject)
	}
}

// TestEncodeSubject_NoHeaderInjection 编码后的 subject 不含裸 CR/LF
// (encoded-word 输出本身为 ASCII 安全, 与 sanitizeHeader 协同防注入)。
//
// 回归: W3-7
func TestEncodeSubject_NoHeaderInjection(t *testing.T) {
	got := encodeSubject("含\r\n注入的\r\n中文主题")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("编码后的 subject 仍含 CR/LF: %q", got)
	}
}

// 确保 errors 导入在该文件被使用(W3-6 用例若后续调整仍保留)。
var _ = errors.New
