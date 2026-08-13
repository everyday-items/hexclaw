package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12apihttp "github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type c02ExpectedBindingAuthSolveExec struct{}

func (c02ExpectedBindingAuthSolveExec) Execute(
	context.Context,
	map[string]any,
) (*skill.Result, error) {
	return &skill.Result{}, nil
}

type c02ExpectedBindingAuthDelivery struct {
	sends int
}

func c02ExpectedBindingAuthTarget() usecase.ResolvedDeliveryTarget {
	return usecase.ResolvedDeliveryTarget{
		BindingID: "agent-rule:101",
		Target: k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: "bot-a", ChatID: "parent", Label: "dingtalk",
		},
	}
}

func (*c02ExpectedBindingAuthDelivery) ResolveTextTargets(
	context.Context,
	string,
) ([]usecase.ResolvedDeliveryTarget, error) {
	return []usecase.ResolvedDeliveryTarget{c02ExpectedBindingAuthTarget()}, nil
}

func (*c02ExpectedBindingAuthDelivery) PrepareTextForTargets(
	_ context.Context,
	_ string,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	out := make([]usecase.PreparedTextDelivery, 0, len(targets))
	for _, target := range targets {
		out = append(out, usecase.PreparedTextDelivery{
			BindingID: target.BindingID, Target: target.Target,
			PayloadJSON: `{"text":"fixed"}`, RenderJSON: `{}`,
		})
	}
	return out, nil
}

func (*c02ExpectedBindingAuthDelivery) PrepareText(
	context.Context,
	string,
	string,
) (usecase.PreparedTextDelivery, error) {
	target := c02ExpectedBindingAuthTarget()
	return usecase.PreparedTextDelivery{
		BindingID: target.BindingID, Target: target.Target,
		PayloadJSON: `{"text":"fixed"}`, RenderJSON: `{}`,
	}, nil
}

func (d *c02ExpectedBindingAuthDelivery) SendPrepared(
	context.Context,
	k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	d.sends++
	return usecase.DeliveryTransportAck{
		Status: k12.DeliveryDelivered, ExternalMessageID: "must-not-send",
	}, nil
}

func (*c02ExpectedBindingAuthDelivery) QueryPrepared(
	context.Context,
	k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	return usecase.DeliveryTransportAck{}, nil
}

// 回归锁（修复⑤）：配了 APIToken 时，非回环、无 Authorization 的 K12 写请求必须被拦（401），
// 与 /api/v1 写端点一致；loopback 仍放行（cron http_get 到本机、桌面 sidecar）。
func TestK12WriteEndpointsRequireAuth(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	s.cfg.Server.APIToken = "secret-token"
	// BUG-4：场景鉴权前缀从挂载注册表派生——注册 K12 子路由，守卫才认它（等价 composition root 的 Mount）。
	s.Mount("/api/k12", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	reached := false
	guarded := s.apiAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	call := func(method, path, remote, auth string) (int, bool) {
		reached = false
		req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
		req.RemoteAddr = remote
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code, reached
	}

	// 非回环 + 无 token → K12 写端点必须 401（此前 bug：直接 200 放行）。
	for _, p := range []string{"/api/k12/grade", "/api/k12/restore", "/api/k12/cron/provision", "/api/k12/cron/reconcile-defaults", "/api/k12/bind-im"} {
		if code, hit := call(http.MethodPost, p, "203.0.113.7:5555", ""); code != http.StatusUnauthorized || hit {
			t.Errorf("%s 非回环无 token 应 401 且不进 handler，got code=%d hit=%v", p, code, hit)
		}
	}
	// 非回环 + 正确 token → 放行。
	if code, hit := call(http.MethodPost, "/api/k12/grade", "203.0.113.7:5555", "Bearer secret-token"); code != http.StatusOK || !hit {
		t.Errorf("带正确 token 应放行，got code=%d hit=%v", code, hit)
	}
	// loopback 无 token → 仍放行（cron/桌面）。
	if code, hit := call(http.MethodPost, "/api/k12/grade", "127.0.0.1:1234", ""); code != http.StatusOK || !hit {
		t.Errorf("loopback 应放行（cron http_get），got code=%d hit=%v", code, hit)
	}
	// K12 读端点（GET）也须鉴权——backup/export/profile/mistakes 含孩子 PII，非回环无 token 必拦。
	for _, p := range []string{"/api/k12/backup", "/api/k12/export", "/api/k12/profile", "/api/k12/mistakes"} {
		if code, hit := call(http.MethodGet, p, "203.0.113.7:5555", ""); code != http.StatusUnauthorized || hit {
			t.Errorf("%s 读端点非回环无 token 应 401（含 PII），got code=%d hit=%v", p, code, hit)
		}
	}
	// loopback GET 仍放行（桌面直接拉）。
	if code, _ := call(http.MethodGet, "/api/k12/mistakes", "127.0.0.1:1234", ""); code != http.StatusOK {
		t.Errorf("loopback 读端点应放行，got code=%d", code)
	}
}

func TestK12C02ExpectedBindingSendRequiresExactSidecarCapabilityBeforeHandler(
	t *testing.T,
) {
	const (
		capability = "c02-native-sidecar-capability-0123456789"
		artifactID = "grading-final-c02-auth"
		digest     = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	if capability == "" {
		t.Fatal("test requires a non-empty sidecar capability")
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	testCtx := t.Context()
	if migrateErr := migrate.Run(testCtx, db, migrate.All); migrateErr != nil {
		t.Fatal(migrateErr)
	}
	if _, execErr := db.ExecContext(testCtx, `
		INSERT INTO agents(name) VALUES('mingming');
		INSERT INTO k12_grading_jobs
			(record_id,agent_name,status,dedupe_key,created_at,updated_at)
		VALUES('job-c02-auth','mingming','completed','job-c02-auth',100,100);
		INSERT INTO k12_grading_final_artifacts
			(artifact_id,agent_name,job_id,structure_version,coverage_status,
			 total_count,published_count,skipped_count,ordered_current_digests_json,
			 canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		artifactID, "mingming", "job-c02-auth", 1, "complete",
		16, 16, 0, `["`+digest+`"]`, "14 道正确 / 2 道过程问题", digest,
		"summary-c02-auth", 100, 100,
	); execErr != nil {
		t.Fatal(execErr)
	}
	delivery := &c02ExpectedBindingAuthDelivery{}
	wired, err := assembly.Wire(
		db,
		c02ExpectedBindingAuthSolveExec{},
		assembly.WithDeliveryTransport(delivery),
	)
	if err != nil {
		t.Fatal(err)
	}
	k12Handler := k12apihttp.NewHandler(k12apihttp.Runtime{
		Views: wired.Registry.Views, Records: wired.Records, Deps: wired.Deps,
	})
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = ""
	srv := &Server{cfg: cfg}
	srv.SetSidecarCapabilityToken(capability)
	srv.Mount("/api/k12", k12Handler)
	h := srv.routes()

	body := `{
		"agent":"mingming",
		"final_artifact_id":"grading-final-c02-auth",
		"final_artifact_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"expected_binding":{
			"binding_id":"agent-rule:101",
			"platform":"dingtalk",
			"instance_id":"bot-a",
			"chat_id":"wrong-parent"
		}
	}`
	assertNoDeliverySideEffects := func(stage string) {
		t.Helper()
		var batches, receipts, attempts int
		if err := db.QueryRowContext(testCtx, `SELECT count(*) FROM k12_delivery_batches`).Scan(&batches); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(testCtx, `SELECT count(*), COALESCE(sum(attempt),0)
			FROM k12_delivery_receipts`).Scan(&receipts, &attempts); err != nil {
			t.Fatal(err)
		}
		if batches != 0 || receipts != 0 || attempts != 0 || delivery.sends != 0 {
			t.Fatalf("%s changed delivery state: batches=%d receipts=%d attempts=%d sends=%d",
				stage, batches, receipts, attempts, delivery.sends)
		}
	}
	request := func(auth string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(
			testCtx,
			http.MethodPost,
			"/api/k12/tutoring-tips/send",
			strings.NewReader(body),
		)
		req.RemoteAddr = "127.0.0.1:42123"
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct {
		name string
		auth string
	}{
		{name: "anonymous"},
		{name: "wrong bearer", auth: "Bearer wrong-capability"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := request(tc.auth)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s want 401", rec.Code, rec.Body.String())
			}
			assertNoDeliverySideEffects(tc.name)
		})
	}

	// 精确匹配的 capability 会抵达实际挂载的路由；随后故意传入的
	// 过期绑定会被 use-case CAS 拒绝，且仍不触发发送。
	rec := request("Bearer " + capability)
	if rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), `"error":"binding_snapshot_conflict"`) {
		t.Fatalf("authorized mounted route status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertNoDeliverySideEffects("authorized binding drift")
}
