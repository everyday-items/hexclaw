package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// 增量 G②：ActiveRecall 回复前主动会话深召回。这些测试钉死：浮现相关 / 坑F 去重 / 跳当前 query /
// 有界 TopK / 空安全 / 超时熔断（含半开）/ buildTurnContext 资格门控（仅 DM）+ 围栏。

type fakeSearcher struct {
	results []*storage.SearchResult
	err     error
	calls   int
}

func (f *fakeSearcher) SearchMessages(_ context.Context, _, _ string, _, _ int) ([]*storage.SearchResult, int, error) {
	f.calls++
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.results, len(f.results), nil
}

func msgResult(title, content string) *storage.SearchResult {
	return &storage.SearchResult{
		SessionTitle: title,
		Message:      &storage.MessageRecord{Role: "user", Content: content, CreatedAt: time.Now()},
	}
}

func TestActiveRecall_SurfacesRelevant(t *testing.T) {
	f := &fakeSearcher{results: []*storage.SearchResult{
		msgResult("部署讨论", "上次我们用 blue-green 部署上线"),
		msgResult("架构", "数据库选了 Postgres 主从"),
	}}
	ar := NewActiveRecall(f)
	out := ar.Prefetch(context.Background(), "u1", "部署方案", "", "")
	if !strings.Contains(out, "blue-green") || !strings.Contains(out, "Postgres") {
		t.Fatalf("应浮现两条历史片段:\n%s", out)
	}
	if !strings.Contains(out, "部署讨论") {
		t.Fatalf("应带会话标题:\n%s", out)
	}
}

// 坑F：已在注入的策展记忆里覆盖的片段不重复浮现。
func TestActiveRecall_DedupAgainstInjected(t *testing.T) {
	f := &fakeSearcher{results: []*storage.SearchResult{
		msgResult("会话A", "用户对花生过敏需要注意"),
		msgResult("会话B", "用户上次提到喜欢爬山徒步"),
	}}
	ar := NewActiveRecall(f)
	injected := "## 长期记忆\n\n- 用户对花生过敏需要注意"
	out := ar.Prefetch(context.Background(), "u1", "过敏 爬山", injected, "")
	if strings.Contains(out, "花生过敏") {
		t.Fatalf("已注入策展事实不应再被主动召回重复浮现（坑F）:\n%s", out)
	}
	if !strings.Contains(out, "爬山") {
		t.Fatalf("未覆盖的片段应正常浮现:\n%s", out)
	}
}

func TestActiveRecall_SkipsCurrentQueryAndShort(t *testing.T) {
	f := &fakeSearcher{results: []*storage.SearchResult{
		msgResult("s", "部署方案"), // 与 query 完全相同 → 跳
		msgResult("s", "嗯"),    // 过短 → 跳
		msgResult("s", "上次聊的容器编排细节"),
	}}
	ar := NewActiveRecall(f)
	out := ar.Prefetch(context.Background(), "u1", "部署方案", "", "")
	if strings.Count(out, "\n")+1 != 1 || !strings.Contains(out, "容器编排") {
		t.Fatalf("应只剩 1 条有效片段:\n%q", out)
	}
}

func TestActiveRecall_TopKBounded(t *testing.T) {
	var rs []*storage.SearchResult
	for _, c := range []string{"片段甲内容长一点", "片段乙内容长一点", "片段丙内容长一点", "片段丁内容长一点", "片段戊内容长一点"} {
		rs = append(rs, msgResult("s", c))
	}
	ar := NewActiveRecall(&fakeSearcher{results: rs})
	out := ar.Prefetch(context.Background(), "u1", "片段", "", "")
	if n := strings.Count(out, "\n") + 1; n > activeRecallTopK {
		t.Fatalf("应有界 TopK=%d，实际 %d 条:\n%s", activeRecallTopK, n, out)
	}
}

func TestActiveRecall_EmptyCases(t *testing.T) {
	if got := (*ActiveRecall)(nil).Prefetch(context.Background(), "u", "q", "", ""); got != "" {
		t.Fatal("nil 接收者应空")
	}
	if got := NewActiveRecall(nil).Prefetch(context.Background(), "u", "q", "", ""); got != "" {
		t.Fatal("nil store 应空")
	}
	if got := NewActiveRecall(&fakeSearcher{}).Prefetch(context.Background(), "u", "  ", "", ""); got != "" {
		t.Fatal("空 query 应空")
	}
	if got := NewActiveRecall(&fakeSearcher{}).Prefetch(context.Background(), "u", "q", "", ""); got != "" {
		t.Fatal("无结果应空")
	}
}

// 超时/失败 → 熔断：连续失败达阈值后停止打底层、返回空；冷却后半开重试。
func TestActiveRecall_CircuitBreaker(t *testing.T) {
	f := &fakeSearcher{err: errors.New("boom")}
	ar := NewActiveRecall(f)
	cur := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	ar.now = func() time.Time { return cur }

	for i := 0; i < breakerFailThreshold; i++ {
		if got := ar.Prefetch(context.Background(), "u", "q", "", ""); got != "" {
			t.Fatalf("失败应返回空")
		}
	}
	if f.calls != breakerFailThreshold {
		t.Fatalf("阈值前应打满 %d 次，实际 %d", breakerFailThreshold, f.calls)
	}
	// 熔断打开：不再打底层。
	ar.Prefetch(context.Background(), "u", "q", "", "")
	if f.calls != breakerFailThreshold {
		t.Fatalf("熔断打开后不应再调底层，calls=%d", f.calls)
	}
	// 冷却结束 → 半开：重试一次（仍 err → 又失败）。
	cur = cur.Add(breakerCooldown + time.Second)
	ar.Prefetch(context.Background(), "u", "q", "", "")
	if f.calls != breakerFailThreshold+1 {
		t.Fatalf("冷却后应半开重试一次，calls=%d", f.calls)
	}
}

// buildTurnContext 资格门控：仅 DM/交互式注入 <recalled-context>；系统派发(heartbeat 等)/memory=off 不注入。
func TestActiveRecall_EligibilityAndFence(t *testing.T) {
	fm := newFileMem(t, 200)
	e := &ReActEngine{}
	e.SetFileMemory(fm)
	e.SetActiveRecall(NewActiveRecall(&fakeSearcher{results: []*storage.SearchResult{
		msgResult("旧会话", "上次部署用的是金丝雀发布策略"),
	}}))

	// DM（无系统派发）→ 注入。
	dm := e.buildTurnContext(context.Background(), map[string]string{}, "", "部署策略")
	if !strings.Contains(dm, "<recalled-context>") || !strings.Contains(dm, "金丝雀") {
		t.Fatalf("DM 应主动召回并围栏注入:\n%s", dm)
	}
	// 围栏闭合标签存在。
	if !strings.Contains(dm, "</recalled-context>") {
		t.Fatalf("应有闭合围栏:\n%s", dm)
	}

	// 系统派发（heartbeat）→ 不注入。
	hbCtx := skill.WithSystemDispatchSource(context.Background(), "heartbeat")
	hb := e.buildTurnContext(hbCtx, map[string]string{}, "", "部署策略")
	if strings.Contains(hb, "recalled-context") {
		t.Fatalf("系统派发不应跑主动召回:\n%s", hb)
	}

	// memory=off → 不注入。
	off := e.buildTurnContext(context.Background(), map[string]string{"memory": "off"}, "", "部署策略")
	if strings.Contains(off, "recalled-context") {
		t.Fatalf("memory=off 应门掉主动召回:\n%s", off)
	}
}

// 真实 sqlite store 端到端：种入历史会话消息 → buildTurnContext 经真实 FTS 主动召回浮现（零 LLM）。
func TestActiveRecall_RealStoreEndToEnd(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "ar.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := store.CreateSession(ctx, &storage.Session{ID: "s1", UserID: "u1", Platform: "web", Title: "上次的部署讨论"}); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "m1", SessionID: "s1", Role: "user", Content: "我们上次用蓝绿部署上线了新版本", Metadata: "{}",
	}); err != nil {
		t.Fatalf("msg: %v", err)
	}

	e := &ReActEngine{}
	e.SetFileMemory(newFileMem(t, 200))
	e.SetActiveRecall(NewActiveRecall(store))

	// DM ctx 带鉴权用户 u1（与消息同 user，防越权）。
	turn := e.buildTurnContext(skill.WithAuthenticatedUser(ctx, "u1"), map[string]string{}, "", "部署")
	if !strings.Contains(turn, "<recalled-context>") || !strings.Contains(turn, "蓝绿部署") {
		t.Fatalf("真实 store FTS 应主动召回历史「蓝绿部署」:\n%s", turn)
	}

	// 越权隔离：别的用户搜不到 u1 的历史。
	other := e.buildTurnContext(skill.WithAuthenticatedUser(ctx, "u2"), map[string]string{}, "", "部署")
	if strings.Contains(other, "蓝绿部署") {
		t.Fatalf("跨用户不应召回他人历史（越权）:\n%s", other)
	}
}
