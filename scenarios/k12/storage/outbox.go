package k12storage

// Transactional Outbox（架构设计-v0.5.0.md §6.9 / §5.6 OutboxEvent / §6.15 Outbox 消费）：
//   - 领域写与事件在同一 SQLite 事务提交（禁止拆库）；
//   - 单进程 Dispatcher 顺序消费，at-least-once；
//   - 每个消费者以 (consumer, event_id) 去重（outbox_consumptions），支持重放；
//   - 连续失败进入 dead-letter（status=dead + last_error 取证），不静默丢弃。
//
// 本批事件清单（申报：文档未给具体清单，按 §6.11「学情刷新：消费领域事件并幂等重算」
// 选定学情信号事件）：
//   - k12.mistake.recorded —— 错题入库（含去重命中）：payload 携带学科/知识点/错因/录入来源，
//     学情消费者据此写薄弱点信号（原用例内联 WriteWeakness 迁移至此，投影失败不撤销域写）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 事件类型。
const (
	EventMistakeRecorded            = "k12.mistake.recorded"
	EventGradingAssessmentCommitted = "k12.grading.assessment.committed"
)

// Outbox 事件状态。
const (
	OutboxPending   = "pending"
	OutboxDelivered = "delivered"
	OutboxDead      = "dead"
)

// OutboxEvent 一条领域事件（outbox_events 行映射，§5.6）。
type OutboxEvent struct {
	EventID        string `json:"event_id"`
	AgentName      string `json:"agent_name"`
	AggregateID    string `json:"aggregate_id"`
	EventType      string `json:"event_type"`
	PayloadVersion int    `json:"payload_version"`
	Payload        string `json:"payload_json"`
	Status         string `json:"status"`
	Attempts       int    `json:"attempts"`
	LastError      string `json:"last_error"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// MistakeRecordedPayload k12.mistake.recorded 的 payload v1。
type MistakeRecordedPayload struct {
	RecordID       string `json:"record_id"`
	AgentName      string `json:"agent_name"`
	Created        bool   `json:"created"` // false = 去重命中（同题再错，仍是有效的薄弱信号）
	Subject        string `json:"subject,omitempty"`
	KnowledgePoint string `json:"knowledge_point,omitempty"`
	ErrorCause     string `json:"error_cause,omitempty"`
	EntrySource    string `json:"entry_source,omitempty"` // photo/manual…（§5.5）
}

// GradingAssessmentCommittedPayload is deliberately metadata-only. Model
// output remains in the local receipt; the Outbox never duplicates potentially
// sensitive model text.
type GradingAssessmentCommittedPayload struct {
	AgentName    string `json:"agent_name"`
	JobID        string `json:"job_id"`
	ProblemID    string `json:"problem_id"`
	AttemptID    string `json:"attempt_id"`
	Status       string `json:"status"`
	ResultDigest string `json:"result_digest"`
}

// appendDomainEvents 在领域写事务内追加 Outbox 事件（§6.9 同事务提交）。
// 返回是否有事件入队（供提交后通知 Dispatcher）。
func appendDomainEvents(ctx context.Context, ex dbHandle, r *records.AgentRecord, created bool) (bool, error) {
	if r.Collection != k12.CollectionMistakes {
		return false, nil
	}
	return appendMistakeRecordedEvent(ctx, ex, r, created, idgen.NanoID())
}

func appendMistakeRecordedEvent(ctx context.Context, ex dbHandle, r *records.AgentRecord,
	created bool, eventID string,
) (bool, error) {
	f, err := k12.ParseMistakeFields(r.Fields)
	if err != nil {
		return false, fmt.Errorf("k12storage: 解析错题字段(事件): %w", err)
	}
	payload, err := json.Marshal(MistakeRecordedPayload{
		RecordID:       r.RecordID,
		AgentName:      r.AgentName,
		Created:        created,
		Subject:        f.Subject,
		KnowledgePoint: f.KnowledgePoint,
		ErrorCause:     f.ErrorCause,
		EntrySource:    f.EntrySource,
	})
	if err != nil {
		return false, fmt.Errorf("k12storage: marshal 事件 payload: %w", err)
	}
	return appendOutboxEvent(ctx, ex, OutboxEvent{
		EventID: eventID, AgentName: r.AgentName, AggregateID: r.RecordID,
		EventType: EventMistakeRecorded, PayloadVersion: 1, Payload: string(payload),
	})
}

func appendOutboxEvent(ctx context.Context, ex dbHandle, event OutboxEvent) (bool, error) {
	now := nowUnix()
	res, err := ex.ExecContext(ctx, `INSERT INTO outbox_events
        (event_id, agent_name, aggregate_id, event_type, payload_version, payload_json,
         status, attempts, last_error, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)
		ON CONFLICT(event_id) DO NOTHING`, event.EventID, event.AgentName, event.AggregateID,
		event.EventType, event.PayloadVersion, event.Payload, OutboxPending, now, now)
	if err != nil {
		return false, fmt.Errorf("k12storage: 写 outbox 事件: %w", err)
	}
	if inserted, _ := res.RowsAffected(); inserted > 0 {
		return true, nil
	}
	var stored OutboxEvent
	if err := ex.QueryRowContext(ctx, `SELECT agent_name,aggregate_id,event_type,payload_version,payload_json
		FROM outbox_events WHERE event_id=?`, event.EventID).Scan(&stored.AgentName, &stored.AggregateID,
		&stored.EventType, &stored.PayloadVersion, &stored.Payload); err != nil {
		return false, fmt.Errorf("k12storage: 回读 deterministic outbox 事件: %w", err)
	}
	if stored.AgentName != event.AgentName || stored.AggregateID != event.AggregateID ||
		stored.EventType != event.EventType || stored.PayloadVersion != event.PayloadVersion ||
		stored.Payload != event.Payload {
		return false, fmt.Errorf("k12storage: deterministic outbox event %q payload conflict", event.EventID)
	}
	return false, nil
}

// PendingEvents 按序取 pending 事件（Dispatcher / 取证用）。
func PendingEvents(ctx context.Context, db *sql.DB, limit int) ([]OutboxEvent, error) {
	rows, err := db.QueryContext(ctx, `SELECT event_id, agent_name, aggregate_id, event_type,
        payload_version, payload_json, status, attempts, last_error, created_at, updated_at
        FROM outbox_events WHERE status = ? ORDER BY created_at, event_id LIMIT ?`,
		OutboxPending, limit)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 读 outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var ev OutboxEvent
		if err := rows.Scan(&ev.EventID, &ev.AgentName, &ev.AggregateID, &ev.EventType,
			&ev.PayloadVersion, &ev.Payload, &ev.Status, &ev.Attempts, &ev.LastError,
			&ev.CreatedAt, &ev.UpdatedAt); err != nil {
			return nil, fmt.Errorf("k12storage: 扫描 outbox 事件: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// DeadEvents dead-letter 取证（§6.15：可在 UI/日志中取证，不静默丢弃）。
func DeadEvents(ctx context.Context, db *sql.DB, limit int) ([]OutboxEvent, error) {
	rows, err := db.QueryContext(ctx, `SELECT event_id, agent_name, aggregate_id, event_type,
        payload_version, payload_json, status, attempts, last_error, created_at, updated_at
        FROM outbox_events WHERE status = ? ORDER BY created_at, event_id LIMIT ?`,
		OutboxDead, limit)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 读 dead-letter: %w", err)
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var ev OutboxEvent
		if err := rows.Scan(&ev.EventID, &ev.AgentName, &ev.AggregateID, &ev.EventType,
			&ev.PayloadVersion, &ev.Payload, &ev.Status, &ev.Attempts, &ev.LastError,
			&ev.CreatedAt, &ev.UpdatedAt); err != nil {
			return nil, fmt.Errorf("k12storage: 扫描 dead-letter: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
