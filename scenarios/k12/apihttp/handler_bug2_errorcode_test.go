package apihttp_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// gradeOKSolveFailExec：批改（带 student_answer）成功以便造出错题记录；纯解题（review/retry
// 走的 Solve，无 student_answer）返回错误，模拟下游 solver 执行失败。
type gradeOKSolveFailExec struct{}

func (gradeOKSolveFailExec) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	if _, grading := args["student_answer"]; grading {
		return &skill.Result{Metadata: map[string]string{
			"solve_verdict": "agree", "solve_evidence": "numeric_exec", "grade_correct": "false",
			"grade_wrong_step": "小数点错位", "grade_misconception": "对位错误",
		}}, nil
	}
	// 仅 review/retry 的「相似题」解题失败；grade 内部对原题的解题正常，以便先造出错题记录。
	problem, _ := args["problem"].(string)
	if strings.Contains(problem, "相似题") || strings.Contains(problem, "参照这道错题") {
		return nil, fmt.Errorf("上游解题服务不可用")
	}
	return &skill.Result{Content: "解：11.4", Metadata: map[string]string{
		"solve_verdict": "agree", "solve_evidence": "numeric_exec",
	}}, nil
}

func newServerWithSolver(t *testing.T, exec engineadapter.SolveExecutor, opts ...assembly.Option) http.Handler {
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
	allOptions := append(
		[]assembly.Option{
			assembly.WithAccumulationMetadataDeriver(fixedAccumulationMetadataDeriver{}),
		},
		opts...,
	)
	k, err := assembly.Wire(db, exec, allOptions...)
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps})
}

// memProfiles 是内存孩子档案存储（测试用），供 assembly.WithProfiles 注入年级确定性注入链路。
type memProfiles struct{ m map[string]k12.ChildProfile }

func (s *memProfiles) GetProfile(_ context.Context, agent string) (k12.ChildProfile, error) {
	p, ok := s.m[agent]
	if !ok {
		return k12.ChildProfile{}, fmt.Errorf("profile not found: %s", agent)
	}
	return p, nil
}

func (s *memProfiles) SaveProfile(_ context.Context, agent string, p k12.ChildProfile) error {
	s.m[agent] = p
	return nil
}

// BUG-2：mark-mastered 原来任意 err 一律 409。记录不存在应 404，不是版本冲突 409。
func TestBUG2_MarkMastered_NotFoundIs404(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, "POST", "/mark-mastered", `{"agent":"mingming","record_id":"does-not-exist","version":1}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("记录不存在应 404, got %d", rec.Code)
	}
}

// BUG-2：真实版本冲突仍应 409（分流后不回退）。
func TestBUG2_MarkMastered_VersionConflictIs409(t *testing.T) {
	h := newServer(t)
	body := `{"agent":"mingming","grade":"五年级上","source_session":"s1","problem":"3.8×3=?","student_answer":"10.4","knowledge_points":["小数乘法"]}`
	_, out := do(t, h, "POST", "/grade", body)
	rid, _ := out["record_id"].(string)
	if rid == "" {
		t.Fatalf("应造出错题记录, out=%v", out)
	}
	// version=99 与库中不符 → 乐观锁冲突。
	rec, _ := do(t, h, "POST", "/mark-mastered", fmt.Sprintf(`{"agent":"mingming","record_id":%q,"version":99}`, rid))
	if rec.Code != http.StatusConflict {
		t.Errorf("版本冲突应 409, got %d", rec.Code)
	}
}

// BUG-2：review/retry 记录不存在应 404，不是 400。
func TestBUG2_ReviewRetry_NotFoundIs404(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, "POST", "/review/retry", `{"agent":"mingming","record_id":"nope","grade":"五年级上"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("错题不存在应 404, got %d", rec.Code)
	}
}

// BUG-2：review/retry 下游 solver 执行失败应 502（对齐 recognize 用 502），不是 400。
func TestBUG2_ReviewRetry_SolverFailureIs502(t *testing.T) {
	h := newServerWithSolver(t, gradeOKSolveFailExec{})
	body := `{"agent":"mingming","grade":"五年级上","source_session":"s1","problem":"3.8×3=?","student_answer":"10.4","knowledge_points":["小数乘法"]}`
	_, out := do(t, h, "POST", "/grade", body)
	rid, _ := out["record_id"].(string)
	if rid == "" {
		t.Fatalf("应造出错题记录, out=%v", out)
	}
	rec, _ := do(t, h, "POST", "/review/retry", fmt.Sprintf(`{"agent":"mingming","record_id":%q,"grade":"五年级上"}`, rid))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("下游解题失败应 502, got %d", rec.Code)
	}
}

// BUG-2：restore 归档版本过新（ErrVersionUnsupported）应 409，与 checksum 不符的 400 区分。
func TestBUG2_Restore_VersionUnsupportedIs409(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, "POST", "/restore", `{"version":999,"agent_name":"mingming","records":[],"checksum":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("归档版本过新应 409, got %d", rec.Code)
	}
}

// BUG-2：restore checksum 不符仍应 400（分流后不误判为 409）。
func TestBUG2_Restore_ChecksumMismatchIs400(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, "POST", "/restore", `{"version":1,"agent_name":"mingming","records":[],"checksum":"bogus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("checksum 不符应 400, got %d", rec.Code)
	}
}
