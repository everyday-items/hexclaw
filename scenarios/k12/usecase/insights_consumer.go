package usecase

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// InsightsConsumer 学情信号 Outbox 消费者（§6.9/§6.11「学情刷新：消费领域事件并幂等重算」）。
//
// 它接替此前用例内联的 WriteWeakness：错题域写与 k12.mistake.recorded 事件同事务落库，
// 本消费者按 payload 还原两条录入路径的既有措辞——
//   - photo（批改判错，含老记录空 entry_source）：「在「kp」出错：错因」，同题再错（created=false）
//     仍写信号（重复出错本身是有效的薄弱证据，与原 GradeHomeworkProblem 行为一致）；
//   - manual（家长手动记入）：「在「kp」出错（家长手动记入）：错因」，仅新建时写
//     （与原 RecordMistake 行为一致）。
//
// 幂等：Dispatcher 以 (consumer, event_id) 去重；本消费者对同一事件重放安全
// （WriteWeakness 是画像追加信号，去重表挡住常规重复，崩溃窗口内 at-least-once 可接受）。
type InsightsConsumer struct {
	Insights Insights
}

// Name 消费者标识（outbox_consumptions 去重键的一半）。
func (c InsightsConsumer) Name() string { return "learning-insights" }

// Handle 消费一条事件。未接线 Insights 时静默跳过（与原 d.Insights != nil 判定同语义）。
func (c InsightsConsumer) Handle(ctx context.Context, ev k12storage.OutboxEvent) error {
	if ev.EventType != k12storage.EventMistakeRecorded || c.Insights == nil {
		return nil
	}
	var p k12storage.MistakeRecordedPayload
	if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
		return fmt.Errorf("学情消费者: 解析 payload: %w", err)
	}
	if p.KnowledgePoint == "" {
		return nil // 与原语义一致：无知识点不写薄弱信号
	}
	var note string
	if p.EntrySource == k12.MistakeEntryManual {
		if !p.Created {
			return nil // 手动路径幂等去重命中不重复写信号（原语义）
		}
		note = fmt.Sprintf("在「%s」出错（家长手动记入）：%s", p.KnowledgePoint, p.ErrorCause)
	} else {
		note = fmt.Sprintf("在「%s」出错：%s", p.KnowledgePoint, p.ErrorCause)
	}
	return c.Insights.WriteWeakness(ctx, p.AgentName, p.KnowledgePoint, note)
}
