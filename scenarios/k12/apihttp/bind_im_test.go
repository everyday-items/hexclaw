package apihttp_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type fakeBinder struct {
	platform, instanceID, chatID, agent string
	calls                               int
}

func (f *fakeBinder) Bind(_ context.Context, platform, instanceID, chatID, agent string) error {
	f.platform, f.instanceID, f.chatID, f.agent = platform, instanceID, chatID, agent
	f.calls++
	return nil
}

func newServerWithBinder(t *testing.T, b apihttp.IMBinder) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	k, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{
		Views: k.Registry.Views, Records: k.Records, Deps: k.Deps, Binder: b,
	})
}

func TestBindIM_Success(t *testing.T) {
	b := &fakeBinder{}
	h := newServerWithBinder(t, b)
	rec, out := do(t, h, "POST", "/bind-im",
		`{"agent":"mingming","platform":"dingtalk","instance_id":"dt-parent","chat_id":"direct-42"}`)
	if rec.Code != 200 {
		t.Fatalf("状态 %d", rec.Code)
	}
	if bound, _ := out["bound"].(bool); !bound {
		t.Errorf("应 bound=true, got %v", out)
	}
	if b.calls != 1 || b.agent != "mingming" || b.platform != "dingtalk" || b.chatID != "direct-42" {
		t.Errorf("绑定透传不符: %+v", b)
	}
}

func TestBindIM_MissingFields(t *testing.T) {
	h := newServerWithBinder(t, &fakeBinder{})
	rec, _ := do(t, h, "POST", "/bind-im", `{"agent":"mingming"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 platform/chat_id 应 400, got %d", rec.Code)
	}
}

func TestBindIM_501WhenNoBinder(t *testing.T) {
	h := newServer(t) // 无 Binder
	rec, _ := do(t, h, "POST", "/bind-im", `{"agent":"mingming","platform":"dingtalk","chat_id":"g"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("无 binder 应 501, got %d", rec.Code)
	}
}
