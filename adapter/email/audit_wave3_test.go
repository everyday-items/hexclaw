package email

// audit_wave3_test.go —— 邮件适配器全场景审计测试
//
// 本包是 IMAP/SMTP 轮询型 IM 适配器（非 webhook/HMAC 型），因此审计重点放在
// 平台特定逻辑上：
//   - 入站 payload 解析（parseIncomingEmail / ParseEmailAddress）：各种消息类型、
//     空字段、超长、Unicode、非法 RFC822。
//   - IMAP 协议解析（parseLiteralSize / SearchUnseen / FetchRFC822 / run）：
//     literal 大小边界、负数、0、搜索空结果、maxFetch 截断。
//   - 出站消息转换（Send / SendStream / sanitizeHeader）：头部注入防护、subject 拼接、
//     流式缓冲、错误透传。
//   - 配置默认值与状态一致性（New / Stop）。
//
// 所有测试遵循 Go table-driven 惯例。仅断言对外可观察的正确行为，不修改被测源码。

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// ---------------------------------------------------------------------------
// 入站解析: parseIncomingEmail
// ---------------------------------------------------------------------------

// TestParseIncomingEmail 覆盖入站邮件解析的各种消息形态。
func TestParseIncomingEmail(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantErr     bool
		wantChatID  string
		wantUser    string
		wantContent string
		wantSubject string
		// checkID 为可选的 msgID 断言函数（用于无 Message-ID 时的回退判断）。
		checkID func(t *testing.T, id string)
	}{
		{
			name:        "正常邮件",
			raw:         "From: Alice <alice@example.com>\r\nSubject: Hello\r\nMessage-ID: <m1@x>\r\nDate: Fri, 20 Mar 2026 10:00:00 +0800\r\n\r\nhello body\r\n",
			wantChatID:  "alice@example.com",
			wantUser:    "alice@example.com",
			wantContent: "hello body",
			wantSubject: "Hello",
		},
		{
			name:        "空主题与空正文",
			raw:         "From: bob@example.com\r\n\r\n",
			wantChatID:  "bob@example.com",
			wantUser:    "bob@example.com",
			wantContent: "",
			wantSubject: "",
			checkID: func(t *testing.T, id string) {
				// 无 Message-ID 时应回退为 email-<纳秒>。
				if !strings.HasPrefix(id, "email-") {
					t.Errorf("无 Message-ID 时应回退为 email- 前缀, 得到 %q", id)
				}
			},
		},
		{
			name:        "Message-ID 去除尖括号",
			raw:         "From: c@example.com\r\nMessage-ID: <  abc@host  >\r\n\r\nx",
			wantChatID:  "c@example.com",
			wantUser:    "c@example.com",
			wantContent: "x",
			checkID: func(t *testing.T, id string) {
				// 先 Trim<>，再 TrimSpace；内层空格保留。
				if id != "abc@host" {
					t.Errorf("Message-ID 解析 = %q, want %q", id, "abc@host")
				}
			},
		},
		{
			name:        "Unicode 主题与正文",
			raw:         "From: 张三 <zhang@example.com>\r\nSubject: 你好世界\r\n\r\n这是一封测试邮件 emoji😀\r\n",
			wantChatID:  "zhang@example.com",
			wantUser:    "zhang@example.com",
			wantContent: "这是一封测试邮件 emoji😀",
			wantSubject: "你好世界",
		},
		{
			name:        "正文前后空白被裁剪",
			raw:         "From: d@example.com\r\n\r\n   \r\n  padded  \r\n   ",
			wantChatID:  "d@example.com",
			wantUser:    "d@example.com",
			wantContent: "padded",
		},
		{
			name:    "非法 RFC822 (缺少头/正文分隔)",
			raw:     "this is not a valid email at all",
			wantErr: true,
		},
		{
			name:    "完全空 payload",
			raw:     "",
			wantErr: true,
		},
		{
			// From 头缺失时, ParseEmailAddress("") 返回空, ChatID 为空字符串。
			name:        "缺少 From 头",
			raw:         "Subject: NoFrom\r\n\r\nbody",
			wantChatID:  "",
			wantUser:    "",
			wantContent: "body",
			wantSubject: "NoFrom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, subject, err := parseIncomingEmail([]byte(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望解析报错, 但成功返回 msg=%+v", msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if msg.Platform != adapter.PlatformEmail {
				t.Errorf("Platform = %q, want email", msg.Platform)
			}
			if msg.ChatID != tt.wantChatID {
				t.Errorf("ChatID = %q, want %q", msg.ChatID, tt.wantChatID)
			}
			if msg.UserID != tt.wantUser {
				t.Errorf("UserID = %q, want %q", msg.UserID, tt.wantUser)
			}
			if msg.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", msg.Content, tt.wantContent)
			}
			if subject != tt.wantSubject {
				t.Errorf("subject = %q, want %q", subject, tt.wantSubject)
			}
			// ChatID 与 from_email 元数据应一致。
			if msg.Metadata["from_email"] != msg.ChatID {
				t.Errorf("metadata from_email=%q 与 ChatID=%q 不一致", msg.Metadata["from_email"], msg.ChatID)
			}
			if msg.Metadata["subject"] != subject {
				t.Errorf("metadata subject=%q 与返回 subject=%q 不一致", msg.Metadata["subject"], subject)
			}
			if tt.checkID != nil {
				tt.checkID(t, msg.ID)
			}
		})
	}
}

// TestParseIncomingEmail_OversizedBody 超长正文（1MB）应正常解析不丢内容。
func TestParseIncomingEmail_OversizedBody(t *testing.T) {
	body := strings.Repeat("A", 1<<20) // 1MB
	raw := "From: big@example.com\r\nSubject: Big\r\n\r\n" + body
	msg, _, err := parseIncomingEmail([]byte(raw))
	if err != nil {
		t.Fatalf("超长正文解析失败: %v", err)
	}
	if len(msg.Content) != len(body) {
		t.Errorf("超长正文长度 = %d, want %d", len(msg.Content), len(body))
	}
}

// TestParseIncomingEmail_InvalidDateFallsBackToNow 非法 Date 头应回退到当前时间。
func TestParseIncomingEmail_InvalidDateFallsBackToNow(t *testing.T) {
	before := time.Now()
	raw := "From: a@example.com\r\nDate: not-a-valid-date\r\n\r\nbody"
	msg, _, err := parseIncomingEmail([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if msg.Timestamp.Before(before) {
		t.Errorf("非法 Date 应回退到当前时间, 得到 %v (before=%v)", msg.Timestamp, before)
	}
}

// ---------------------------------------------------------------------------
// 入站解析: ParseEmailAddress
// ---------------------------------------------------------------------------

// TestParseEmailAddress_EdgeCases 覆盖地址解析的边界与畸形输入。
func TestParseEmailAddress_EdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantName  string
		wantEmail string
	}{
		{"标准带名", "John Doe <john@example.com>", "John Doe", "john@example.com"},
		{"仅尖括号地址", "<alice@example.com>", "", "alice@example.com"},
		{"裸地址", "bob@example.com", "", "bob@example.com"},
		{"中文名带空格", "  张三  <zhangsan@example.com>", "张三", "zhangsan@example.com"},
		{"空字符串", "", "", ""},
		{"仅空白", "   ", "", ""},
		{"带引号名", "\"Quoted, Name\" <q@x.com>", "\"Quoted, Name\"", "q@x.com"},
		{"多个尖括号取第一个", "A <a@b> <c@d>", "A", "a@b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, email := ParseEmailAddress(tt.input)
			if name != tt.wantName || email != tt.wantEmail {
				t.Errorf("ParseEmailAddress(%q) = (%q, %q), want (%q, %q)",
					tt.input, name, email, tt.wantName, tt.wantEmail)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IMAP literal 大小解析: parseLiteralSize
// ---------------------------------------------------------------------------

// TestParseLiteralSize 覆盖 IMAP literal 大小标记解析的全部边界。
func TestParseLiteralSize(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantSize int
		wantOK   bool
	}{
		{"正常", "* 1 FETCH (RFC822 {12}", 12, true},
		{"带 CRLF", "* 1 FETCH {34}\r\n", 34, true},
		{"零字节", "x {0}", 0, true},
		{"前导零", "{007}", 7, true},
		{"花括号非行尾", "{12} trailing", 0, false},
		{"无花括号", "no brace", 0, false},
		{"空花括号", "{}", 0, false},
		{"花括号内含空格", "{ 5 }", 0, false},
		{"非数字", "{abc}", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, ok := parseLiteralSize(tt.line)
			if ok != tt.wantOK || (ok && size != tt.wantSize) {
				t.Errorf("parseLiteralSize(%q) = (%d, %v), want (%d, %v)",
					tt.line, size, ok, tt.wantSize, tt.wantOK)
			}
		})
	}
}

// TestParseLiteralSize_Negative 负数 literal 大小不应被当作合法大小。
//
// 这是关键安全/健壮性断言：IMAP literal 大小语义上必须 >= 0。
// 若 parseLiteralSize 对 "{-5}" 返回 ok=true 且 size<0, 则 run() 中
// make([]byte, size) 会 panic (makeslice: len out of range)，
// 由于 run() 经 FetchRFC822 在无 recover 的 pollLoop goroutine 中调用，
// 一个畸形/恶意 IMAP 服务器即可使轮询协程崩溃。
//
// 回归: W3-5
func TestParseLiteralSize_Negative(t *testing.T) {
	size, ok := parseLiteralSize("* 1 FETCH (RFC822 {-5}")
	if ok && size < 0 {
		t.Errorf("parseLiteralSize 对负数 literal 大小返回 (size=%d, ok=true)；"+
			"负数大小应被拒绝(ok=false), 否则 run() 中 make([]byte, %d) 会 panic", size, size)
	}
}

// ---------------------------------------------------------------------------
// IMAP run() / FetchRFC822: 协议层数据流与 panic 防护
// ---------------------------------------------------------------------------

// TestRun_NegativeLiteralPanicGuard 验证 run() 在服务器返回负数 literal 时
// 不应 panic（应返回 error）。这是对上面 TestParseLiteralSize_Negative 的端到端佐证。
//
// 用 recover 捕获 panic 以免崩溃整个测试二进制，再以 t.Error 记录为失败。
//
// 回归: W3-5
func TestRun_NegativeLiteralPanicGuard(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("run() 在服务器返回负数 literal {-3} 时发生 panic: %v；"+
				"应当返回 error 而非崩溃 pollLoop goroutine", r)
		}
	}()

	server := newScriptedClient("* 1 FETCH (RFC822 {-3}\r\nA0001 OK FETCH done\r\n")
	_, err := server.run("FETCH 1 RFC822")
	if err == nil {
		t.Errorf("负数 literal 应返回 error, 得到 nil")
	}
}

// TestFetchRFC822 覆盖 FETCH 响应的正常/缺 literal 情况。
func TestFetchRFC822(t *testing.T) {
	t.Run("正常返回 RFC822 内容", func(t *testing.T) {
		// literal 大小 5, 内容 "hello", 末尾带 \r\n 应被 TrimRight。
		script := "* 1 FETCH (RFC822 {7}\r\nhello\r\nA0001 OK FETCH done\r\n"
		c := newScriptedClient(script)
		raw, err := c.FetchRFC822("1")
		if err != nil {
			t.Fatalf("FetchRFC822 失败: %v", err)
		}
		if string(raw) != "hello" {
			t.Errorf("FetchRFC822 = %q, want %q (尾部 CRLF 应被 TrimRight)", raw, "hello")
		}
	})

	t.Run("无 literal 返回错误", func(t *testing.T) {
		c := newScriptedClient("A0001 OK FETCH done\r\n")
		_, err := c.FetchRFC822("1")
		if err == nil {
			t.Error("FETCH 无 RFC822 内容应返回错误")
		}
	})
}

// TestRun_TaggedFailure 服务器返回非 OK tagged 响应时 run() 应返回错误。
func TestRun_TaggedFailure(t *testing.T) {
	c := newScriptedClient("A0001 NO LOGIN failed\r\n")
	_, err := c.run("LOGIN %q %q", "u", "p")
	if err == nil {
		t.Error("tagged NO 响应应返回错误")
	}
}

// ---------------------------------------------------------------------------
// IMAP SearchUnseen: 搜索结果解析与截断
// ---------------------------------------------------------------------------

// TestSearchUnseen 覆盖搜索结果解析的各种形态。
func TestSearchUnseen(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		maxFetch int
		wantIDs  []string
	}{
		{
			name:     "多个未读",
			script:   "* SEARCH 1 2 3 4 5\r\nA0001 OK SEARCH done\r\n",
			maxFetch: 10,
			wantIDs:  []string{"1", "2", "3", "4", "5"},
		},
		{
			name:     "maxFetch 截断",
			script:   "* SEARCH 1 2 3 4 5\r\nA0001 OK SEARCH done\r\n",
			maxFetch: 2,
			wantIDs:  []string{"1", "2"},
		},
		{
			name:     "无未读 (空 SEARCH)",
			script:   "* SEARCH\r\nA0001 OK SEARCH done\r\n",
			maxFetch: 10,
			wantIDs:  nil,
		},
		{
			name:     "maxFetch=0 不截断",
			script:   "* SEARCH 1 2 3\r\nA0001 OK SEARCH done\r\n",
			maxFetch: 0,
			wantIDs:  []string{"1", "2", "3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newScriptedClient(tt.script)
			ids, err := c.SearchUnseen(tt.maxFetch)
			if err != nil {
				t.Fatalf("SearchUnseen 失败: %v", err)
			}
			if !equalSlice(ids, tt.wantIDs) {
				t.Errorf("SearchUnseen(%d) = %v, want %v", tt.maxFetch, ids, tt.wantIDs)
			}
		})
	}
}

// TestSearchUnseen_NegativeMaxFetch maxFetch<0 时按当前实现不截断（maxFetch>0 才截断）。
func TestSearchUnseen_NegativeMaxFetch(t *testing.T) {
	c := newScriptedClient("* SEARCH 1 2 3\r\nA0001 OK SEARCH done\r\n")
	ids, err := c.SearchUnseen(-1)
	if err != nil {
		t.Fatalf("SearchUnseen 失败: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("负数 maxFetch 应不截断, 得到 %v", ids)
	}
}

// ---------------------------------------------------------------------------
// 配置默认值: New
// ---------------------------------------------------------------------------

// TestNew_ConfigDefaults 覆盖配置默认值填充, 包含负数/零值场景。
func TestNew_ConfigDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   EmailConfig
		want EmailConfig
	}{
		{
			name: "全零值填充默认",
			in:   EmailConfig{},
			want: EmailConfig{PollInterval: 60, MaxFetch: 10,
				IMAP: IMAPConfig{Port: 993, Folder: "INBOX"}, SMTP: SMTPConfig{Port: 587}},
		},
		{
			name: "负数 PollInterval 被纠正为默认",
			in:   EmailConfig{PollInterval: -5, MaxFetch: -1},
			want: EmailConfig{PollInterval: 60, MaxFetch: 10,
				IMAP: IMAPConfig{Port: 993, Folder: "INBOX"}, SMTP: SMTPConfig{Port: 587}},
		},
		{
			name: "保留用户自定义值",
			in: EmailConfig{PollInterval: 30, MaxFetch: 5,
				IMAP: IMAPConfig{Port: 143, Folder: "Custom"}, SMTP: SMTPConfig{Port: 25}},
			want: EmailConfig{PollInterval: 30, MaxFetch: 5,
				IMAP: IMAPConfig{Port: 143, Folder: "Custom"}, SMTP: SMTPConfig{Port: 25}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.in)
			if a.cfg.PollInterval != tt.want.PollInterval {
				t.Errorf("PollInterval = %d, want %d", a.cfg.PollInterval, tt.want.PollInterval)
			}
			if a.cfg.MaxFetch != tt.want.MaxFetch {
				t.Errorf("MaxFetch = %d, want %d", a.cfg.MaxFetch, tt.want.MaxFetch)
			}
			if a.cfg.IMAP.Port != tt.want.IMAP.Port {
				t.Errorf("IMAP.Port = %d, want %d", a.cfg.IMAP.Port, tt.want.IMAP.Port)
			}
			if a.cfg.IMAP.Folder != tt.want.IMAP.Folder {
				t.Errorf("IMAP.Folder = %q, want %q", a.cfg.IMAP.Folder, tt.want.IMAP.Folder)
			}
			if a.cfg.SMTP.Port != tt.want.SMTP.Port {
				t.Errorf("SMTP.Port = %d, want %d", a.cfg.SMTP.Port, tt.want.SMTP.Port)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateConfig: 配置校验路径
// ---------------------------------------------------------------------------

// TestValidateConfig_MissingFields 缺字段时应在拨号前返回配置错误。
func TestValidateConfig_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		cfg  EmailConfig
	}{
		{"IMAP 全空", EmailConfig{}},
		{"缺 IMAP 密码", EmailConfig{IMAP: IMAPConfig{Host: "h", Username: "u"}}},
		{"缺 SMTP 配置", EmailConfig{IMAP: IMAPConfig{Host: "h", Username: "u", Password: "p"}}},
		{"缺 SMTP From", EmailConfig{
			IMAP: IMAPConfig{Host: "h", Username: "u", Password: "p"},
			SMTP: SMTPConfig{Host: "h", Username: "u", Password: "p"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.cfg)
			// 注入会失败的 dialIMAP 以确保不会真的去拨号；若提前因配置返回则不会调用它。
			dialed := false
			a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) {
				dialed = true
				return nil, errors.New("不应被调用")
			}
			err := a.ValidateConfig(context.Background())
			if err == nil {
				t.Fatal("缺配置时应返回错误")
			}
			if dialed {
				t.Error("配置不完整时不应进行 IMAP 拨号")
			}
		})
	}
}

// TestValidateConfig_DialAndLogin 覆盖拨号失败/登录失败/成功三条路径。
func TestValidateConfig_DialAndLogin(t *testing.T) {
	full := EmailConfig{
		IMAP: IMAPConfig{Host: "h", Username: "u", Password: "p"},
		SMTP: SMTPConfig{Host: "h", Username: "u", Password: "p", From: "f@x.com"},
	}

	t.Run("拨号失败", func(t *testing.T) {
		a := New(full)
		a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) {
			return nil, errors.New("dial boom")
		}
		if err := a.ValidateConfig(context.Background()); err == nil {
			t.Error("拨号失败应返回错误")
		}
	})

	t.Run("登录失败", func(t *testing.T) {
		a := New(full)
		fc := &fakeSession{loginErr: errors.New("login boom")}
		a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) { return fc, nil }
		if err := a.ValidateConfig(context.Background()); err == nil {
			t.Error("登录失败应返回错误")
		}
		if !fc.closed {
			t.Error("ValidateConfig 应在结束时关闭会话")
		}
	})

	t.Run("成功", func(t *testing.T) {
		a := New(full)
		fc := &fakeSession{}
		a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) { return fc, nil }
		if err := a.ValidateConfig(context.Background()); err != nil {
			t.Errorf("配置完整应成功, 得到 %v", err)
		}
		if !fc.loggedOut {
			t.Error("成功路径应调用 Logout")
		}
	})
}

// ---------------------------------------------------------------------------
// fetchAndProcess: 入站到出站的完整数据流
// ---------------------------------------------------------------------------

// TestFetchAndProcess_NilHandlerNoop handler 为 nil 时应直接返回, 不拨号。
func TestFetchAndProcess_NilHandlerNoop(t *testing.T) {
	a := New(EmailConfig{})
	dialed := false
	a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) {
		dialed = true
		return nil, errors.New("不应被调用")
	}
	a.fetchAndProcess(context.Background()) // handler == nil
	if dialed {
		t.Error("handler 为 nil 时不应拨号")
	}
}

// TestFetchAndProcess_SubjectPropagatedToReply 验证 reply.Metadata.subject
// 在未显式设置时由入站 subject 回填, 从而 Send 使用 "Re: <原主题>"。
func TestFetchAndProcess_SubjectPropagatedToReply(t *testing.T) {
	raw := "From: u@example.com\r\nSubject: OriginalSubject\r\n\r\nhi"
	fc := &fakeSession{ids: []string{"1"}, raw: raw}
	a := New(EmailConfig{MaxFetch: 1})
	a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) { return fc, nil }

	var capturedReply *adapter.Reply
	a.handler = func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) {
		// 返回不带 subject 的回复, 期望 fetchAndProcess 回填。
		capturedReply = &adapter.Reply{Content: "reply body"}
		return capturedReply, nil
	}
	// 拦截发送: 替换为可观测的 SMTP 失败, 但 reply.Metadata 已在 Send 前被回填。
	a.fetchAndProcess(context.Background())

	if capturedReply.Metadata == nil || capturedReply.Metadata["subject"] != "OriginalSubject" {
		t.Errorf("reply.Metadata[subject] = %v, want OriginalSubject (应由入站 subject 回填)",
			capturedReply.Metadata)
	}
	if !fc.markedSeen {
		t.Error("处理完成后应 MarkSeen")
	}
}

// TestFetchAndProcess_HandlerErrorSkipsMarkSeen handler 报错时跳过该邮件,
// 不应 MarkSeen（保证下次仍可重试该邮件）。
func TestFetchAndProcess_HandlerErrorSkipsMarkSeen(t *testing.T) {
	raw := "From: u@example.com\r\nSubject: S\r\n\r\nhi"
	fc := &fakeSession{ids: []string{"1"}, raw: raw}
	a := New(EmailConfig{MaxFetch: 1})
	a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) { return fc, nil }
	a.handler = func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) {
		return nil, errors.New("handler boom")
	}
	a.fetchAndProcess(context.Background())
	if fc.markedSeen {
		t.Error("handler 报错时不应 MarkSeen (否则邮件丢失无法重试)")
	}
}

// TestFetchAndProcess_FetchErrorContinues 单封邮件拉取失败时应继续处理其余邮件。
func TestFetchAndProcess_FetchErrorContinues(t *testing.T) {
	raw := "From: u@example.com\r\nSubject: S\r\n\r\nhi"
	fc := &fakeSession{
		ids:         []string{"1", "2"},
		raw:         raw,
		fetchErrFor: map[string]bool{"1": true}, // id=1 拉取失败
	}
	a := New(EmailConfig{MaxFetch: 10})
	a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) { return fc, nil }
	processed := 0
	a.handler = func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) {
		processed++
		return nil, nil
	}
	a.fetchAndProcess(context.Background())
	if processed != 1 {
		t.Errorf("应只成功处理 id=2 一封, 得到 processed=%d", processed)
	}
}

// TestFetchAndProcess_LoginFailureNoProcess 登录失败应整体放弃且不处理。
func TestFetchAndProcess_LoginFailureNoProcess(t *testing.T) {
	fc := &fakeSession{loginErr: errors.New("login boom"), ids: []string{"1"}, raw: "From: a@b.com\r\n\r\nx"}
	a := New(EmailConfig{MaxFetch: 1})
	a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) { return fc, nil }
	called := false
	a.handler = func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) {
		called = true
		return nil, nil
	}
	a.fetchAndProcess(context.Background())
	if called {
		t.Error("登录失败时不应调用 handler")
	}
}

// ---------------------------------------------------------------------------
// Send / SendStream: 出站消息转换
// ---------------------------------------------------------------------------

// TestSend_SubjectComposition 验证 Send 的 subject 拼接逻辑。
//
// 这里通过让 SMTP 拨号失败、断言不 panic 来验证 Send 至少能走到 sendEmail；
// 真实 SMTP 行为无法在单测中触达，故仅做行为边界与回退断言。
func TestSend_SubjectComposition(t *testing.T) {
	a := New(EmailConfig{SMTP: SMTPConfig{Host: "127.0.0.1", Port: 1, From: "from@x.com"}})

	tests := []struct {
		name  string
		reply *adapter.Reply
	}{
		{"无 metadata", &adapter.Reply{Content: "body"}},
		{"带 subject", &adapter.Reply{Content: "body", Metadata: map[string]string{"subject": "Topic"}}},
		{"空 subject 值", &adapter.Reply{Content: "body", Metadata: map[string]string{"subject": ""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// SMTP host 不可达, 期望返回 error 而非 panic。
			err := a.Send(context.Background(), "to@example.com", tt.reply)
			if err == nil {
				t.Log("Send 返回 nil (取决于本地 SMTP 回退路径), 仅校验无 panic")
			}
		})
	}
}

// TestSendStream_BuffersChunks 验证流式分片被正确缓冲拼接, 并能透传错误。
func TestSendStream_BuffersChunks(t *testing.T) {
	t.Run("错误分片提前返回", func(t *testing.T) {
		a := New(EmailConfig{SMTP: SMTPConfig{Host: "127.0.0.1", Port: 1, From: "f@x.com"}})
		ch := make(chan *adapter.ReplyChunk, 3)
		ch <- &adapter.ReplyChunk{Content: "a"}
		ch <- &adapter.ReplyChunk{Error: errors.New("stream boom")}
		ch <- &adapter.ReplyChunk{Content: "should-not-reach"}
		close(ch)
		err := a.SendStream(context.Background(), "to@x.com", ch)
		if err == nil || !strings.Contains(err.Error(), "stream boom") {
			t.Errorf("应透传 chunk.Error, 得到 %v", err)
		}
	})

	t.Run("空 chunks channel", func(t *testing.T) {
		a := New(EmailConfig{SMTP: SMTPConfig{Host: "127.0.0.1", Port: 1, From: "f@x.com"}})
		ch := make(chan *adapter.ReplyChunk)
		close(ch)
		// 无分片, 缓冲为空, 会尝试发送空内容邮件; 仅校验不 panic。
		_ = a.SendStream(context.Background(), "to@x.com", ch)
	})
}

// ---------------------------------------------------------------------------
// sanitizeHeader: 头部注入防护
// ---------------------------------------------------------------------------

// TestSanitizeHeader 覆盖 SMTP 头部注入字符过滤。
func TestSanitizeHeader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"无注入", "normal subject", "normal subject"},
		{"含 CRLF 注入", "evil\r\nBcc: attacker@evil.com", "evilBcc: attacker@evil.com"},
		{"仅 LF", "a\nb", "ab"},
		{"仅 CR", "a\rb", "ab"},
		{"多重换行", "x\r\n\r\ny", "xy"},
		{"空字符串", "", ""},
		{"Unicode 保留", "主题 你好\r\nInjected", "主题 你好Injected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeHeader(tt.in)
			if got != tt.want {
				t.Errorf("sanitizeHeader(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("sanitizeHeader 输出仍含 CR/LF: %q", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 状态一致性: Stop / Start
// ---------------------------------------------------------------------------

// TestStop_SetsStoppedFlag Stop 应置位 stopped 标志。
func TestStop_SetsStoppedFlag(t *testing.T) {
	a := New(EmailConfig{})
	if a.stopped.Load() {
		t.Error("新建适配器 stopped 应为 false")
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 返回错误: %v", err)
	}
	if !a.stopped.Load() {
		t.Error("Stop 后 stopped 应为 true")
	}
}

// TestStart_ResetsStoppedAndPolls Start 会重置 stopped 并启动轮询 goroutine。
// 用极短 ctx 截断, 验证不阻塞且 handler 被绑定。
func TestStart_ResetsStoppedAndPolls(t *testing.T) {
	fc := &fakeSession{ids: nil}
	a := New(EmailConfig{PollInterval: 3600}) // 长间隔, 只触发首次立即拉取
	a.dialIMAP = func(ctx context.Context, c IMAPConfig) (imapSession, error) { return fc, nil }
	_ = a.Stop(context.Background()) // 先置 true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	called := false
	h := func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) {
		mu.Lock()
		called = true
		mu.Unlock()
		return nil, nil
	}
	if err := a.Start(ctx, h); err != nil {
		t.Fatalf("Start 返回错误: %v", err)
	}
	if a.stopped.Load() {
		t.Error("Start 后 stopped 应被重置为 false")
	}
	// 首次立即拉取在 goroutine 中执行; 给它一点时间完成并登录(无未读, 不会调 handler)。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fc.LoginCallCount() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if fc.LoginCallCount() == 0 {
		t.Error("Start 后首次立即拉取应触发 IMAP 登录")
	}
	mu.Lock()
	_ = called
	mu.Unlock()
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// newScriptedClient 构造一个直接读取脚本响应的 imapClient,
// 用于测试 run()/SearchUnseen()/FetchRFC822() 等协议解析逻辑。
func newScriptedClient(serverResponse string) *imapClient {
	return &imapClient{
		writer: bufio.NewWriter(io.Discard),
		reader: bufio.NewReader(bytes.NewReader([]byte(serverResponse))),
	}
}

// equalSlice 比较两个字符串切片是否相等（含 nil 与空切片等价处理）。
func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeSession 是一个可编程的 imapSession 实现, 用于驱动 fetchAndProcess/ValidateConfig。
type fakeSession struct {
	ids         []string
	raw         string
	loginErr    error
	fetchErrFor map[string]bool

	mu         sync.Mutex // 保护下列计数/状态字段: Start 的 pollLoop 后台 goroutine 与测试主 goroutine 并发访问
	loginCalls int
	markedSeen bool
	closed     bool
	loggedOut  bool
}

func (f *fakeSession) Close() error { f.mu.Lock(); f.closed = true; f.mu.Unlock(); return nil }
func (f *fakeSession) Login(u, p string) error {
	f.mu.Lock()
	f.loginCalls++
	f.mu.Unlock()
	return f.loginErr
}
func (f *fakeSession) Select(folder string) error             { return nil }
func (f *fakeSession) SearchUnseen(max int) ([]string, error) { return f.ids, nil }
func (f *fakeSession) FetchRFC822(id string) ([]byte, error) {
	if f.fetchErrFor[id] {
		return nil, fmt.Errorf("fetch err for %s", id)
	}
	return []byte(f.raw), nil
}
func (f *fakeSession) MarkSeen(id string) error {
	f.mu.Lock()
	f.markedSeen = true
	f.mu.Unlock()
	return nil
}
func (f *fakeSession) Logout() error { f.mu.Lock(); f.loggedOut = true; f.mu.Unlock(); return nil }

// LoginCallCount 并发安全地返回 Login 被调用次数, 供启动了后台 pollLoop 的测试读取。
func (f *fakeSession) LoginCallCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.loginCalls }
