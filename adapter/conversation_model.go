package adapter

import "sync"

// ConversationKey 唯一标识一个「对话」，用于 /model 逐对话模型覆盖。
//
// 群聊与私聊都按 (platform, instance, chat) 维度归并：群聊里 chat 是群 ID（一个群一份
// 覆盖），私聊里 chat 通常等于对端用户。是否**允许**某用户在群里改模型是 intake 层的
// 权限策略（见集成补丁），不是本存储的职责——本存储只做 key→override 的并发安全映射。
type ConversationKey struct {
	Platform   string
	InstanceID string
	ChatID     string
}

// ModelOverride 是一次 /model 切换的结果。Provider 可空（仅指定 model 时由上层按模型归属解析）。
type ModelOverride struct {
	Provider string
	Model    string
}

// ConversationModelStore 保存各对话的 /model 覆盖。进程内、并发安全。
//
// 覆盖是「临时会话偏好」而非持久配置，重启清空即可——符合 /model 即时切换语义
// （对标 Hermes：/model 切换当前对话，不写盘、不影响通道默认绑定）。
type ConversationModelStore struct {
	mu sync.RWMutex
	m  map[ConversationKey]ModelOverride
}

// NewConversationModelStore 创建空存储。
func NewConversationModelStore() *ConversationModelStore {
	return &ConversationModelStore{m: make(map[ConversationKey]ModelOverride)}
}

// Set 设置某对话的模型覆盖。
func (s *ConversationModelStore) Set(key ConversationKey, ov ModelOverride) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = ov
}

// Get 返回某对话的模型覆盖；ok=false 表示无覆盖（应回退到优先级链的下一级）。
func (s *ConversationModelStore) Get(key ConversationKey) (ModelOverride, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ov, ok := s.m[key]
	return ov, ok
}

// Clear 清除某对话的模型覆盖（/model reset）。无覆盖时为 no-op。
func (s *ConversationModelStore) Clear(key ConversationKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}
