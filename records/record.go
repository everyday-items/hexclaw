// Package records 提供领域中性的「记录本原语」（agent_records）。
//
// 设计目标（架构设计和执行计划-v0.5.0.md §5.2.3 / §6.4-6.5）：
//   - 平台原语，任何场景包共用；平台**不认识**任何业务概念（领域字段一律进 Fields）；
//   - 通用基建列 typed（record_id/agent_name/collection/schema_version/status/dedupe_key/
//     tags/due_at/version/时间戳），领域字段进 Fields（JSON），保持领域中性；
//   - 隔离键 = AgentName（对应 agents.name = 不可变 PK；display_name 才是可变显示名）；
//   - 场景包通过 RecordSchemaRegistry 声明记录集（状态机 / 去重键 / 字段校验），
//     records 包本身零业务条件（禁止出现 if collection=="某业务记录集"）。
//
// 各场景包的记录集是注册进来的 RecordSchema，不在本包内硬编码（AP-1 回归锁：本包领域词趋零）。
package records

import (
	"errors"
	"time"
)

// ErrNotFound 记录不存在。
var ErrNotFound = errors.New("record not found")

// ErrVersionConflict 乐观并发锁冲突（updated 时 version 已被他人推进）。
var ErrVersionConflict = errors.New("record version conflict")

// ErrUnknownCollection 未注册的记录集。
var ErrUnknownCollection = errors.New("unknown record collection")

// ErrInvalidStatus 状态不在记录集状态机声明内。
var ErrInvalidStatus = errors.New("invalid status for collection")

// ErrIllegalTransition 状态转移不在记录集声明的转移偏序内（如倒退 / 离开终态）。
var ErrIllegalTransition = errors.New("illegal status transition for collection")

// AgentRecord 一条通用记录（agent_records 表的行映射）。
//
// 字段分两层：
//   - 通用基建列（本结构体的具名字段）：平台负责，typed；
//   - 领域字段：装进 Fields（JSON 字符串），由记录集 schema 声明与校验，平台不 typed。
type AgentRecord struct {
	RecordID      string `json:"record_id"`
	AgentName     string `json:"agent_name"`     // 归属实例（agents.name = 不可变隔离键）
	Collection    string `json:"collection"`     // 记录集（由场景包的 schema 声明）
	SchemaVersion int    `json:"schema_version"` // 写入时的记录集 schema 版本
	Status        string `json:"status"`         // 记录集状态机当前态
	Fields        string `json:"fields"`         // 领域字段载体（JSON），平台不解析
	DedupeKey     string `json:"dedupe_key"`     // 同实例+记录集内去重键（幂等写）
	Tags          string `json:"tags"`           // 横切标签（JSON 数组）
	DueAt         *int64 `json:"due_at"`         // 到期时点（unix 秒）；无到期调度语义为 nil
	SourceSession string `json:"source_session"` // 来源会话（回跳入口）
	Version       int    `json:"version"`        // 乐观并发锁
	CreatedAt     int64  `json:"created_at"`     // unix 秒
	UpdatedAt     int64  `json:"updated_at"`     // unix 秒
}

// RecordSchema 记录集声明（由场景包提供，注册进 RecordSchemaRegistry）。
//
// 它描述一个记录集的**通用规则**——状态机、去重键、字段校验——而不含任何平台侧业务硬编码。
type RecordSchema struct {
	// Collection 记录集名（全局唯一，由场景包声明）。
	Collection string
	// Version schema 版本，随字段/状态机演进递增。
	Version int
	// InitialStatus 新记录默认落入的状态（状态机首态）。
	InitialStatus string
	// Statuses 合法状态全集（状态机节点）。写入/流转的 status 必须 ∈ 此集合。
	Statuses []string
	// Transitions 状态转移偏序（from → 允许的 to 集合）。声明后 UpdateStatus 强制校验：
	// 只允许 to==from（幂等）或 to ∈ Transitions[from]，否则 ErrIllegalTransition（挡倒退/离开终态）。
	// 为 nil 时不校验转移（向后兼容未声明转移的记录集，如积累本）。终态在 map 里给空 slice。
	Transitions map[string][]string
	// DedupeKey 从一条待写记录派生去重键（同实例+记录集内唯一）。
	// 由场景包定义"什么算同一条"（如：某业务摘要 + 来源会话）。
	DedupeKey func(r *AgentRecord) string
	// ValidateFields 校验领域字段（Fields JSON）。可为 nil（不校验）。
	ValidateFields func(fieldsJSON string) error
}

// hasStatus 判断 status 是否在状态机声明内。
func (s *RecordSchema) hasStatus(status string) bool {
	for _, v := range s.Statuses {
		if v == status {
			return true
		}
	}
	return false
}

// canTransition 判断 from→to 是否合法。未声明 Transitions 时一律放行（向后兼容）；
// 声明后：同态（幂等）放行，否则须 to ∈ Transitions[from]。
func (s *RecordSchema) canTransition(from, to string) bool {
	if len(s.Transitions) == 0 || from == to {
		return true
	}
	for _, allowed := range s.Transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// nowUnix 返回当前 unix 秒（抽出便于测试注入）。
var nowUnix = func() int64 { return time.Now().Unix() }
