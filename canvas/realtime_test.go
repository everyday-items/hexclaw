package canvas

import (
	"testing"
	"time"
)

func TestSubscribeAndNotify(t *testing.T) {
	svc := NewService()
	rt := NewRealtimeExtension()

	sub := rt.Subscribe("panel-1")
	defer rt.Unsubscribe(sub.ID)

	if rt.SubscriberCount() != 1 {
		t.Fatalf("订阅者数量不正确: %d", rt.SubscriberCount())
	}

	// 发布面板并通知
	panel := NewPanel("panel-1", "测试面板")
	panel.Add(Markdown("hello"))
	rt.PublishAndNotify(svc, panel, nil)

	select {
	case update := <-sub.Ch:
		if update.Type != UpdateFull {
			t.Errorf("期望 full 更新，得到 %s", update.Type)
		}
		if update.PanelID != "panel-1" {
			t.Errorf("面板 ID 不匹配: %s", update.PanelID)
		}
		if update.Panel == nil {
			t.Error("面板不应为 nil")
		}
	case <-time.After(time.Second):
		t.Fatal("超时未收到更新")
	}
}

func TestSubscribeWildcard(t *testing.T) {
	svc := NewService()
	rt := NewRealtimeExtension()

	sub := rt.Subscribe("*") // 订阅所有面板
	defer rt.Unsubscribe(sub.ID)

	panel := NewPanel("any-panel", "任意面板")
	rt.PublishAndNotify(svc, panel, nil)

	select {
	case update := <-sub.Ch:
		if update.PanelID != "any-panel" {
			t.Errorf("面板 ID 不匹配: %s", update.PanelID)
		}
	case <-time.After(time.Second):
		t.Fatal("通配符订阅者应收到更新")
	}
}

func TestRemoveAndNotify(t *testing.T) {
	svc := NewService()
	rt := NewRealtimeExtension()

	panel := NewPanel("to-remove", "待移除")
	svc.Publish(panel, nil)

	sub := rt.Subscribe("to-remove")
	defer rt.Unsubscribe(sub.ID)

	rt.RemoveAndNotify(svc, "to-remove")

	select {
	case update := <-sub.Ch:
		if update.Type != UpdateRemove {
			t.Errorf("期望 remove 更新，得到 %s", update.Type)
		}
		if !update.Removed {
			t.Error("Removed 应为 true")
		}
	case <-time.After(time.Second):
		t.Fatal("超时未收到移除通知")
	}

	// 确认面板已被移除
	if _, ok := svc.GetPanel("to-remove"); ok {
		t.Error("面板应已被移除")
	}
}

func TestUpdateComponent(t *testing.T) {
	svc := NewService()
	rt := NewRealtimeExtension()

	panel := NewPanel("comp-test", "组件测试")
	md := Markdown("原始内容")
	panel.Add(md)
	svc.Publish(panel, nil)

	sub := rt.Subscribe("comp-test")
	defer rt.Unsubscribe(sub.ID)

	newComp := Markdown("更新内容")
	newComp.ID = md.ID // 保持 ID 一致
	err := rt.UpdateComponent(svc, "comp-test", md.ID, newComp)
	if err != nil {
		t.Fatalf("更新组件失败: %v", err)
	}

	select {
	case update := <-sub.Ch:
		if update.Type != UpdatePatch {
			t.Errorf("期望 patch 更新，得到 %s", update.Type)
		}
		if update.Component == nil {
			t.Error("组件不应为 nil")
		}
	case <-time.After(time.Second):
		t.Fatal("超时未收到组件更新")
	}
}

func TestUpdateComponentNotFound(t *testing.T) {
	svc := NewService()
	rt := NewRealtimeExtension()

	// 面板不存在
	err := rt.UpdateComponent(svc, "ghost", "comp", Markdown("x"))
	if err == nil {
		t.Error("更新不存在的面板应报错")
	}

	// 组件不存在
	panel := NewPanel("exists", "存在")
	svc.Publish(panel, nil)
	err = rt.UpdateComponent(svc, "exists", "ghost-comp", Markdown("x"))
	if err == nil {
		t.Error("更新不存在的组件应报错")
	}
}

func TestHandleInteraction(t *testing.T) {
	rt := NewRealtimeExtension()

	called := false
	rt.SetInteractionHandler("interactive", func(i *Interaction) (*PanelUpdate, error) {
		called = true
		if i.Action != "click" {
			t.Errorf("action 不匹配: %s", i.Action)
		}
		return &PanelUpdate{
			Type:    UpdateFull,
			PanelID: "interactive",
		}, nil
	})

	sub := rt.Subscribe("interactive")
	defer rt.Unsubscribe(sub.ID)

	update, err := rt.HandleInteraction(&Interaction{
		PanelID:     "interactive",
		ComponentID: "btn-1",
		Action:      "click",
		Data:        map[string]any{"value": "ok"},
	})
	if err != nil {
		t.Fatalf("处理交互失败: %v", err)
	}
	if !called {
		t.Error("处理器应被调用")
	}
	if update == nil {
		t.Error("应返回更新")
	}

	// 订阅者也应收到
	select {
	case <-sub.Ch:
	case <-time.After(time.Second):
		t.Fatal("订阅者应收到交互产生的更新")
	}
}

func TestHandleInteractionNoHandler(t *testing.T) {
	rt := NewRealtimeExtension()

	update, err := rt.HandleInteraction(&Interaction{PanelID: "no-handler"})
	if err != nil {
		t.Fatalf("无处理器不应报错: %v", err)
	}
	if update != nil {
		t.Error("无处理器应返回 nil")
	}
}

func TestUnsubscribe(t *testing.T) {
	rt := NewRealtimeExtension()
	sub := rt.Subscribe("test")
	if rt.SubscriberCount() != 1 {
		t.Fatal("应有 1 个订阅者")
	}
	rt.Unsubscribe(sub.ID)
	if rt.SubscriberCount() != 0 {
		t.Fatal("取消订阅后应有 0 个订阅者")
	}
}
