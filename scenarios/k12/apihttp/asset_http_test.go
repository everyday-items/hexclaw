package apihttp_test

// 资产服务 HTTP 契约（RED 先行）——POST /assets（multipart 或 base64 JSON）+ GET /assets/{file}：
//   1. multipart 上传合法图片 → asset_id（asset://<agent>/<sha256>.<ext>）；
//   2. base64 JSON 上传等价；同内容幂等同 id；
//   3. 魔数校验：非图片 → 415；大小上限：>10MB → 413；
//   4. 归属隔离：GET 必带 agent，跨 agent / 穿越名 → 404；
//   5. GET 回图：Content-Type 正确、字节一致。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type assetGatewayErrorStub struct {
	persistErr error
	openErr    error
}

func (s assetGatewayErrorStub) Persist(
	context.Context, string, string, []byte,
) (usecase.ReadyPageAsset, error) {
	return usecase.ReadyPageAsset{}, s.persistErr
}

func (s assetGatewayErrorStub) OpenReady(
	context.Context, string, string, string,
) (usecase.ReadyPageAsset, error) {
	return usecase.ReadyPageAsset{}, s.openErr
}

const tinyPNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func tinyPNGBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(tinyPNGB64)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func postMultipartAsset(t *testing.T, h http.Handler, agent, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", "/assets?agent="+agent, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAssetHTTP_MultipartUploadAndGet(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	h := newServer(t)
	png := tinyPNGBytes(t)

	rec := postMultipartAsset(t, h, "mingming", "作品.png", png)
	if rec.Code != http.StatusOK {
		t.Fatalf("multipart 上传应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	id, _ := out["asset_id"].(string)
	if !strings.HasPrefix(id, "asset://mingming/") {
		t.Fatalf("asset_id 应带归属前缀, got %q", id)
	}

	// GET 回图：文件段取自 id，agent 必带。
	file := id[strings.LastIndex(id, "/")+1:]
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, httptest.NewRequest("GET", "/assets/"+file+"?agent=mingming", nil))
	if getRec.Code != http.StatusOK || !bytes.Equal(getRec.Body.Bytes(), png) {
		t.Fatalf("GET 回图失败: code=%d", getRec.Code)
	}
	if ct := getRec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type 应为 image/png, got %q", ct)
	}

	// 归属隔离：他人 agent 读 → 404；缺 agent → 400。
	crossRec := httptest.NewRecorder()
	h.ServeHTTP(crossRec, httptest.NewRequest("GET", "/assets/"+file+"?agent=honghong", nil))
	if crossRec.Code != http.StatusNotFound {
		t.Fatalf("跨 agent 读取应 404, got %d", crossRec.Code)
	}
	noAgentRec := httptest.NewRecorder()
	h.ServeHTTP(noAgentRec, httptest.NewRequest("GET", "/assets/"+file, nil))
	if noAgentRec.Code != http.StatusBadRequest {
		t.Fatalf("缺 agent 应 400, got %d", noAgentRec.Code)
	}
}

func TestAssetHTTP_Base64JSONUploadIdempotent(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	h := newServer(t)
	body := fmt.Sprintf(`{"agent":"mingming","data_base64":%q}`, tinyPNGB64)
	rec1, out1 := do(t, h, "POST", "/assets", body)
	rec2, out2 := do(t, h, "POST", "/assets", body)
	if rec1.Code != http.StatusOK || rec2.Code != http.StatusOK {
		t.Fatalf("base64 上传应 200: %d/%d %v", rec1.Code, rec2.Code, out1)
	}
	if out1["asset_id"] != out2["asset_id"] {
		t.Fatalf("同内容重复上传应幂等同 id: %v vs %v", out1["asset_id"], out2["asset_id"])
	}
}

func TestAssetHTTP_RejectsNonImage(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	h := newServer(t)
	rec := postMultipartAsset(t, h, "mingming", "evil.png", []byte("not an image at all"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非图片魔数应 415, got %d", rec.Code)
	}
}

func TestAssetHTTP_RejectsOversize(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	h := newServer(t)
	big := make([]byte, 10<<20+1024)
	copy(big, tinyPNGBytes(t))
	rec := postMultipartAsset(t, h, "mingming", "big.png", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf(">10MB 应 413, got %d", rec.Code)
	}
}

func TestAssetHTTP_TraversalRejected(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/..%2fsecret.png?agent=mingming", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("穿越文件名不得 200, got %d", rec.Code)
	}
}

func TestPROG026G_AssetHTTPBindsAuthenticatedOwnerAgentAndReadyMetadata(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	fixture := newImageTaskHTTPFixture(t)
	remoteHandler := func(
		owner string,
		authorize func(context.Context, string, string) error,
	) http.Handler {
		return apihttp.NewHandler(apihttp.Runtime{
			Records:       fixture.coordinator.Records,
			PrincipalMode: "remote",
			AuthenticatedOwnerScope: func(context.Context) (string, error) {
				return owner, nil
			},
			AuthorizeAgentScope: authorize,
		})
	}
	authorizeGuardian := func(_ context.Context, owner, agent string) error {
		if owner != "guardian-1" || agent != "mingming" {
			return fmt.Errorf("unexpected scope %q -> %q", owner, agent)
		}
		return nil
	}

	upload := postMultipartAsset(
		t,
		remoteHandler("guardian-1", authorizeGuardian),
		"mingming",
		"page.png",
		tinyPNGBytes(t),
	)
	if upload.Code != http.StatusOK {
		t.Fatalf("authorized PageAsset upload=%d body=%s", upload.Code, upload.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(upload.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	assetID, _ := body["asset_id"].(string)
	var ownerScope, agentName, state, mediaType, orientation string
	var width, height int
	if err := fixture.db.QueryRow(`
		SELECT owner_scope,agent_name,storage_state,media_type,
		       orientation_policy,pixel_width,pixel_height
		FROM k12_page_assets
		WHERE owner_scope='guardian-1' AND page_asset_id=?`,
		assetID,
	).Scan(
		&ownerScope,
		&agentName,
		&state,
		&mediaType,
		&orientation,
		&width,
		&height,
	); err != nil {
		t.Fatalf("ready PageAsset metadata missing: %v", err)
	}
	if ownerScope != "guardian-1" || agentName != "mingming" ||
		state != "ready" || mediaType != "image/png" || orientation != "verified" ||
		width != 1 || height != 1 {
		t.Fatalf(
			"PageAsset metadata drift: owner=%q agent=%q state=%q mime=%q orientation=%q dims=%dx%d",
			ownerScope,
			agentName,
			state,
			mediaType,
			orientation,
			width,
			height,
		)
	}

	file := assetID[strings.LastIndex(assetID, "/")+1:]
	get := httptest.NewRecorder()
	remoteHandler("guardian-1", authorizeGuardian).ServeHTTP(
		get,
		httptest.NewRequest("GET", "/assets/"+file+"?agent=mingming", nil),
	)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), tinyPNGBytes(t)) {
		t.Fatalf("owner-scoped ready PageAsset GET=%d", get.Code)
	}

	deny := func(context.Context, string, string) error { return fmt.Errorf("denied") }
	for name, handler := range map[string]http.Handler{
		"denied owner":       remoteHandler("attacker", deny),
		"missing authorizer": remoteHandler("guardian-1", nil),
	} {
		t.Run(name, func(t *testing.T) {
			rec := postMultipartAsset(t, handler, "mingming", "page.png", tinyPNGBytes(t))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("unauthorized upload=%d want 404 body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPROG026G_AssetHTTPHidesExistenceButSurfacesIntegrityAndInfrastructureAsServerErrors(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	newHandler := func(gateway usecase.PageAssetGateway) http.Handler {
		return apihttp.NewHandler(apihttp.Runtime{
			Records: fixture.coordinator.Records, PageAssets: gateway,
		})
	}
	const internalDetail = "sqlite disk I/O failure /Users/private/database.db"
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "owner scoped absence", err: k12storage.ErrPageAssetNotFound, want: http.StatusNotFound},
		{name: "ready bytes integrity drift", err: usecase.ErrPageAssetIntegrity, want: http.StatusInternalServerError},
		{name: "storage infrastructure", err: errors.New(internalDetail), want: http.StatusInternalServerError},
	} {
		t.Run("get "+tc.name, func(t *testing.T) {
			handler := newHandler(assetGatewayErrorStub{openErr: tc.err})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(
				http.MethodGet,
				"/assets/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png?agent=mingming",
				nil,
			))
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), internalDetail) ||
				strings.Contains(rec.Body.String(), "/Users/private") {
				t.Fatalf("GET leaked internal error: %s", rec.Body.String())
			}
		})
	}

	handler := newHandler(assetGatewayErrorStub{persistErr: errors.New(internalDetail)})
	rec := postMultipartAsset(t, handler, "mingming", "page.png", tinyPNGBytes(t))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("upload infrastructure status=%d want=500 body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), internalDetail) ||
		strings.Contains(rec.Body.String(), "/Users/private") {
		t.Fatalf("upload leaked internal error: %s", rec.Body.String())
	}
}
