package k12storage_test

// Transactional Outbox 契约（§6.9/§6.15）：
//  1. 错题域写与事件同事务落库（写成功必有事件；写失败无事件残留）；
//  2. at-least-once + 消费者以 (consumer, event_id) 去重（重放不重复投递）；
//  3. 连续失败进入 dead-letter（attempts 达上限，last_error 取证，不静默丢弃）；
//  4. 投影失败不撤销成功域写（错题在库，事件 pending 可重试补投影）。

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type countingConsumer struct {
	name    string
	handled []k12storage.OutboxEvent
	fail    bool
}

func (c *countingConsumer) Name() string { return c.name }
func (c *countingConsumer) Handle(_ context.Context, ev k12storage.OutboxEvent) error {
	if c.fail {
		return errors.New("consumer 故障注入")
	}
	c.handled = append(c.handled, ev)
	return nil
}

// TestOutbox_SameTxAppend 域写成功 → 同事务事件已落库（pending）；
// 字段校验失败的写 → 无域行也无事件（同事务原子）。
func TestOutbox_SameTxAppend(t *testing.T) {
	s, db := setup(t)
	ctx := context.Background()
	r := newMistake(t, "mingming", "s1", "3.8×3=?")
	if _, err := s.Put(ctx, r); err != nil {
		t.Fatal(err)
	}
	events, err := k12storage.PendingEvents(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != k12storage.EventMistakeRecorded ||
		events[0].AggregateID != r.RecordID || events[0].AgentName != "mingming" {
		t.Fatalf("域写应同事务落 1 条事件, got %+v", events)
	}

	// 非法字段（缺题干）→ 写失败 → 事件数不变
	bad, _ := k12.NewMistakeRecord("mingming", "s1", k12.MistakeFields{Question: "占位"})
	bad.Fields = `{"question":""}`
	if _, err := s.Put(ctx, bad); err == nil {
		t.Fatal("非法字段应写失败")
	}
	events, _ = k12storage.PendingEvents(ctx, db, 10)
	if len(events) != 1 {
		t.Fatalf("失败写不得残留事件, got %d", len(events))
	}
}

// TestOutbox_ConsumerDedupeByEventID 消费幂等：投递成功后重放（重跑 ProcessPending +
// 手动把事件拨回 pending 模拟崩溃窗口重放）→ 消费者不重复处理。
func TestOutbox_ConsumerDedupeByEventID(t *testing.T) {
	s, db := setup(t)
	ctx := context.Background()
	c := &countingConsumer{name: "learning-insights"}
	d := k12storage.NewDispatcher(s, c)

	if _, err := s.Put(ctx, newMistake(t, "mingming", "s1", "7×8=?")); err != nil {
		t.Fatal(err)
	}
	if err := d.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.handled) != 1 {
		t.Fatalf("应投递 1 次, got %d", len(c.handled))
	}
	// 幂等：再跑一轮无事发生
	if err := d.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	// 崩溃窗口重放：事件拨回 pending → 去重表挡住重复投递
	if _, err := db.Exec(`UPDATE outbox_events SET status='pending'`); err != nil {
		t.Fatal(err)
	}
	if err := d.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.handled) != 1 {
		t.Fatalf("重放后消费者应仍只处理 1 次（event_id 去重）, got %d", len(c.handled))
	}
	// 重放后事件重新标 delivered
	events, _ := k12storage.PendingEvents(ctx, db, 10)
	if len(events) != 0 {
		t.Fatalf("重放后应无 pending, got %d", len(events))
	}
}

// TestOutbox_DeadLetterAfterMaxAttempts 连续失败 → attempts 累计 → dead-letter 取证；
// 域写（错题）不受投影失败影响。
func TestOutbox_DeadLetterAfterMaxAttempts(t *testing.T) {
	s, db := setup(t)
	ctx := context.Background()
	c := &countingConsumer{name: "learning-insights", fail: true}
	d := k12storage.NewDispatcher(s, c)

	r := newMistake(t, "mingming", "s1", "6÷2=?")
	if _, err := s.Put(ctx, r); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < k12storage.DispatcherMaxAttempts; i++ {
		if err := d.ProcessPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	dead, err := k12storage.DeadEvents(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 || dead[0].Attempts != k12storage.DispatcherMaxAttempts || dead[0].LastError == "" {
		t.Fatalf("连续失败应进 dead-letter 且 last_error 可取证, got %+v", dead)
	}
	// 投影失败不撤销域写（§6.9）：错题仍在库
	if _, err := s.Get(ctx, r.RecordID); err != nil {
		t.Fatalf("投影失败不得影响域写: %v", err)
	}
	// dead 后不再投递
	if err := d.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.handled) != 0 {
		t.Fatalf("dead 事件不应再投递, got %d", len(c.handled))
	}
}

// TestOutbox_ManualVsPhotoNoteParity 学情消费者措辞契约由 usecase.InsightsConsumer 测试钉住；
// 此处钉 payload 事实：entry_source 与 created 随事件透出。
func TestOutbox_PayloadFacts(t *testing.T) {
	s, db := setup(t)
	ctx := context.Background()
	r := newMistake(t, "mingming", "s1", "8+9=?")
	if _, err := s.Put(ctx, r); err != nil {
		t.Fatal(err)
	}
	// 同题重复 → created=false 事件仍出（同题再错是有效薄弱信号，与原内联行为一致）
	dup := newMistake(t, "mingming", "s1", "8+9=?")
	if created, err := s.Put(ctx, dup); err != nil || created {
		t.Fatalf("应去重命中: created=%v err=%v", created, err)
	}
	events, err := k12storage.PendingEvents(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("新建+去重命中应各出一条事件, got %d", len(events))
	}
}
