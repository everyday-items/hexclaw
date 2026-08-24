package apihttp_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

func newRuntimeWithSolver(
	t *testing.T,
	exec engineadapter.SolveExecutor,
	opts ...assembly.Option,
) apihttp.Runtime {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Async K12 workers must observe the same in-memory SQLite database.
	// A plain ":memory:" DSN creates one database per connection.
	db.SetMaxOpenConns(1)
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
	k.Deps.WorkFeedbackRoute = func(
		context.Context,
		string,
	) (k12.ImageTaskRouteSnapshot, error) {
		return k12.ImageTaskRouteSnapshot{
			Provider: "test-provider", Model: "test-model",
			Route: "test-provider/test-model", Capability: "text",
			SelectionSource: "auto", PolicyVersion: "test-work-feedback",
			PromptVersion: "writing-feedback-v1",
		}, nil
	}
	return apihttp.Runtime{
		Views: k.Registry.Views, Records: k.Records, Deps: k.Deps,
	}
}

func newServerWithSolver(t *testing.T, exec engineadapter.SolveExecutor, opts ...assembly.Option) http.Handler {
	t.Helper()
	runtime := newRuntimeWithSolver(t, exec, opts...)
	runtime.PracticeGeneration = &usecase.SinglePracticeGenerationCoordinator{
		Deps: &runtime.Deps, Records: runtime.Records,
		BaseContext: context.Background(),
	}
	return apihttp.NewHandler(runtime)
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
