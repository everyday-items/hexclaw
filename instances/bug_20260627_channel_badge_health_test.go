package instances

import (
	"context"
	"fmt"
	"testing"
)

// BUG-20260627: 钉钉(及 feishu/discord 等异步 Stream 适配器)显示「已连接」，但点「测试」返回
// 「dingtalk Stream 未连接」——同一频道两个真相源：徽章读 List()(DB status)，测试读 live
// Health()。根因=Start() 在连接循环刚启动时就乐观写 running，Stream 尚未/无法真正连上。
// 修复=handleListInstances 改用 ListLive()，以 live Health 作单一真相源对账徽章状态。
func TestBug20260627_ChannelBadgeReflectsLiveStreamHealth(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	inst := &Instance{Provider: "dingtalk", Name: "dingtalk-main", Enabled: true, Config: []byte(`{"app_key":"x"}`)}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Start() 乐观写 running（Stream 连接异步，此刻可能尚未/无法连上）。
	if err := mgr.setStatus(ctx, inst.Name, StatusRunning, ""); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	// 模拟：适配器在跑，但 Stream 未连上（conn==nil → Health 报错），正是用户场景。
	mgr.running[inst.Name] = &stubAdapter{healthErr: fmt.Errorf("dingtalk Stream 未连接")}

	// 徽章(ListLive)必须显示 error，而非误导的 running(已连接)。
	// ★先断徽章再断 Health：Health() 自身会把 error 落库，先调它会掩盖 RED。
	got := findInstance(t, mgr, inst.Name)
	if got.Status == StatusRunning {
		t.Fatalf("BUG：徽章仍显示 running(已连接)，但 Stream 未连接——徽章与测试按钮不一致")
	}
	if got.Status != StatusError {
		t.Errorf("徽章状态应为 error(与 Health 一致)，got %s", got.Status)
	}
	if got.LastError == "" {
		t.Errorf("error 状态应带 last_error 供 UI 展示原因")
	}

	// 测试按钮(Health)同样判定没连上——两源一致。
	hr, err := mgr.Health(ctx, inst.Name)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if hr.Healthy || hr.Status != StatusError {
		t.Fatalf("Health 应报 error/unhealthy（与徽章一致），got %+v", hr)
	}
}

// 反向：Stream 真连上时，ListLive 显示 running 且清空旧 last_error（防卡死在 error）。
func TestBug20260627_ListLiveRecoversWhenStreamConnects(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	inst := &Instance{Provider: "dingtalk", Name: "dt2", Enabled: true, Config: []byte(`{"app_key":"x"}`)}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 之前因没连上被标 error。
	if err := mgr.setStatus(ctx, inst.Name, StatusError, "dingtalk Stream 未连接"); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	// 现在 Stream 真连上了（Health ok）。
	mgr.running[inst.Name] = &stubAdapter{}

	got := findInstance(t, mgr, inst.Name)
	if got.Status != StatusRunning {
		t.Fatalf("重连后 ListLive 应恢复 running，got %s", got.Status)
	}
	if got.LastError != "" {
		t.Errorf("恢复 running 应清空 last_error，got %q", got.LastError)
	}
}

// DB 标 running 但没有 live runtime（崩溃/重启后未起）→ ListLive 显示 error，不报假「已连接」。
func TestBug20260627_ListLiveRunningWithoutRuntimeIsError(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	inst := &Instance{Provider: "dingtalk", Name: "dt3", Enabled: true, Config: []byte(`{"app_key":"x"}`)}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mgr.setStatus(ctx, inst.Name, StatusRunning, ""); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	// 不注入 m.running → 没有 live runtime。
	got := findInstance(t, mgr, inst.Name)
	if got.Status != StatusError {
		t.Fatalf("DB running 但无 runtime 应为 error，got %s", got.Status)
	}
}

// webhook 类适配器（无 HealthChecker）保持 running（已挂载即就绪），不被误判 error。
func TestBug20260627_ListLiveWebhookAdapterStaysRunning(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	inst := &Instance{Provider: "wechat", Name: "wechat-1", Enabled: true, Config: []byte(`{"token":"x"}`)}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mgr.setStatus(ctx, inst.Name, StatusRunning, ""); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	mgr.running[inst.Name] = &noHealthAdapter{}

	got := findInstance(t, mgr, inst.Name)
	if got.Status != StatusRunning {
		t.Fatalf("无 HealthChecker 的 webhook 适配器应保持 running，got %s", got.Status)
	}
}

func findInstance(t *testing.T, mgr *Manager, name string) *Instance {
	t.Helper()
	list, err := mgr.ListLive(context.Background())
	if err != nil {
		t.Fatalf("ListLive: %v", err)
	}
	for _, i := range list {
		if i.Name == name {
			return i
		}
	}
	t.Fatalf("instance %q missing from ListLive", name)
	return nil
}

// noHealthAdapter 是个不实现 adapter.HealthChecker 的适配器（模拟 webhook 类）。
type noHealthAdapter struct{ stubAdapter }

func (a *noHealthAdapter) Health() {} // 故意签名不符 HealthChecker，使类型断言失败
