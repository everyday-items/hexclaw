package records

import (
	"fmt"
	"sync"
)

// RecordSchemaRegistry 记录集 schema 注册表（§6.4 六注入缝之一）。
//
// 场景包在启动装配期把自己的记录集 schema 注册进来；records 存储层按 collection
// 查表拿到状态机/去重规则/字段校验。平台侧**零业务条件**——它不认识任何业务记录集，
// 只认识「某个被注册进来的 collection」。这是 AP-1「场景可卸」的关键缝：
// 删掉场景包后，没有 schema 注册，records 依然是一个干净的通用记录系统。
type RecordSchemaRegistry struct {
	mu      sync.RWMutex
	schemas map[string]*RecordSchema
}

// NewRecordSchemaRegistry 创建空注册表。
func NewRecordSchemaRegistry() *RecordSchemaRegistry {
	return &RecordSchemaRegistry{schemas: make(map[string]*RecordSchema)}
}

// Register 注册一个记录集 schema。
//
// 校验：Collection 非空、不重复；InitialStatus 必须 ∈ Statuses。
func (r *RecordSchemaRegistry) Register(s *RecordSchema) error {
	if s == nil {
		return fmt.Errorf("records: nil schema")
	}
	if s.Collection == "" {
		return fmt.Errorf("records: schema 缺少 Collection")
	}
	if len(s.Statuses) == 0 {
		return fmt.Errorf("records: 记录集 %q 未声明状态机", s.Collection)
	}
	if !s.hasStatus(s.InitialStatus) {
		return fmt.Errorf("records: 记录集 %q 的 InitialStatus %q 不在 Statuses 内", s.Collection, s.InitialStatus)
	}
	if s.DedupeKey == nil {
		return fmt.Errorf("records: 记录集 %q 未提供 DedupeKey 函数", s.Collection)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.schemas[s.Collection]; exists {
		return fmt.Errorf("records: 记录集 %q 重复注册", s.Collection)
	}
	r.schemas[s.Collection] = s
	return nil
}

// Get 按 collection 取 schema；未注册返回 ErrUnknownCollection。
func (r *RecordSchemaRegistry) Get(collection string) (*RecordSchema, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.schemas[collection]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownCollection, collection)
	}
	return s, nil
}

// Deregister 移除一个记录集 schema（场景卸载缝，§6.3 按 Receipt 精确清理）。
// 只摘注册表——已落库的记录数据保留，属数据保留策略（平台不越权删数据）。
func (r *RecordSchemaRegistry) Deregister(collection string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.schemas, collection)
}

// Collections 返回已注册的记录集名（用于自省 / 测试）。
func (r *RecordSchemaRegistry) Collections() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.schemas))
	for c := range r.schemas {
		out = append(out, c)
	}
	return out
}
