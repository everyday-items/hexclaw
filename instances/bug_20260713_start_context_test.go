package instances

import (
	"context"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type startContextProbeAdapter struct {
	stubAdapter
	startCtx context.Context
}

func (a *startContextProbeAdapter) Start(ctx context.Context, _ adapter.MessageHandler) error {
	a.startCtx = ctx
	return nil
}

// BUG-20260713：POST /instances/{name}/start 过去把 r.Context() 直接交给长生命周期适配器；
// HTTP 响应一写完该 context 就取消，DingTalk/Discord/Telegram 等异步连接随即被杀掉。
func TestManagerStartDetachesAdapterLifetimeFromRequestContext_BUG20260713(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	inst := &Instance{
		Provider: "slack",
		Name:     "manual-start",
		Enabled:  true,
		Config:   []byte(`{"token":"x"}`),
	}
	if err := mgr.Upsert(requestCtx, inst); err != nil {
		t.Fatalf("保存实例失败: %v", err)
	}
	probe := &startContextProbeAdapter{}
	mgr.buildAdapter = func(*Instance) (adapter.Adapter, error) { return probe, nil }
	mgr.SetHandler(func(context.Context, *adapter.Message) (*adapter.Reply, error) { return nil, nil })

	if err := mgr.Start(requestCtx, inst.Name); err != nil {
		t.Fatalf("启动实例失败: %v", err)
	}
	cancelRequest() // 模拟 start HTTP handler 返回

	select {
	case <-probe.startCtx.Done():
		t.Fatalf("请求结束取消了实例生命周期: %v", probe.startCtx.Err())
	case <-time.After(50 * time.Millisecond):
		// 实例仍运行；其真正停止由 Manager.Stop → adapter.Stop 负责。
	}
}
