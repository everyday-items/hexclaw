package apihttp_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// subjectGroundingWriterSpy 支持分科写入的 grounding spy。
type subjectGroundingWriterSpy struct {
	agent, subject, title string
	legacy                bool
}

func (s *subjectGroundingWriterSpy) Ground(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (s *subjectGroundingWriterSpy) AddGrounding(_ context.Context, agent, title, _ string) error {
	s.agent, s.title, s.legacy = agent, title, true
	return nil
}

func (s *subjectGroundingWriterSpy) AddGroundingSubject(_ context.Context, agent, subject, title, _ string) error {
	s.agent, s.subject, s.title = agent, subject, title
	return nil
}

// TestGroundingUpload_SubjectRoutedToScopedWriter POST /grounding 带 subject：
// 学科透传到分科写侧。
func TestGroundingUpload_SubjectRoutedToScopedWriter(t *testing.T) {
	spy := &subjectGroundingWriterSpy{}
	h := apihttp.NewHandler(apihttp.Runtime{Deps: usecase.Deps{Grounding: spy}})
	rec, _ := do(t, h, http.MethodPost, "/grounding",
		`{"agent":"mingming","subject":"数学","title":"数学五上","content":"小数乘法教材讲法"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("分科上传 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.agent != "mingming" || spy.subject != "数学" || spy.legacy {
		t.Fatalf("学科未透传写侧: %+v", spy)
	}
}

// TestGroundingUpload_InvalidSubjectRejected 非六学科 → 400。
func TestGroundingUpload_InvalidSubjectRejected(t *testing.T) {
	spy := &subjectGroundingWriterSpy{}
	h := apihttp.NewHandler(apihttp.Runtime{Deps: usecase.Deps{Grounding: spy}})
	rec, _ := do(t, h, http.MethodPost, "/grounding",
		`{"agent":"mingming","subject":"体育","title":"t","content":"c"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法学科 status=%d want 400", rec.Code)
	}
	if spy.subject != "" || spy.legacy {
		t.Fatalf("非法学科不得写入: %+v", spy)
	}
}

// subjectGroundSpy 记录备课卡检索侧收到的学科。
type subjectGroundSpy struct{ queried []string }

func (s *subjectGroundSpy) Ground(context.Context, string, string, string) (string, bool, error) {
	return "通用讲法", true, nil
}

func (s *subjectGroundSpy) GroundSubject(_ context.Context, _, subject, _, _ string) (string, bool, error) {
	s.queried = append(s.queried, subject)
	return "数学教材讲法", true, nil
}

// TestPrepCard_SubjectThreadedToGrounding POST /prep-card 带 subject：
// 注入侧按当前题目学科检索本学科教材。
func TestPrepCard_SubjectThreadedToGrounding(t *testing.T) {
	spy := &subjectGroundSpy{}
	h := newServerWithSolver(t, fakeSolveExec{}, assembly.WithGrounding(spy))
	rec, out := do(t, h, http.MethodPost, "/prep-card",
		`{"agent":"mingming","grade":"五年级上","subject":"数学","knowledge_points":["小数乘法"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("prep-card status=%d %v", rec.Code, out)
	}
	if len(spy.queried) != 1 || spy.queried[0] != "数学" {
		t.Fatalf("学科应下推检索侧: %v", spy.queried)
	}
}

// TestGroundingUpload_NoSubjectLegacySemantics 不带 subject 的老请求走不分科写入
// （前向兼容，字段可缺省）。
func TestGroundingUpload_NoSubjectLegacySemantics(t *testing.T) {
	spy := &subjectGroundingWriterSpy{}
	h := apihttp.NewHandler(apihttp.Runtime{Deps: usecase.Deps{Grounding: spy}})
	rec, _ := do(t, h, http.MethodPost, "/grounding",
		`{"agent":"mingming","title":"人教版五上","content":"小数乘法教材讲法"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("旧请求 status=%d", rec.Code)
	}
	if !spy.legacy || spy.subject != "" {
		t.Fatalf("旧请求应走不分科写入: %+v", spy)
	}
}
