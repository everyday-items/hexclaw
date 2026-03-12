package session

import (
	"context"
	"testing"
	"time"

	"github.com/everyday-items/hexclaw/storage"
)

// mockStore 测试用存储
type mockStore struct {
	messages map[string][]*storage.MessageRecord
}

func newMockStore() *mockStore {
	return &mockStore{
		messages: make(map[string][]*storage.MessageRecord),
	}
}

func (s *mockStore) Init(_ context.Context) error   { return nil }
func (s *mockStore) Close() error                    { return nil }
func (s *mockStore) CreateSession(_ context.Context, _ *storage.Session) error { return nil }
func (s *mockStore) GetSession(_ context.Context, _ string) (*storage.Session, error) {
	return nil, storage.ErrNotFound
}
func (s *mockStore) ListSessions(_ context.Context, _ string, _, _ int) ([]*storage.Session, error) {
	return nil, nil
}
func (s *mockStore) DeleteSession(_ context.Context, _ string) error { return nil }

func (s *mockStore) SaveMessage(_ context.Context, msg *storage.MessageRecord) error {
	s.messages[msg.SessionID] = append(s.messages[msg.SessionID], msg)
	return nil
}

func (s *mockStore) DeleteMessage(_ context.Context, id string) error {
	for sid, msgs := range s.messages {
		for i, msg := range msgs {
			if msg.ID == id {
				s.messages[sid] = append(msgs[:i], msgs[i+1:]...)
				return nil
			}
		}
	}
	return nil
}

func (s *mockStore) ListMessages(_ context.Context, sessionID string, limit, _ int) ([]*storage.MessageRecord, error) {
	msgs := s.messages[sessionID]
	if limit > 0 && limit < len(msgs) {
		return msgs[:limit], nil
	}
	return msgs, nil
}

func (s *mockStore) SaveCost(_ context.Context, _ *storage.CostRecord) error  { return nil }
func (s *mockStore) GetUserCost(_ context.Context, _ string, _ time.Time) (float64, error) {
	return 0, nil
}
func (s *mockStore) GetGlobalCost(_ context.Context, _ time.Time) (float64, error) { return 0, nil }
func (s *mockStore) WithTx(_ context.Context, fn func(storage.Store) error) error {
	return fn(s)
}

// TestCompactor_NeedsCompaction 测试压缩判断
func TestCompactor_NeedsCompaction(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()

	// 添加 60 条消息
	for i := 0; i < 60; i++ {
		store.SaveMessage(ctx, &storage.MessageRecord{
			ID:        "msg-" + string(rune('A'+i%26)),
			SessionID: "sess-1",
			Role:      "user",
			Content:   "消息内容",
		})
	}

	compactor := NewCompactor(store, DefaultCompactionConfig())

	needs, err := compactor.NeedsCompaction(ctx, "sess-1")
	if err != nil {
		t.Fatalf("检查失败: %v", err)
	}
	if !needs {
		t.Error("60 条消息应需要压缩（阈值 50）")
	}

	// 少量消息不需要压缩
	needs, err = compactor.NeedsCompaction(ctx, "sess-nonexist")
	if err != nil {
		t.Fatalf("检查失败: %v", err)
	}
	if needs {
		t.Error("空会话不应需要压缩")
	}
}

// TestCompactor_Config 测试配置默认值
func TestCompactor_Config(t *testing.T) {
	c := NewCompactor(newMockStore(), CompactionConfig{})
	if c.config.MaxMessages != 50 {
		t.Errorf("默认 MaxMessages 应为 50，实际 %d", c.config.MaxMessages)
	}
	if c.config.KeepRecent != 10 {
		t.Errorf("默认 KeepRecent 应为 10，实际 %d", c.config.KeepRecent)
	}
}

// TestTruncate 测试文本截断
func TestTruncate(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"短文本", 10, "短文本"},
		{"这是一段比较长的中文文本", 5, "这是一段比..."},
		{"", 5, ""},
		{"ab", 5, "ab"},
	}

	for _, tt := range tests {
		got := truncate(tt.input, tt.maxLen)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
		}
	}
}
