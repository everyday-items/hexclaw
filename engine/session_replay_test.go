package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// 做梦回放相「会话选择」逻辑（纯 store、零 LLM）：只选 since 之后有新消息、且含可记信息暗示的会话。
func TestSessionReplay_GatherSelectsRightSessions(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "rp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mk := func(sid, content string, age time.Duration) {
		if err := store.CreateSession(ctx, &storage.Session{ID: sid, UserID: "u1", Platform: "web", Title: sid}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveMessage(ctx, &storage.MessageRecord{
			ID: sid + "-m", SessionID: sid, Role: "user", Content: content, Metadata: "{}", CreatedAt: now.Add(-age),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("s-new-memorable", "我对海鲜过敏，住在上海", 1*time.Hour) // 新 + 可记 → 选
	mk("s-new-chitchat", "今天天气不错哈哈", 1*time.Hour)     // 新 + 无可记暗示 → 跳
	mk("s-old-memorable", "我叫小明是工程师", 72*time.Hour)   // 旧（since 之前）→ 跳

	e := &ReActEngine{}
	e.store = store
	since := now.Add(-24 * time.Hour) // 水位线：24h 前

	convs := e.gatherReplayConversations(ctx, "u1", since, 20)
	got := map[string]bool{}
	for _, c := range convs {
		got[c.sessionID] = true
	}
	if !got["s-new-memorable"] {
		t.Errorf("应回放：新会话含可记信息（海鲜过敏/上海）")
	}
	if got["s-new-chitchat"] {
		t.Errorf("不应回放：闲聊无可记暗示（省 token 快闸）")
	}
	if got["s-old-memorable"] {
		t.Errorf("不应回放：since 水位线之前的旧会话")
	}
}

// 安全：无 store/无 router → ReplayRecentSessions 安全返回 0，不 panic。
func TestSessionReplay_NoStoreSafe(t *testing.T) {
	e := &ReActEngine{}
	if n := e.ReplayRecentSessions(context.Background(), "u1", time.Now(), 10); n != 0 {
		t.Fatalf("无 store 应返回 0，得 %d", n)
	}
}
