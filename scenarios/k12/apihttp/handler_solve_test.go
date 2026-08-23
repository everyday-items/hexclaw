package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// faithfulSolveExec 忠实复刻 engine/solve.go 的 gradingMode 契约：只有 student_answer
// **非空** 才进批改模式产出 grade_correct；空白时只解题、无 grade_correct（治本前的 502 根因）。
// 与 handler_test.go 的 fakeSolveExec（按 key 存在与否判定）不同——那个掩盖了空白卷根因。
type faithfulSolveExec struct{}

func (faithfulSolveExec) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	sa, _ := args["student_answer"].(string)
	if strings.TrimSpace(sa) != "" { // gradingMode
		return &skill.Result{Metadata: map[string]string{
			"solve_verdict": "agree", "solve_evidence": "numeric_exec", "grade_correct": "false",
			"grade_wrong_step": "小数点错位", "grade_misconception": "对位错误",
		}}, nil
	}
	// 空白：solve 模式，仅解法（无 grade_correct）——旧路径在此让 adapter 报 missing grade_correct → 502。
	return &skill.Result{Content: "解：3.8×3=11.4", Metadata: map[string]string{
		"solve_verdict": "agree", "solve_evidence": "numeric_exec",
	}}, nil
}

func newFaithfulServer(t *testing.T, seededProfiles ...k12.ChildProfile) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	metadata := "{}"
	if len(seededProfiles) > 0 {
		encoded, marshalErr := json.Marshal(k12.ApplyProfileToMeta(nil, seededProfiles[0]))
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		metadata = string(encoded)
	}
	if _, err := db.Exec(`INSERT INTO agents(name,metadata) VALUES('mingming',?)`, metadata); err != nil {
		t.Fatal(err)
	}
	k, err := assembly.Wire(db, faithfulSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps})
}

// TestSolveEndpoint 新 /solve 端点契约：空白题求解，返回解法+徽章，不入错题本。
func TestSolveEndpoint(t *testing.T) {
	h := newFaithfulServer(t)
	rec, out := do(t, h, "POST", "/solve",
		`{"agent":"mingming","grade":"五年级上","problem":"3.8 × 3 = ?","knowledge_points":["小数乘法"]}`)
	if rec.Code != 200 {
		t.Fatalf("/solve 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if s, _ := out["solution"].(string); s == "" {
		t.Errorf("应返回解法, got %v", out)
	}
	if b, _ := out["badge"].(string); b == "" {
		t.Error("应返回徽章")
	}
	// 未入库
	_, mist := do(t, h, "GET", "/mistakes?agent=mingming", "")
	if items, _ := mist["items"].([]any); len(items) != 0 {
		t.Errorf("/solve 不应入错题本, got %d", len(items))
	}
}

// TestGradeBlankAnswer_NoLonger502 治本命脉：空白 student_answer 打 /grade 不再 502，
// 内部转解题分叉返回 200 + solve_only。RED（治本前）：adapter 缺 grade_correct → 502。
func TestGradeBlankAnswer_NoLonger502(t *testing.T) {
	h := newFaithfulServer(t)
	rec, out := do(t, h, "POST", "/grade",
		`{"agent":"mingming","grade":"五年级上","problem":"3.8 × 3 = ?","student_answer":"","knowledge_points":["小数乘法"]}`)
	if rec.Code != 200 {
		t.Fatalf("空白卷 /grade 应 200（内部转 solve），got %d body=%s", rec.Code, rec.Body.String())
	}
	if so, _ := out["solve_only"].(bool); !so {
		t.Errorf("空白卷 /grade 应标 solve_only=true, got %v", out)
	}
	if s, _ := out["solution"].(string); s == "" {
		t.Error("空白卷 /grade 应给解法")
	}
	if rc, _ := out["record_created"].(bool); rc {
		t.Error("空白卷 /grade 不应入错题本")
	}
}

// TestGradeAnswered_StillGrades 已答题打 /grade 仍批改（回归保护）。
func TestGradeAnswered_StillGrades(t *testing.T) {
	h := newFaithfulServer(t)
	rec, out := do(t, h, "POST", "/grade",
		`{"agent":"mingming","grade":"五年级上","problem":"3.8 × 3 = ?","student_answer":"10.4","knowledge_points":["小数乘法"]}`)
	if rec.Code != 200 {
		t.Fatalf("已答 /grade 应 200, got %d", rec.Code)
	}
	if so, _ := out["solve_only"].(bool); so {
		t.Error("已答题不应标 solve_only")
	}
	if rc, _ := out["record_created"].(bool); !rc {
		t.Error("已答判错应入错题本")
	}
}
