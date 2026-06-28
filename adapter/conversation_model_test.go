package adapter

import (
	"sync"
	"testing"
)

func TestConversationModelStore_SetGetClear(t *testing.T) {
	s := NewConversationModelStore()
	k := ConversationKey{Platform: "feishu", InstanceID: "feishu-support", ChatID: "oc_123"}

	if _, ok := s.Get(k); ok {
		t.Fatalf("空存储不应有覆盖")
	}

	s.Set(k, ModelOverride{Provider: "openai", Model: "gpt-4o"})
	ov, ok := s.Get(k)
	if !ok || ov.Provider != "openai" || ov.Model != "gpt-4o" {
		t.Fatalf("Get 后应得到覆盖, got=%+v ok=%v", ov, ok)
	}

	// 不同对话互不影响（群聊隔离）。
	other := ConversationKey{Platform: "feishu", InstanceID: "feishu-support", ChatID: "oc_999"}
	if _, ok := s.Get(other); ok {
		t.Fatalf("另一对话不应受影响")
	}

	s.Clear(k)
	if _, ok := s.Get(k); ok {
		t.Fatalf("Clear 后不应再有覆盖")
	}
	s.Clear(k) // 重复 clear 应为 no-op，不 panic
}

func TestConversationModelStore_Concurrent(t *testing.T) {
	s := NewConversationModelStore()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			k := ConversationKey{Platform: "tg", ChatID: string(rune('a' + n%26))}
			s.Set(k, ModelOverride{Model: "m"})
			_, _ = s.Get(k)
			if n%3 == 0 {
				s.Clear(k)
			}
		}(i)
	}
	wg.Wait()
}
