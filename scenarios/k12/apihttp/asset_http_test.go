package apihttp_test

// 资产服务 HTTP 契约（RED 先行）——POST /assets（multipart 或 base64 JSON）+ GET /assets/{file}：
//   1. multipart 上传合法图片 → asset_id（asset://<agent>/<sha256>.<ext>）；
//   2. base64 JSON 上传等价；同内容幂等同 id；
//   3. 魔数校验：非图片 → 415；大小上限：>10MB → 413；
//   4. 归属隔离：GET 必带 agent，跨 agent / 穿越名 → 404；
//   5. GET 回图：Content-Type 正确、字节一致。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
