package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// bboxRecognizer 桩识题：一题带合法 bbox（已答题，可叠加），一题无 bbox（降级纯文字批改）。
type bboxRecognizer struct{}

func (bboxRecognizer) Recognize(context.Context, []byte) ([]usecase.RecognizedQuestion, error) {
	return []usecase.RecognizedQuestion{
		{Question: "3.8×3=?", Subject: "数学", KnowledgePoints: []string{"小数乘法"}, StudentAnswer: "10.4",
			BBox: &usecase.BBox{X: 0.12, Y: 0.34, W: 0.18, H: 0.05}},
		{Question: "简算 25×4", Subject: "数学", KnowledgePoints: []string{"乘法结合律"}, StudentAnswer: "100"},
	}, nil
}

func newServerWithBBoxRecognizer(t *testing.T) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	k, err := assembly.Wire(db, fakeSolveExec{}, assembly.WithRecognizer(bboxRecognizer{}))
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps})
}

// TestRecognizeReturnsBBox 原图批改 Phase 1 契约：识题响应逐题下发 bbox（合法框带出、缺失=null）。
// RED 若响应缺 bbox 字段，前端无从在原图上定位标记，叠加链断。
func TestRecognizeReturnsBBox(t *testing.T) {
	h := newServerWithBBoxRecognizer(t)
	img := base64.StdEncoding.EncodeToString([]byte("fake-image-bytes"))
	rec, out := do(t, h, http.MethodPost, "/recognize", `{"image_base64":"`+img+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("recognize 应 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	qs, ok := out["questions"].([]any)
	if !ok || len(qs) != 2 {
		t.Fatalf("应含 2 题, got %v", out["questions"])
	}
	// 第 1 题：合法 bbox 应下发
	q0, _ := qs[0].(map[string]any)
	bbox, ok := q0["bbox"].(map[string]any)
	if !ok {
		t.Fatalf("第 1 题应带 bbox, got %v", q0["bbox"])
	}
	if bbox["x"].(float64) != 0.12 || bbox["y"].(float64) != 0.34 ||
		bbox["w"].(float64) != 0.18 || bbox["h"].(float64) != 0.05 {
		t.Errorf("bbox 应原样下发, got %v", bbox)
	}
	// 第 2 题：无 bbox → 字段缺失/null（前端据此降级纯文字批改）
	q1, _ := qs[1].(map[string]any)
	if b := q1["bbox"]; b != nil {
		t.Errorf("无 bbox 的题应为 null/缺失（降级），got %v", b)
	}
}
