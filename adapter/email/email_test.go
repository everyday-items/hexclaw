package email

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestParseEmailAddress(t *testing.T) {
	tests := []struct {
		input     string
		wantName  string
		wantEmail string
	}{
		{"John Doe <john@example.com>", "John Doe", "john@example.com"},
		{"<alice@example.com>", "", "alice@example.com"},
		{"bob@example.com", "", "bob@example.com"},
		{"  张三  <zhangsan@example.com>", "张三", "zhangsan@example.com"},
	}

	for _, tt := range tests {
		name, email := ParseEmailAddress(tt.input)
		if name != tt.wantName || email != tt.wantEmail {
			t.Errorf("ParseEmailAddress(%q) = (%q, %q), want (%q, %q)",
				tt.input, name, email, tt.wantName, tt.wantEmail)
		}
	}
}

func TestNewDefaults(t *testing.T) {
	a := New(EmailConfig{})

	if a.cfg.PollInterval != 60 {
		t.Errorf("默认轮询间隔应为 60，得到 %d", a.cfg.PollInterval)
	}
	if a.cfg.MaxFetch != 10 {
		t.Errorf("默认 MaxFetch 应为 10，得到 %d", a.cfg.MaxFetch)
	}
	if a.cfg.IMAP.Port != 993 {
		t.Errorf("默认 IMAP 端口应为 993，得到 %d", a.cfg.IMAP.Port)
	}
	if a.cfg.IMAP.Folder != "INBOX" {
		t.Errorf("默认 Folder 应为 INBOX，得到 %s", a.cfg.IMAP.Folder)
	}
	if a.cfg.SMTP.Port != 587 {
		t.Errorf("默认 SMTP 端口应为 587，得到 %d", a.cfg.SMTP.Port)
	}
}

func TestNameAndPlatform(t *testing.T) {
	a := New(EmailConfig{})
	if a.Name() != "email" {
		t.Errorf("Name() = %s, want email", a.Name())
	}
	if a.Platform() != "email" {
		t.Errorf("Platform() = %s, want email", a.Platform())
	}
}

func TestFetchAndProcess_ProcessesUnreadEmail(t *testing.T) {
	rawMessage := "From: Alice <alice@example.com>\r\n" +
		"Subject: Hello\r\n" +
		"Message-ID: <msg-1@example.com>\r\n" +
		"Date: Fri, 20 Mar 2026 10:00:00 +0800\r\n" +
		"\r\n" +
		"email body\r\n"

	client := &fakeIMAPClient{rawMessage: rawMessage}

	a := New(EmailConfig{MaxFetch: 1})
	a.dialIMAP = func(ctx context.Context, cfg IMAPConfig) (imapSession, error) {
		return client, nil
	}

	var gotEmail, gotSubject, gotContent string
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		gotEmail = msg.UserID
		gotSubject = msg.Metadata["subject"]
		gotContent = msg.Content
		return nil, nil
	}

	a.fetchAndProcess(context.Background())

	if gotEmail != "alice@example.com" {
		t.Fatalf("handler user id = %q, want alice@example.com", gotEmail)
	}
	if gotSubject != "Hello" {
		t.Fatalf("handler subject = %q, want Hello", gotSubject)
	}
	if gotContent != "email body" {
		t.Fatalf("handler content = %q, want %q", gotContent, "email body")
	}
	if !client.sawStoreSeen {
		t.Fatal("expected STORE +FLAGS (\\Seen) after processing message")
	}
}

type fakeIMAPClient struct {
	rawMessage   string
	sawStoreSeen bool
}

func (c *fakeIMAPClient) Close() error                                { return nil }
func (c *fakeIMAPClient) Login(username, password string) error       { return nil }
func (c *fakeIMAPClient) Select(folder string) error                  { return nil }
func (c *fakeIMAPClient) SearchUnseen(maxFetch int) ([]string, error) { return []string{"1"}, nil }
func (c *fakeIMAPClient) FetchRFC822(id string) ([]byte, error)       { return []byte(c.rawMessage), nil }
func (c *fakeIMAPClient) MarkSeen(id string) error {
	c.sawStoreSeen = true
	return nil
}
func (c *fakeIMAPClient) Logout() error { return nil }
