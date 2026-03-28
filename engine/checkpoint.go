package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// CheckpointData 检查点数据
type CheckpointData struct {
	SessionID string          `json:"session_id"`
	AgentName string          `json:"agent_name,omitempty"`
	Turn      int             `json:"turn"`
	Messages  json.RawMessage `json:"messages"`
	Budget    json.RawMessage `json:"budget,omitempty"`
}

// CheckpointManager 检查点管理器
//
// 每 N 轮自动保存，支持从中断处恢复。
// 对标 LangGraph Checkpoint，OpenClaw 无此能力。
type CheckpointManager struct {
	store    storage.Store
	interval int // 每 N 轮保存一次，默认 5
}

// NewCheckpointManager 创建检查点管理器
func NewCheckpointManager(store storage.Store, interval int) *CheckpointManager {
	if interval <= 0 {
		interval = 5
	}
	return &CheckpointManager{store: store, interval: interval}
}

// ShouldSave 是否应该在当前轮次保存
func (cm *CheckpointManager) ShouldSave(turn int) bool {
	return turn > 0 && turn%cm.interval == 0
}

// Save 保存检查点
func (cm *CheckpointManager) Save(ctx context.Context, data CheckpointData) (string, error) {
	id := "cp-" + idgen.ShortID()
	jsonData, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal checkpoint: %w", err)
	}

	record := &storage.MessageRecord{
		ID:        id,
		SessionID: data.SessionID,
		Role:      "checkpoint",
		Content:   string(jsonData),
		CreatedAt: time.Now(),
	}

	if err := cm.store.SaveMessage(ctx, record); err != nil {
		return "", fmt.Errorf("save checkpoint: %w", err)
	}

	log.Printf("检查点已保存: id=%s session=%s turn=%d", id, data.SessionID, data.Turn)
	return id, nil
}

// Load 加载最新检查点
func (cm *CheckpointManager) Load(ctx context.Context, sessionID string) (*CheckpointData, error) {
	// 查找最近的 checkpoint 消息
	msgs, err := cm.store.ListMessages(ctx, sessionID, 100, 0)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	// 从后向前找第一个 checkpoint
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "checkpoint" {
			var data CheckpointData
			if err := json.Unmarshal([]byte(msgs[i].Content), &data); err != nil {
				continue
			}
			return &data, nil
		}
	}

	return nil, fmt.Errorf("no checkpoint found for session %s", sessionID)
}
