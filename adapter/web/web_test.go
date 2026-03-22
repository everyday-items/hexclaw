package web

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// TestNew 测试创建 Web 适配器
func TestNew(t *testing.T) {
	a := New()
	if a == nil {
		t.Fatal("New() 返回 nil")
	}
}

// TestName 测试 Name() 返回值
func TestName(t *testing.T) {
	a := New()
	if got := a.Name(); got != "web" {
		t.Errorf("Name() = %q, 期望 %q", got, "web")
	}
}

// TestPlatform 测试 Platform() 返回值
func TestPlatform(t *testing.T) {
	a := New()
	if got := a.Platform(); got != adapter.PlatformWeb {
		t.Errorf("Platform() = %q, 期望 %q", got, adapter.PlatformWeb)
	}
}

// TestHandler 测试 Handler() 返回非 nil 的 http.Handler
func TestHandler(t *testing.T) {
	a := New()
	handler := a.Handler()
	if handler == nil {
		t.Fatal("Handler() 返回 nil")
	}
}

// TestStart 测试 Start 设置 handler
func TestStart(t *testing.T) {
	a := New()
	if a.handler != nil {
		t.Error("初始 handler 应为 nil")
	}

	handler := func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "ok"}, nil
	}

	err := a.Start(context.Background(), handler)
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if a.handler == nil {
		t.Error("Start 后 handler 不应为 nil")
	}
}

// TestStop 测试 Stop 关闭所有连接
func TestStop(t *testing.T) {
	a := New()
	// 没有连接时 Stop 不应报错
	err := a.Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
}

// TestGetConnNotFound 测试获取不存在的连接
func TestGetConnNotFound(t *testing.T) {
	a := New()
	conn, ok := a.getConn("nonexistent")
	if ok {
		t.Error("不存在的 chatID 不应返回 ok=true")
	}
	if conn != nil {
		t.Error("不存在的 chatID 不应返回 conn")
	}
}

// TestSendNoConn 测试向不存在的连接发送消息（应静默忽略）
func TestSendNoConn(t *testing.T) {
	a := New()
	err := a.Send(context.Background(), "nonexistent", &adapter.Reply{Content: "hello"})
	// 连接不存在时应静默忽略（返回 nil）
	if err != nil {
		t.Errorf("Send 到不存在连接应返回 nil，实际: %v", err)
	}
}

// TestSendStreamNoConn 测试向不存在的连接流式发送（应静默忽略）
func TestSendStreamNoConn(t *testing.T) {
	a := New()
	chunks := make(chan *adapter.ReplyChunk, 1)
	chunks <- &adapter.ReplyChunk{Content: "hello", Done: true}
	close(chunks)

	err := a.SendStream(context.Background(), "nonexistent", chunks)
	if err != nil {
		t.Errorf("SendStream 到不存在连接应返回 nil，实际: %v", err)
	}
}

// TestWsMessageJSON 测试 wsMessage JSON 序列化
func TestWsMessageJSON(t *testing.T) {
	tests := []struct {
		name string
		msg  wsMessage
		want map[string]any
	}{
		{
			name: "完整消息",
			msg: wsMessage{
				Type:      "reply",
				Content:   "hello world",
				SessionID: "sess-123",
				Provider:  "智谱",
				Model:     "glm-5",
				Role:      "analyst",
				Done:      true,
			},
			want: map[string]any{
				"type":       "reply",
				"content":    "hello world",
				"session_id": "sess-123",
				"provider":   "智谱",
				"model":      "glm-5",
				"role":       "analyst",
				"done":       true,
			},
		},
		{
			name: "无 session_id 和 done",
			msg: wsMessage{
				Type:    "chunk",
				Content: "partial",
			},
			want: map[string]any{
				"type":    "chunk",
				"content": "partial",
			},
		},
		{
			name: "错误消息",
			msg: wsMessage{
				Type:    "error",
				Content: "something went wrong",
			},
			want: map[string]any{
				"type":    "error",
				"content": "something went wrong",
			},
		},
		{
			name: "带附件消息",
			msg: wsMessage{
				Type:    "message",
				Content: "",
				Attachments: []adapter.Attachment{
					{Type: "image", Mime: "image/png", Data: "abc123"},
				},
			},
			want: map[string]any{
				"type":    "message",
				"content": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatalf("Marshal 失败: %v", err)
			}

			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal 失败: %v", err)
			}

			if got["type"] != tt.want["type"] {
				t.Errorf("type = %v, 期望 %v", got["type"], tt.want["type"])
			}
			if got["content"] != tt.want["content"] {
				t.Errorf("content = %v, 期望 %v", got["content"], tt.want["content"])
			}

			// 检查可选字段
			if wantSID, ok := tt.want["session_id"]; ok {
				if got["session_id"] != wantSID {
					t.Errorf("session_id = %v, 期望 %v", got["session_id"], wantSID)
				}
			} else {
				// session_id 为空时应通过 omitempty 省略
				if sid, exists := got["session_id"]; exists && sid != "" {
					t.Errorf("session_id 应省略，实际: %v", sid)
				}
			}

			if wantDone, ok := tt.want["done"]; ok {
				if got["done"] != wantDone {
					t.Errorf("done = %v, 期望 %v", got["done"], wantDone)
				}
			}
			if wantProvider, ok := tt.want["provider"]; ok && got["provider"] != wantProvider {
				t.Errorf("provider = %v, 期望 %v", got["provider"], wantProvider)
			}
			if wantModel, ok := tt.want["model"]; ok && got["model"] != wantModel {
				t.Errorf("model = %v, 期望 %v", got["model"], wantModel)
			}
			if wantRole, ok := tt.want["role"]; ok && got["role"] != wantRole {
				t.Errorf("role = %v, 期望 %v", got["role"], wantRole)
			}
			if len(tt.msg.Attachments) > 0 {
				raw, ok := got["attachments"].([]any)
				if !ok || len(raw) != len(tt.msg.Attachments) {
					t.Fatalf("attachments 序列化结果不符合预期: %#v", got["attachments"])
				}
			}
		})
	}
}

// TestWsMessageJSONDeserialization 测试 wsMessage JSON 反序列化
func TestWsMessageJSONDeserialization(t *testing.T) {
	input := `{"type":"message","content":"你好","session_id":"sess-abc","provider":"智谱","model":"glm-5","role":"analyst"}`

	var msg wsMessage
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}

	if msg.Type != "message" {
		t.Errorf("Type = %q, 期望 %q", msg.Type, "message")
	}
	if msg.Content != "你好" {
		t.Errorf("Content = %q, 期望 %q", msg.Content, "你好")
	}
	if msg.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, 期望 %q", msg.SessionID, "sess-abc")
	}
	if msg.Provider != "智谱" {
		t.Errorf("Provider = %q, 期望 %q", msg.Provider, "智谱")
	}
	if msg.Model != "glm-5" {
		t.Errorf("Model = %q, 期望 %q", msg.Model, "glm-5")
	}
	if msg.Role != "analyst" {
		t.Errorf("Role = %q, 期望 %q", msg.Role, "analyst")
	}
}

func TestWsMessageJSONDeserializationWithAttachments(t *testing.T) {
	input := `{"type":"message","content":"","attachments":[{"type":"image","mime":"image/png","data":"abc123"}]}`

	var msg wsMessage
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("期望 1 个附件，实际 %d", len(msg.Attachments))
	}
	if msg.Attachments[0].Mime != "image/png" {
		t.Fatalf("附件 MIME 不匹配: %q", msg.Attachments[0].Mime)
	}
}

// TestConnManagement 测试连接管理基本操作
func TestConnManagement(t *testing.T) {
	a := New()

	// 初始状态无连接
	_, ok := a.getConn("id-1")
	if ok {
		t.Error("初始状态不应有连接")
	}

	// getConn 对不存在的 key 返回 nil, false
	conn, ok := a.getConn("id-2")
	if ok || conn != nil {
		t.Error("不存在的连接应返回 nil, false")
	}
}
