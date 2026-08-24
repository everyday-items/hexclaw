package instances

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/adapter/dingtalk"
)

type managerInboundPhotoAdmissionProbe struct{}

func (*managerInboundPhotoAdmissionProbe) AdmitInboundPhoto(
	context.Context, *adapter.Message,
) (bool, error) {
	return true, nil
}

type managerDingTalkAdmissionAdapter struct {
	portAtStart dingtalk.InboundPhotoAdmissionPort
	port        dingtalk.InboundPhotoAdmissionPort
}

func (*managerDingTalkAdmissionAdapter) Name() string { return "dingtalk-admission-probe" }

func (*managerDingTalkAdmissionAdapter) Platform() adapter.Platform {
	return adapter.PlatformDingtalk
}

func (a *managerDingTalkAdmissionAdapter) SetInboundPhotoAdmissionPort(
	port dingtalk.InboundPhotoAdmissionPort,
) {
	a.port = port
}

func (a *managerDingTalkAdmissionAdapter) Start(
	_ context.Context, _ adapter.MessageHandler,
) error {
	a.portAtStart = a.port
	return nil
}

func (*managerDingTalkAdmissionAdapter) Stop(context.Context) error { return nil }

func (*managerDingTalkAdmissionAdapter) Send(
	context.Context, string, *adapter.Reply,
) error {
	return nil
}

func (*managerDingTalkAdmissionAdapter) SendStream(
	context.Context, string, <-chan *adapter.ReplyChunk,
) error {
	return nil
}

func TestManagerInstallsDingTalkInboundPhotoAdmissionBeforeStart(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	inst := &Instance{
		Provider: "dingtalk",
		Name:     "dingtalk-admission",
		Enabled:  true,
		Config:   []byte(`{"app_key":"x","app_secret":"y"}`),
	}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatal(err)
	}
	probe := &managerInboundPhotoAdmissionProbe{}
	adapterProbe := &managerDingTalkAdmissionAdapter{}
	mgr.buildAdapter = func(*Instance) (adapter.Adapter, error) {
		return adapterProbe, nil
	}
	mgr.SetHandler(func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		return nil, nil
	})
	mgr.SetDingTalkInboundPhotoAdmissionPort(probe)

	if err := mgr.Start(ctx, inst.Name); err != nil {
		t.Fatal(err)
	}
	if adapterProbe.portAtStart != probe {
		t.Fatalf("DingTalk admission port at Start = %#v, want %#v", adapterProbe.portAtStart, probe)
	}
}
