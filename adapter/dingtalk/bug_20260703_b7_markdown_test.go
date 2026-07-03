package dingtalk

// BUG-20260703 B7：钉钉回复显示裸 markdown。
//
// 现象：钉钉里问"你是谁"，回复含 `### 🧠 1. ...` 裸标题——出站消息硬编码
// sampleText（纯文本），钉钉 text 消息不渲染 markdown，LLM 产出的标题/加粗
// 原样刺给用户。
//
// 契约：出站回复走钉钉 sampleMarkdown 消息类型（{"title","text"} 载荷，title
// 必填、从正文派生），钉钉客户端原生渲染标题/加粗/链接/列表子集。

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// 契约层（源码级，仿 official_sdk_contract_test.go）：回复发送实现必须使用
// sampleMarkdown + markdown 载荷 marshal，且不再硬编码 sampleText。
func TestBug20260703_B7_OutboundUsesSampleMarkdownContract(t *testing.T) {
	src, err := os.ReadFile("dingtalk.go")
	if err != nil {
		t.Fatalf("读取 dingtalk.go 失败: %v", err)
	}
	code := string(src)
	if !strings.Contains(code, `"sampleMarkdown"`) {
		t.Errorf("B7: 出站消息应使用钉钉 sampleMarkdown 消息类型（当前缺失）")
	}
	if strings.Contains(code, `SetMsgKey("sampleText")`) {
		t.Errorf("B7: 出站消息仍硬编码 sampleText——钉钉 text 消息不渲染 markdown，### 标题会裸露")
	}
}

// 行为层：Send 出站经 fake 捕获，必须是 sampleMarkdown 且载荷 {title 非空, text=原文}。
func TestBug20260703_B7_SendDeliversMarkdownPayload(t *testing.T) {
	a := newTestAdapter()
	fakeAPI := newFakeDingtalkOpenAPI("tok")
	a.openAPI = fakeAPI

	content := "### 🧠 1. 我是谁\n**我是小蟹**，你的学习伙伴。"
	if err := a.Send(context.Background(), "user1", &adapter.Reply{Content: content}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	calls := fakeAPI.SendCalls()
	if len(calls) != 1 {
		t.Fatalf("send 调用次数 = %d, 期望 1", len(calls))
	}
	if calls[0].MsgKey != "sampleMarkdown" {
		t.Fatalf("B7: MsgKey = %q, 期望 sampleMarkdown（text 消息不渲染 markdown）", calls[0].MsgKey)
	}
	var param struct {
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(calls[0].MsgParam), &param); err != nil {
		t.Fatalf("MsgParam 非法 JSON: %v (%q)", err, calls[0].MsgParam)
	}
	if param.Text != content {
		t.Errorf("B7: markdown 载荷 text = %q, 期望原文 %q", param.Text, content)
	}
	if strings.TrimSpace(param.Title) == "" {
		t.Errorf("B7: sampleMarkdown 的 title 必填（推送通知摘要），不得为空")
	}
	if strings.Contains(param.Title, "#") {
		t.Errorf("B7: title 应剥掉 markdown 标记, got %q", param.Title)
	}
}

// title 派生的边界：sampleMarkdown 的 title 必填（推送通知摘要），任何内容形状下
// 都不得为空、不得带行首 markdown 标记、不得超长。
func TestBug20260703_B7_MessageTitleEdgeCases(t *testing.T) {
	longLine := strings.Repeat("很长的标题内容", 10) // 70 runes
	tests := []struct {
		name    string
		content string
		want    func(t *testing.T, title string)
	}{
		{"markdown 标题行", "### 🧠 1. 我是谁\n正文", func(t *testing.T, title string) {
			if !strings.HasPrefix(title, "🧠 1. 我是谁") {
				t.Errorf("应剥 ### 取首行, got %q", title)
			}
		}},
		{"引用行开头", "> 引用要点\n正文", func(t *testing.T, title string) {
			if !strings.HasPrefix(title, "引用要点") {
				t.Errorf("应剥 > 取首行, got %q", title)
			}
		}},
		{"首行空白跳过", "\n\n  \n第一段正文", func(t *testing.T, title string) {
			if !strings.HasPrefix(title, "第一段正文") {
				t.Errorf("应跳过空白行, got %q", title)
			}
		}},
		{"空内容回退品牌名", "", func(t *testing.T, title string) {
			if title != "小蟹回复" {
				t.Errorf("空内容应回退默认 title, got %q", title)
			}
		}},
		{"纯标记行回退品牌名", "### \n> \n- ", func(t *testing.T, title string) {
			if title != "小蟹回复" {
				t.Errorf("纯标记内容应回退默认 title, got %q", title)
			}
		}},
		{"超长截断", longLine, func(t *testing.T, title string) {
			if n := len([]rune(title)); n > 33 { // 30 + 截断省略号裕量
				t.Errorf("title 应按 rune 截断（≤~30），got %d runes: %q", n, title)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title := dingtalkMessageTitle(tt.content)
			if strings.TrimSpace(title) == "" {
				t.Fatalf("title 不得为空")
			}
			tt.want(t, title)
		})
	}
}
