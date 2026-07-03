package instances

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type stubAdapter struct {
	healthErr  error
	startPanic any
	startCalls int
}

func (a *stubAdapter) Name() string { return "stub" }

func (a *stubAdapter) Platform() adapter.Platform { return adapter.PlatformSlack }

func (a *stubAdapter) Start(context.Context, adapter.MessageHandler) error {
	a.startCalls++
	if a.startPanic != nil {
		panic(a.startPanic)
	}
	return nil
}

func (a *stubAdapter) Stop(context.Context) error { return nil }

func (a *stubAdapter) Send(context.Context, string, *adapter.Reply) error { return nil }

func (a *stubAdapter) SendStream(context.Context, string, <-chan *adapter.ReplyChunk) error {
	return nil
}

func (a *stubAdapter) Health(context.Context) error { return a.healthErr }

func newTestManager(t *testing.T) (*Manager, func()) {
	t.Helper()

	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "instances.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 存储失败: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化 SQLite 存储失败: %v", err)
	}

	mgr := NewManager(store.DB())
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatalf("初始化实例管理器失败: %v", err)
	}
	return mgr, func() { store.Close() }
}

func TestManagerHealthRunningInstance(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	inst := &Instance{
		Provider: "slack",
		Name:     "slack-main",
		Enabled:  true,
		Config:   []byte(`{"token":"x"}`),
	}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("保存实例失败: %v", err)
	}
	if err := mgr.setStatus(ctx, inst.Name, StatusRunning, ""); err != nil {
		t.Fatalf("设置实例状态失败: %v", err)
	}

	mgr.running[inst.Name] = &stubAdapter{}

	report, err := mgr.Health(ctx, inst.Name)
	if err != nil {
		t.Fatalf("健康检查失败: %v", err)
	}
	if !report.Healthy {
		t.Fatalf("期望 healthy=true，实际 false: %+v", report)
	}
	if report.Status != StatusRunning {
		t.Fatalf("期望 status=running，实际 %s", report.Status)
	}
}

func TestManagerHealthMarksError(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	inst := &Instance{
		Provider: "discord",
		Name:     "discord-main",
		Enabled:  true,
		Config:   []byte(`{"token":"x"}`),
	}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("保存实例失败: %v", err)
	}
	if err := mgr.setStatus(ctx, inst.Name, StatusRunning, ""); err != nil {
		t.Fatalf("设置实例状态失败: %v", err)
	}

	mgr.running[inst.Name] = &stubAdapter{healthErr: fmt.Errorf("gateway down")}

	report, err := mgr.Health(ctx, inst.Name)
	if err != nil {
		t.Fatalf("健康检查失败: %v", err)
	}
	if report.Healthy {
		t.Fatalf("期望 healthy=false，实际 true: %+v", report)
	}
	if report.Status != StatusError {
		t.Fatalf("期望 status=error，实际 %s", report.Status)
	}
	if report.LastError != "gateway down" {
		t.Fatalf("期望 last_error=gateway down，实际 %q", report.LastError)
	}

	persisted, err := mgr.Get(ctx, inst.Name)
	if err != nil {
		t.Fatalf("读取实例失败: %v", err)
	}
	if persisted.Status != StatusError {
		t.Fatalf("期望持久化状态为 error，实际 %s", persisted.Status)
	}
}

func TestBug20260630_StartDisabledProviderDoesNotBuildOrRun(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	inst := &Instance{
		Provider: "dingtalk",
		Name:     "dt-disabled",
		Enabled:  true,
		Config:   []byte(`{"app_key":"x","app_secret":"y"}`),
	}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("保存实例失败: %v", err)
	}
	var built bool
	mgr.buildAdapter = func(*Instance) (adapter.Adapter, error) {
		built = true
		return &stubAdapter{}, nil
	}
	mgr.SetHandler(func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "ok"}, nil
	})
	mgr.SetDisabledProviders("dingtalk")

	err := mgr.Start(ctx, inst.Name)
	if err == nil {
		t.Fatal("禁用 provider 后 Start 应返回错误")
	}
	if built {
		t.Fatal("禁用 provider 后不应构造 adapter")
	}
	persisted, perr := mgr.Get(ctx, inst.Name)
	if perr != nil {
		t.Fatalf("读取实例失败: %v", perr)
	}
	if persisted.Status != StatusError || persisted.LastError == "" {
		t.Fatalf("禁用 provider 应持久化 error 状态，got status=%s err=%q", persisted.Status, persisted.LastError)
	}
}

func TestBug20260630_StartPanicIsContainedAndPersisted(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	inst := &Instance{
		Provider: "dingtalk",
		Name:     "dt-panic",
		Enabled:  true,
		Config:   []byte(`{"app_key":"x","app_secret":"y"}`),
	}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("保存实例失败: %v", err)
	}
	adp := &stubAdapter{startPanic: "sdk closed channel"}
	mgr.buildAdapter = func(*Instance) (adapter.Adapter, error) { return adp, nil }
	mgr.SetHandler(func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "ok"}, nil
	})

	err := mgr.Start(ctx, inst.Name)
	if err == nil {
		t.Fatal("adapter.Start panic 应转为错误")
	}
	if adp.startCalls != 1 {
		t.Fatalf("Start 调用次数=%d，want 1", adp.startCalls)
	}
	if _, ok := mgr.running[inst.Name]; ok {
		t.Fatal("Start panic 后不应把 adapter 标记为 running")
	}
	persisted, perr := mgr.Get(ctx, inst.Name)
	if perr != nil {
		t.Fatalf("读取实例失败: %v", perr)
	}
	if persisted.Status != StatusError || !strings.Contains(persisted.LastError, "sdk closed channel") {
		t.Fatalf("panic 应持久化为可诊断 error，got status=%s err=%q", persisted.Status, persisted.LastError)
	}
}
