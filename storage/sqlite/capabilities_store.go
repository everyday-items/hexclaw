// capabilities_store.go 实现 A7 模型 tool_call 能力探测的持久化。
//
// 架构：Store 方法签名与 llmrouter.CapabilityStore 接口鸭式类型匹配，
// 无需 storage/sqlite import llmrouter（避免底层存储依赖业务包）。
// main.go 把 *sqlite.Store 直接传给 llmrouter.NewCapabilityService 即可。
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexclaw/llmrouter"
)

// GetCapability 读取单个 (provider, model) 的能力记录。
// 不存在返回 (nil, nil)。
func (s *Store) GetCapability(ctx context.Context, provider, model string) (*llmrouter.Capability, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT provider_name, model_name, tool_call, last_probe, probe_error
		FROM model_capabilities
		WHERE provider_name = ? AND model_name = ?`, provider, model)

	var cap llmrouter.Capability
	var toolCall int
	if err := row.Scan(&cap.ProviderName, &cap.ModelName, &toolCall, &cap.LastProbe, &cap.ProbeError); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("查询 model_capabilities 失败: %w", err)
	}
	cap.ToolCall = llmrouter.ReliabilityLevel(toolCall)
	cap.ToolCallText = cap.ToolCall.String()
	return &cap, nil
}

// UpsertCapability 插入或覆盖能力记录（主键冲突即更新）。
func (s *Store) UpsertCapability(ctx context.Context, cap llmrouter.Capability) error {
	if cap.LastProbe.IsZero() {
		cap.LastProbe = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO model_capabilities
			(provider_name, model_name, tool_call, last_probe, probe_error)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(provider_name, model_name) DO UPDATE SET
			tool_call   = excluded.tool_call,
			last_probe  = excluded.last_probe,
			probe_error = excluded.probe_error`,
		cap.ProviderName, cap.ModelName, int(cap.ToolCall), cap.LastProbe, cap.ProbeError,
	)
	if err != nil {
		return fmt.Errorf("upsert model_capabilities 失败: %w", err)
	}
	return nil
}

// ListCapabilities 列出所有能力记录，按 provider, model 排序。
// 前端按需过滤 LastProbe 判断新鲜度。
func (s *Store) ListCapabilities(ctx context.Context) ([]llmrouter.Capability, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider_name, model_name, tool_call, last_probe, probe_error
		FROM model_capabilities
		ORDER BY provider_name, model_name`)
	if err != nil {
		return nil, fmt.Errorf("查询 model_capabilities 列表失败: %w", err)
	}
	defer rows.Close()

	var caps []llmrouter.Capability
	for rows.Next() {
		var cap llmrouter.Capability
		var toolCall int
		if err := rows.Scan(&cap.ProviderName, &cap.ModelName, &toolCall, &cap.LastProbe, &cap.ProbeError); err != nil {
			return nil, fmt.Errorf("扫描 model_capabilities 行失败: %w", err)
		}
		cap.ToolCall = llmrouter.ReliabilityLevel(toolCall)
		cap.ToolCallText = cap.ToolCall.String()
		caps = append(caps, cap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 model_capabilities 失败: %w", err)
	}
	return caps, nil
}
