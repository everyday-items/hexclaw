package apihttp_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

func TestJSONBodyLimit_Returns413(t *testing.T) {
	h := newServer(t)
	body := `{"agent":"mingming","problem":"` + strings.Repeat("x", 2<<20) + `"}`
	rec, _ := do(t, h, http.MethodPost, "/grade", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d want 413", rec.Code)
	}
}

func TestAddAccumulation_InternalStorageFailureIs500(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		t.Fatal(err)
	}
	store := k12storage.NewStore(db, reg.Records)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	h := apihttp.NewHandler(apihttp.Runtime{Deps: usecase.Deps{
		Records:              store,
		AccumulationMetadata: fixedAccumulationMetadataDeriver{},
	}})
	rec, _ := doCurrent(
		t,
		h,
		http.MethodPost,
		"/accumulation?agent=mingming",
		`{"content":"believe"}`,
		map[string]string{"Idempotency-Key": "storage-failure"},
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("accumulation storage failure status=%d want 500", rec.Code)
	}
}

type failingProfileStore struct{}

func (failingProfileStore) GetProfile(context.Context, string) (k12.ChildProfile, error) {
	return k12.ChildProfile{}, errors.New("disk unavailable")
}

type groundingWriterSpy struct{ agent, title, content string }

func (s *groundingWriterSpy) Ground(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}
func (s *groundingWriterSpy) AddGrounding(_ context.Context, agent, title, content string) error {
	s.agent, s.title, s.content = agent, title, content
	return nil
}

func TestGroundingUpload_RoutesThroughScopedWriter(t *testing.T) {
	spy := &groundingWriterSpy{}
	h := apihttp.NewHandler(apihttp.Runtime{Deps: usecase.Deps{Grounding: spy}})
	rec, _ := do(t, h, http.MethodPost, "/grounding", `{"agent":"mingming","title":"人教版五上","content":"小数乘法教材讲法"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("grounding upload status=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.agent != "mingming" || spy.title == "" || spy.content == "" {
		t.Fatalf("writer args=%+v", spy)
	}
}
func (failingProfileStore) SaveProfile(context.Context, string, k12.ChildProfile) error {
	return errors.New("disk unavailable")
}

func TestUpdateProfile_LegacyRouteRejectedBeforeStorage(t *testing.T) {
	h := apihttp.NewHandler(apihttp.Runtime{Deps: usecase.Deps{Profiles: failingProfileStore{}}})
	rec, _ := do(t, h, http.MethodPut, "/profile", `{"agent":"mingming","grade_term":"五年级上"}`)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("legacy profile update status=%d want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("legacy profile update Allow=%q want GET", got)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"profile updates require /api/k12/profile-bundle"}` {
		t.Fatalf("legacy profile update body=%s", got)
	}
}

func TestMarkMastered_RequiresMatchingAgentScope(t *testing.T) {
	h := newServer(t)
	_, out := do(t, h, http.MethodPost, "/grade", `{"agent":"mingming","grade":"五年级上","source_session":"scope","problem":"3.8x3","student_answer":"10.4","knowledge_points":["小数乘法"]}`)
	rid, _ := out["record_id"].(string)
	if rid == "" {
		t.Fatalf("seed record failed: %v", out)
	}
	rec, _ := do(t, h, http.MethodPost, "/mark-mastered", fmt.Sprintf(`{"agent":"other-child","record_id":%q,"version":0}`, rid))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-agent mark status=%d want 404", rec.Code)
	}
	// The owner can still update the unchanged version, proving the cross-agent request had no side effect.
	rec, _ = do(t, h, http.MethodPost, "/mark-mastered", fmt.Sprintf(`{"agent":"mingming","record_id":%q,"version":0}`, rid))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner mark status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type alwaysFailExec struct{}

func (alwaysFailExec) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return nil, errors.New("provider down")
}

func TestGrade_InternalFailureIsNotClient400(t *testing.T) {
	h := newServerWithSolver(t, alwaysFailExec{})
	rec, _ := do(t, h, http.MethodPost, "/grade", `{"agent":"mingming","grade":"五年级上","problem":"1+1"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("provider failure status=%d want 502", rec.Code)
	}
}

func TestGrade_InvalidGradeIs400(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, http.MethodPost, "/grade", `{"agent":"mingming","grade":"大学","problem":"1+1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid grade status=%d want 400", rec.Code)
	}
}
