package whatsapp

import (
	"testing"
)

func TestWhatsAppAdapter_NameAndPlatform(t *testing.T) {
	a := New(Config{Token: "test", PhoneID: "123"})
	if a.Name() != "whatsapp" {
		t.Errorf("期望 whatsapp, 得到 %s", a.Name())
	}
	if a.Platform() != PlatformWhatsApp {
		t.Errorf("期望 whatsapp, 得到 %s", a.Platform())
	}
}

func TestWhatsAppAdapter_DefaultConfig(t *testing.T) {
	a := New(Config{})
	if a.config.WebhookPort != 6063 {
		t.Errorf("期望默认端口 6063, 得到 %d", a.config.WebhookPort)
	}
	if a.config.BaseURL == "" {
		t.Error("BaseURL 不应为空")
	}
}

func TestWhatsAppAdapter_GetContactName(t *testing.T) {
	a := New(Config{})

	contacts := []whatsappContact{
		{WaID: "86138001380001", Profile: whatsappProfile{Name: "张三"}},
		{WaID: "86139001390002", Profile: whatsappProfile{Name: "李四"}},
	}

	name := a.getContactName(contacts, "86138001380001")
	if name != "张三" {
		t.Errorf("期望 张三, 得到 %s", name)
	}

	name = a.getContactName(contacts, "unknown")
	if name != "unknown" {
		t.Errorf("未知用户应返回 waID, 得到 %s", name)
	}
}
