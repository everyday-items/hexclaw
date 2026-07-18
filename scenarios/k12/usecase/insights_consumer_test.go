package usecase

// InsightsConsumer 契约：学情信号经 Outbox 消费后与原内联 WriteWeakness 行为逐字等价——
// photo 路径措辞「在「kp」出错：…」且去重命中仍写；manual 路径措辞带「（家长手动记入）」
// 且仅新建时写；无知识点不写。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func mistakeEvent(t *testing.T, p k12storage.MistakeRecordedPayload) k12storage.OutboxEvent {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return k12storage.OutboxEvent{
		EventID: "ev-1", EventType: k12storage.EventMistakeRecorded,
		AgentName: p.AgentName, AggregateID: p.RecordID, Payload: string(raw),
	}
}

func TestInsightsConsumer_PhotoWordingAndDupStillWrites(t *testing.T) {
	ins := &fakeInsights{}
	c := InsightsConsumer{Insights: ins}
	// created=false（同题再错）也写——与原 GradeHomeworkProblem 内联行为一致
	ev := mistakeEvent(t, k12storage.MistakeRecordedPayload{
		RecordID: "m-1", AgentName: "mingming", Created: false,
		KnowledgePoint: "小数乘法", ErrorCause: "计算失误", EntrySource: k12.MistakeEntryPhoto,
	})
	if err := c.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(ins.notes) != 1 || ins.notes[0] != "在「小数乘法」出错：计算失误" {
		t.Fatalf("photo 措辞应逐字保持, got %v", ins.notes)
	}
}

func TestInsightsConsumer_ManualWordingAndCreatedGate(t *testing.T) {
	ins := &fakeInsights{}
	c := InsightsConsumer{Insights: ins}
	// manual + created=false → 不写（与原 RecordMistake 仅新建写信号一致）
	dup := mistakeEvent(t, k12storage.MistakeRecordedPayload{
		RecordID: "m-2", AgentName: "mingming", Created: false,
		KnowledgePoint: "进位加法", ErrorCause: "粗心", EntrySource: k12.MistakeEntryManual,
	})
	if err := c.Handle(context.Background(), dup); err != nil {
		t.Fatal(err)
	}
	if len(ins.notes) != 0 {
		t.Fatalf("manual 去重命中不应写信号, got %v", ins.notes)
	}
	fresh := mistakeEvent(t, k12storage.MistakeRecordedPayload{
		RecordID: "m-2", AgentName: "mingming", Created: true,
		KnowledgePoint: "进位加法", ErrorCause: "粗心", EntrySource: k12.MistakeEntryManual,
	})
	if err := c.Handle(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if len(ins.notes) != 1 || ins.notes[0] != "在「进位加法」出错（家长手动记入）：粗心" {
		t.Fatalf("manual 措辞应逐字保持, got %v", ins.notes)
	}
}

func TestInsightsConsumer_NoKnowledgePointSkips(t *testing.T) {
	ins := &fakeInsights{}
	c := InsightsConsumer{Insights: ins}
	ev := mistakeEvent(t, k12storage.MistakeRecordedPayload{
		RecordID: "m-3", AgentName: "mingming", Created: true,
		ErrorCause: "看错题", EntrySource: k12.MistakeEntryPhoto,
	})
	if err := c.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(ins.notes) != 0 {
		t.Fatalf("无知识点不写薄弱信号（原语义）, got %v", ins.notes)
	}
}
