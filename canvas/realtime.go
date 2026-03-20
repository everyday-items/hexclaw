package canvas

import (
	"fmt"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"
)

// UpdateType 更新类型
type UpdateType string

const (
	UpdateFull   UpdateType = "full"   // 全量面板更新
	UpdatePatch  UpdateType = "patch"  // 单组件更新
	UpdateRemove UpdateType = "remove" // 面板移除
)

// PanelUpdate 推送给订阅者的面板更新
type PanelUpdate struct {
	Type      UpdateType `json:"type"`
	PanelID   string     `json:"panel_id"`
	Panel     *Panel     `json:"panel,omitempty"`
	Component *Component `json:"component,omitempty"`
	Removed   bool       `json:"removed,omitempty"`
}

// Subscriber 订阅者
type Subscriber struct {
	ID      string
	PanelID string            // 订阅的面板 ID，"*" 表示全部
	Ch      chan *PanelUpdate // 更新通道
	done    chan struct{}
}

// Close 关闭订阅
func (s *Subscriber) Close() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// Interaction 用户交互事件
type Interaction struct {
	PanelID     string         `json:"panel_id"`
	ComponentID string         `json:"component_id"`
	Action      string         `json:"action"`
	Data        map[string]any `json:"data"`
	UserID      string         `json:"user_id"`
	Timestamp   time.Time      `json:"timestamp"`
}

// InteractionHandler 交互处理器
type InteractionHandler func(interaction *Interaction) (*PanelUpdate, error)

// RealtimeExtension 为 Service 添加实时双向能力
//
// 管理订阅者和交互处理器。
// 与 Service 配合使用：Service 负责面板 CRUD，
// RealtimeExtension 负责实时推送和交互处理。
type RealtimeExtension struct {
	mu                  sync.RWMutex
	subscribers         map[string]*Subscriber          // subscriber ID → subscriber
	interactionHandlers map[string]InteractionHandler    // panel ID → handler
	maxSubscribers      int                              // 最大订阅数，防止恶意连接导致 OOM
}

// NewRealtimeExtension 创建实时扩展
const defaultMaxSubscribers = 1024

func NewRealtimeExtension() *RealtimeExtension {
	return &RealtimeExtension{
		subscribers:         make(map[string]*Subscriber),
		interactionHandlers: make(map[string]InteractionHandler),
		maxSubscribers:      defaultMaxSubscribers,
	}
}

// Subscribe 订阅面板更新
//
// panelID 为具体面板 ID 或 "*" 表示订阅所有面板。
// 返回 Subscriber，通过其 Ch 接收更新。
func (rt *RealtimeExtension) Subscribe(panelID string) (*Subscriber, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if len(rt.subscribers) >= rt.maxSubscribers {
		return nil, fmt.Errorf("已达到最大订阅数 %d，请先关闭其他连接", rt.maxSubscribers)
	}

	sub := &Subscriber{
		ID:      "sub-" + idgen.ShortID(),
		PanelID: panelID,
		Ch:      make(chan *PanelUpdate, 32),
		done:    make(chan struct{}),
	}
	rt.subscribers[sub.ID] = sub
	return sub, nil
}

// Unsubscribe 取消订阅
func (rt *RealtimeExtension) Unsubscribe(id string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if sub, ok := rt.subscribers[id]; ok {
		sub.Close()
		delete(rt.subscribers, id)
	}
}

// NotifySubscribers 推送更新给匹配的订阅者
func (rt *RealtimeExtension) NotifySubscribers(update *PanelUpdate) {
	rt.mu.RLock()
	var deadIDs []string
	for id, sub := range rt.subscribers {
		select {
		case <-sub.done:
			deadIDs = append(deadIDs, id)
			continue
		default:
		}

		if sub.PanelID == "*" || sub.PanelID == update.PanelID {
			select {
			case sub.Ch <- update:
			default:
				// channel 满，丢弃旧数据
			}
		}
	}
	rt.mu.RUnlock()

	// 清理已关闭的订阅者
	if len(deadIDs) > 0 {
		rt.mu.Lock()
		for _, id := range deadIDs {
			delete(rt.subscribers, id)
		}
		rt.mu.Unlock()
	}
}

// UpdateComponent 更新单个组件并通知订阅者
func (rt *RealtimeExtension) UpdateComponent(svc *Service, panelID, componentID string, component *Component) error {
	svc.mu.Lock()
	panel, ok := svc.panels[panelID]
	if !ok {
		svc.mu.Unlock()
		return fmt.Errorf("面板 %s 不存在", panelID)
	}

	// 替换组件
	found := false
	for i, c := range panel.Components {
		if c.ID == componentID {
			panel.Components[i] = component
			found = true
			break
		}
	}
	if !found {
		svc.mu.Unlock()
		return fmt.Errorf("组件 %s 不存在于面板 %s", componentID, panelID)
	}
	panel.Version++
	svc.mu.Unlock()
	// 注意：这里不能用 defer，因为 NotifySubscribers 不应在持锁时调用（避免死锁）

	// 通知订阅者
	rt.NotifySubscribers(&PanelUpdate{
		Type:      UpdatePatch,
		PanelID:   panelID,
		Component: component,
	})

	return nil
}

// SetInteractionHandler 注册面板交互处理器
func (rt *RealtimeExtension) SetInteractionHandler(panelID string, handler InteractionHandler) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.interactionHandlers[panelID] = handler
}

// HandleInteraction 处理用户交互
func (rt *RealtimeExtension) HandleInteraction(interaction *Interaction) (*PanelUpdate, error) {
	rt.mu.RLock()
	handler, ok := rt.interactionHandlers[interaction.PanelID]
	rt.mu.RUnlock()

	if !ok {
		return nil, nil
	}

	if interaction.Timestamp.IsZero() {
		interaction.Timestamp = time.Now()
	}

	update, err := handler(interaction)
	if err != nil {
		return nil, err
	}

	// 如果处理器返回了更新，自动推送
	if update != nil {
		rt.NotifySubscribers(update)
	}

	return update, nil
}

// PublishAndNotify 发布面板并通知订阅者
func (rt *RealtimeExtension) PublishAndNotify(svc *Service, panel *Panel, handler EventHandler) {
	svc.Publish(panel, handler)
	rt.NotifySubscribers(&PanelUpdate{
		Type:    UpdateFull,
		PanelID: panel.ID,
		Panel:   panel,
	})
}

// RemoveAndNotify 移除面板并通知订阅者
func (rt *RealtimeExtension) RemoveAndNotify(svc *Service, panelID string) {
	svc.RemovePanel(panelID)
	rt.NotifySubscribers(&PanelUpdate{
		Type:    UpdateRemove,
		PanelID: panelID,
		Removed: true,
	})
}

// SubscriberCount 返回当前订阅者数量
func (rt *RealtimeExtension) SubscriberCount() int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return len(rt.subscribers)
}
