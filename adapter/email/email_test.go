package email

import (
	"testing"
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
