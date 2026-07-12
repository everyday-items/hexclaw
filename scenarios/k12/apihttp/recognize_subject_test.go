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

// fakeRecognizer 桩识题：逐题回填自动判定的学科（Polish-2）。
type fakeRecognizer struct{}

func (fakeRecognizer) Recognize(context.Context, []byte) ([]usecase.RecognizedQuestion, error) {
	return []usecase.RecognizedQuestion{
		{Question: "3.8×3=?", Subject: "数学", KnowledgePoints: []string{"小数乘法"}},
		{Question: "简算 25×4", Subject: "数学", KnowledgePoints: []string{"乘法结合律"}},
		{Question: "翻译 apple", Subject: "英语", KnowledgePoints: []string{"单词"}},
	}, nil
}

func newServerWithRecognizer(t *testing.T) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	k, err := assembly.Wire(db, fakeSolveExec{}, assembly.WithRecognizer(fakeRecognizer{}))
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps})
}

// TestRecognizeReturnsSubject Polish-2 ①：识题响应带**逐题学科**与**整卷学科**——家长不必手选，
// 前端据顶层 subject 预填学科下拉。RED 若响应缺 subject 字段则前端无从自动选中、按钮持续 gate 空学科。
func TestRecognizeReturnsSubject(t *testing.T) {
	h := newServerWithRecognizer(t)
	img := base64.StdEncoding.EncodeToString([]byte("fake-image-bytes"))
	rec, out := do(t, h, http.MethodPost, "/recognize", `{"image_base64":"`+img+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("recognize 应 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	// 顶层整卷学科：3 题里数学 2、英语 1 → 多数取数学。
	if got, _ := out["subject"].(string); got != "数学" {
		t.Errorf("整卷学科应为多数科 数学，got %q", got)
	}
	qs, ok := out["questions"].([]any)
	if !ok || len(qs) != 3 {
		t.Fatalf("应含 3 题, got %v", out["questions"])
	}
	q0, _ := qs[0].(map[string]any)
	if got, _ := q0["subject"].(string); got != "数学" {
		t.Errorf("第 1 题 subject 应为 数学，got %q", got)
	}
	q2, _ := qs[2].(map[string]any)
	if got, _ := q2["subject"].(string); got != "英语" {
		t.Errorf("第 3 题 subject 应为 英语，got %q", got)
	}
}
