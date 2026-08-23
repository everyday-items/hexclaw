package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/autonomy"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	"github.com/hexagon-codes/hexclaw/webhook"
)

type webhookOwnerAPIFixture struct {
	db     *sql.DB
	srv    *Server
	mgr    *webhook.Manager
	grants *autonomy.GrantStore
}

type webhookOwnerAPIRow struct {
	ID      string
	Name    string
	OwnerID string
	Enabled bool
}

type webhookOwnerAPIGrant struct {
	ID      string
	TaskRef string
	OwnerID string
	Revoked bool
}

type webhookOwnerAPIMemoryRow struct {
	Name    string
	Exists  bool
	ID      string
	OwnerID string
	Enabled bool
}

type webhookOwnerAPISnapshot struct {
	DBWebhooks   []webhookOwnerAPIRow
	Memory       []webhookOwnerAPIMemoryRow
	DBGrants     []webhookOwnerAPIGrant
	ActiveGrants []webhookOwnerAPIGrant
}

func newWebhookOwnerAPIFixture(t *testing.T) *webhookOwnerAPIFixture {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("initialize migrations: %v", err)
	}
	mgr := webhook.NewManager(db)
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatalf("initialize webhook manager: %v", err)
	}
	grants := autonomy.NewGrantStore(db)
	if err := grants.Init(context.Background()); err != nil {
		t.Fatalf("initialize grant store: %v", err)
	}
	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetWebhookManager(mgr)
	srv.autonomyGrants = grants
	return &webhookOwnerAPIFixture{db: db, srv: srv, mgr: mgr, grants: grants}
}

func (f *webhookOwnerAPIFixture) seed(t *testing.T, name, owner string, enabled bool) string {
	t.Helper()
	wh := &webhook.Webhook{
		ID: "wh-" + name, Name: name, Type: webhook.TypeGeneric,
		Secret: "owner-secret", Prompt: "run", UserID: owner, Enabled: enabled,
	}
	if err := f.mgr.Register(context.Background(), wh); err != nil {
		t.Fatalf("register webhook %q: %v", name, err)
	}
	taskRef := "webhook:" + wh.ID
	ctx := skill.WithAuthenticatedUser(context.Background(), owner)
	if _, err := f.grants.Create(ctx, autonomy.Grant{
		ID: "grant-" + name, TaskRef: taskRef, Source: "webhook", Entries: []string{"browser.open"},
	}); err != nil {
		t.Fatalf("create grant for %q: %v", name, err)
	}
	return taskRef
}

func (f *webhookOwnerAPIFixture) snapshot(t *testing.T, names ...string) webhookOwnerAPISnapshot {
	t.Helper()
	ctx := context.Background()
	var out webhookOwnerAPISnapshot

	rows, err := f.db.QueryContext(ctx, `SELECT id, name, user_id, enabled FROM webhooks ORDER BY name`)
	if err != nil {
		t.Fatalf("query webhook rows: %v", err)
	}
	for rows.Next() {
		var row webhookOwnerAPIRow
		var enabled int
		if err := rows.Scan(&row.ID, &row.Name, &row.OwnerID, &enabled); err != nil {
			_ = rows.Close()
			t.Fatalf("scan webhook row: %v", err)
		}
		row.Enabled = enabled == 1
		out.DBWebhooks = append(out.DBWebhooks, row)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close webhook rows: %v", err)
	}

	for _, name := range names {
		row := webhookOwnerAPIMemoryRow{Name: name}
		if wh, ok := f.mgr.Get(name); ok {
			row.Exists = true
			row.ID = wh.ID
			row.OwnerID = wh.UserID
			row.Enabled = wh.Enabled
		}
		out.Memory = append(out.Memory, row)
	}
	sort.Slice(out.Memory, func(i, j int) bool { return out.Memory[i].Name < out.Memory[j].Name })

	grantRows, err := f.db.QueryContext(ctx, `
		SELECT id, task_ref, owner_id, CASE WHEN revoked_at IS NULL THEN 0 ELSE 1 END
		FROM autonomy_grants ORDER BY id`)
	if err != nil {
		t.Fatalf("query grant rows: %v", err)
	}
	for grantRows.Next() {
		var grant webhookOwnerAPIGrant
		var revoked int
		if err := grantRows.Scan(&grant.ID, &grant.TaskRef, &grant.OwnerID, &revoked); err != nil {
			_ = grantRows.Close()
			t.Fatalf("scan grant row: %v", err)
		}
		grant.Revoked = revoked == 1
		out.DBGrants = append(out.DBGrants, grant)
	}
	if err := grantRows.Close(); err != nil {
		t.Fatalf("close grant rows: %v", err)
	}

	for _, grant := range f.grants.ListActive("") {
		out.ActiveGrants = append(out.ActiveGrants, webhookOwnerAPIGrant{
			ID: grant.ID, TaskRef: grant.TaskRef, OwnerID: grant.OwnerID,
		})
	}
	sort.Slice(out.ActiveGrants, func(i, j int) bool { return out.ActiveGrants[i].ID < out.ActiveGrants[j].ID })
	return out
}

func requireWebhookOwnerAPISnapshot(t *testing.T, before, after webhookOwnerAPISnapshot) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected management operation changed state\nbefore=%+v\nafter=%+v", before, after)
	}
}

func webhookOwnerAPIRequest(method, name, queryOwner, bodyOwner string, enabled *bool) *http.Request {
	body := ""
	if enabled != nil {
		body = `{"enabled":` + map[bool]string{true: "true", false: "false"}[*enabled] +
			`,"user_id":"` + bodyOwner + `"}`
	} else if bodyOwner != "" {
		body = `{"user_id":"` + bodyOwner + `"}`
	}
	path := "/api/v1/webhooks/" + name
	if queryOwner != "" {
		path += "?user_id=" + queryOwner
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.SetPathValue("name", name)
	return req
}

func authenticatedWebhookOwnerAPIRequest(method, name, owner, body string) *http.Request {
	req := httptest.NewRequest(method, "/api/v1/webhooks/"+name, strings.NewReader(body))
	req.SetPathValue("name", name)
	return req.WithContext(skill.WithAuthenticatedUser(context.Background(), owner))
}

// TestBUG20260801003WebhookCreateFreezesAuthenticatedOwner 钉死 Webhook
// 与任务级授权使用同一可信所有者，客户端 user_id 不能改写归属。
func TestBUG20260801003WebhookCreateFreezesAuthenticatedOwner(t *testing.T) {
	srv := newWebhookTestServer(t)
	body := `{"name":"trusted-owner-hook","type":"generic","prompt":"run","user_id":"forged-owner"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(body))
	req = req.WithContext(skill.WithAuthenticatedUser(context.Background(), "trusted-owner"))
	rec := httptest.NewRecorder()

	srv.handleRegisterWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	created, ok := srv.webhookMgr.Get("trusted-owner-hook")
	if !ok {
		t.Fatal("created webhook not found")
	}
	if created.UserID != "trusted-owner" {
		t.Fatalf("webhook owner=%q, want authenticated owner", created.UserID)
	}
}

// TestBUG20260801003K12WebhookCreateFreezesAuthenticatedOwner 钉死 K12
// binding 的所有者只能来自认证上下文，query/body 均不能改写归属。
func TestBUG20260801003K12WebhookCreateFreezesAuthenticatedOwner(t *testing.T) {
	srv := newWebhookTestServer(t)
	body := `{
		"name":"trusted-k12-owner-hook",
		"type":"k12",
		"agent_id":"kid-agent",
		"learner_id":"kid-learner",
		"allowed_events":["k12.submission.requested.v1"],
		"user_id":"forged-body-owner"
	}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/webhooks?user_id=forged-query-owner", strings.NewReader(body))
	req = req.WithContext(skill.WithAuthenticatedUser(context.Background(), "trusted-owner"))
	rec := httptest.NewRecorder()

	srv.handleRegisterWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	trusted, err := srv.webhookMgr.ListK12BindingsForAgent(
		context.Background(), "trusted-owner", "kid-agent",
	)
	if err != nil {
		t.Fatalf("list trusted owner bindings: %v", err)
	}
	if len(trusted) != 1 {
		t.Fatalf("trusted owner binding count=%d, want 1", len(trusted))
	}
	if trusted[0].CreatedBy != "trusted-owner" {
		t.Fatalf("binding owner=%q, want authenticated owner", trusted[0].CreatedBy)
	}

	for _, forgedOwner := range []string{"forged-query-owner", "forged-body-owner"} {
		bindings, listErr := srv.webhookMgr.ListK12BindingsForAgent(
			context.Background(), forgedOwner, "kid-agent",
		)
		if listErr != nil {
			t.Fatalf("list forged owner %q bindings: %v", forgedOwner, listErr)
		}
		if len(bindings) != 0 {
			t.Fatalf("forged owner %q binding count=%d, want 0", forgedOwner, len(bindings))
		}
	}
}

// TestBUG20260801003GenericWebhookPatchOwnerIsolation 钉死 PATCH 的 owner
// 不可枚举语义：跨 owner 与不存在统一 404，且拒绝路径不得改变任一状态源。
func TestBUG20260801003GenericWebhookPatchOwnerIsolation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		targetName string
	}{
		{name: "cross owner", targetName: "owner-a-hook"},
		{name: "missing", targetName: "missing-hook"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newWebhookOwnerAPIFixture(t)
			fixture.seed(t, "owner-a-hook", "owner-a", false)
			before := fixture.snapshot(t, "owner-a-hook", "missing-hook")

			req := authenticatedWebhookOwnerAPIRequest(
				http.MethodPatch, tt.targetName, "owner-b", `{"enabled":true}`,
			)
			rec := httptest.NewRecorder()
			fixture.srv.handleUpdateWebhook(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
			}
			requireWebhookOwnerAPISnapshot(t, before,
				fixture.snapshot(t, "owner-a-hook", "missing-hook"))
		})
	}
}

// TestBUG20260801003GenericWebhookPatchSameOwnerSucceeds 验证同 owner 可更新，
// Webhook 的数据库与内存镜像同步变化，任务授权保持不变。
func TestBUG20260801003GenericWebhookPatchSameOwnerSucceeds(t *testing.T) {
	fixture := newWebhookOwnerAPIFixture(t)
	taskRef := fixture.seed(t, "same-owner-patch", "owner-a", false)
	grantsBefore := append([]autonomy.Grant(nil), fixture.grants.ListActive(taskRef)...)

	req := authenticatedWebhookOwnerAPIRequest(
		http.MethodPatch, "same-owner-patch", "owner-a", `{"enabled":true}`,
	)
	rec := httptest.NewRecorder()
	fixture.srv.handleUpdateWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	after := fixture.snapshot(t, "same-owner-patch")
	if len(after.DBWebhooks) != 1 || !after.DBWebhooks[0].Enabled {
		t.Fatalf("database webhook state=%+v, want enabled", after.DBWebhooks)
	}
	if len(after.Memory) != 1 || !after.Memory[0].Exists || !after.Memory[0].Enabled {
		t.Fatalf("memory webhook state=%+v, want enabled", after.Memory)
	}
	if got := fixture.grants.ListActive(taskRef); !reflect.DeepEqual(grantsBefore, got) {
		t.Fatalf("PATCH changed grants\nbefore=%+v\nafter=%+v", grantsBefore, got)
	}
}

// TestBUG20260801003GenericWebhookDeleteOwnerIsolation 钉死 DELETE 的 owner
// 不可枚举语义：跨 owner 与不存在统一 404，且 Webhook 与授权均保持字节级状态等价。
func TestBUG20260801003GenericWebhookDeleteOwnerIsolation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		targetName string
	}{
		{name: "cross owner", targetName: "owner-a-hook"},
		{name: "missing", targetName: "missing-hook"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newWebhookOwnerAPIFixture(t)
			fixture.seed(t, "owner-a-hook", "owner-a", true)
			before := fixture.snapshot(t, "owner-a-hook", "missing-hook")

			req := authenticatedWebhookOwnerAPIRequest(http.MethodDelete, tt.targetName, "owner-b", "")
			rec := httptest.NewRecorder()
			fixture.srv.handleDeleteWebhook(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
			}
			requireWebhookOwnerAPISnapshot(t, before,
				fixture.snapshot(t, "owner-a-hook", "missing-hook"))
		})
	}
}

// TestBUG20260801003GenericWebhookDeleteSameOwnerSucceeds 验证同 owner 删除后，
// 数据库、内存路由与任务授权生命周期同时收口。
func TestBUG20260801003GenericWebhookDeleteSameOwnerSucceeds(t *testing.T) {
	fixture := newWebhookOwnerAPIFixture(t)
	taskRef := fixture.seed(t, "same-owner-delete", "owner-a", true)
	req := authenticatedWebhookOwnerAPIRequest(http.MethodDelete, "same-owner-delete", "owner-a", "")
	rec := httptest.NewRecorder()
	fixture.srv.handleDeleteWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	after := fixture.snapshot(t, "same-owner-delete")
	if len(after.DBWebhooks) != 0 || len(after.Memory) != 1 || after.Memory[0].Exists {
		t.Fatalf("deleted webhook remains: %+v", after)
	}
	if len(fixture.grants.ListActive(taskRef)) != 0 {
		t.Fatalf("active grants remain for deleted webhook: %+v", fixture.grants.ListActive(taskRef))
	}
	if len(after.DBGrants) != 1 || !after.DBGrants[0].Revoked {
		t.Fatalf("grant was not durably revoked: %+v", after.DBGrants)
	}
}

// TestBUG20260801003GenericWebhookManagementWithoutAuthPrefersQueryOwnerOverBody
// 保留直接嵌入调用兼容：无认证 principal 时 query user_id 优先于 body user_id。
func TestBUG20260801003GenericWebhookManagementWithoutAuthPrefersQueryOwnerOverBody(t *testing.T) {
	t.Run("patch", func(t *testing.T) {
		fixture := newWebhookOwnerAPIFixture(t)
		fixture.seed(t, "query-owner-patch", "query-owner", false)
		enabled := true
		req := webhookOwnerAPIRequest(
			http.MethodPatch, "query-owner-patch", "query-owner", "forged-body-owner", &enabled,
		)
		rec := httptest.NewRecorder()
		fixture.srv.handleUpdateWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want query owner PATCH success", rec.Code, rec.Body.String())
		}
	})

	t.Run("delete", func(t *testing.T) {
		fixture := newWebhookOwnerAPIFixture(t)
		fixture.seed(t, "query-owner-delete", "query-owner", true)
		req := webhookOwnerAPIRequest(
			http.MethodDelete, "query-owner-delete", "query-owner", "forged-body-owner", nil,
		)
		rec := httptest.NewRecorder()
		fixture.srv.handleDeleteWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want query owner DELETE success", rec.Code, rec.Body.String())
		}
	})
}
