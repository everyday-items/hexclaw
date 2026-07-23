package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// subjectGroundingSpy 实现 Grounding + SubjectGrounding + GroundingWriter +
// SubjectGroundingWriter，记录用例传下来的学科。
type subjectGroundingSpy struct {
	wroteSubject  string
	wroteLegacy   bool
	queried       []string // GroundSubject 收到的 subject 序列
	legacyQueried int
	text          string
}

func (s *subjectGroundingSpy) Ground(context.Context, string, string, string) (string, bool, error) {
	s.legacyQueried++
	return s.text, s.text != "", nil
}

func (s *subjectGroundingSpy) GroundSubject(_ context.Context, _, subject, _, _ string) (string, bool, error) {
	s.queried = append(s.queried, subject)
	return s.text, s.text != "", nil
}

func (s *subjectGroundingSpy) AddGrounding(_ context.Context, _, _, _ string) error {
	s.wroteLegacy = true
	return nil
}

func (s *subjectGroundingSpy) AddGroundingSubject(_ context.Context, _, subject, _, _ string) error {
	s.wroteSubject = subject
	return nil
}

// legacyGroundingSpy 只实现旧接口（无分科能力）。
type legacyGroundingSpy struct{ wrote bool }

func (s *legacyGroundingSpy) Ground(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}
func (s *legacyGroundingSpy) AddGrounding(context.Context, string, string, string) error {
	s.wrote = true
	return nil
}

// TestAddGrounding_SubjectValidation 六学科枚举校验；空 = 不分科旧语义。
func TestAddGrounding_SubjectValidation(t *testing.T) {
	spy := &subjectGroundingSpy{}
	d := usecase.Deps{Grounding: spy}
	ctx := context.Background()

	for _, subject := range []string{"数学", "语文", "英语", "科学", "信息科技", "美术"} {
		if err := d.AddGrounding(ctx, "mingming", subject, "教材", "内容"); err != nil {
			t.Fatalf("学科 %s 应可绑定: %v", subject, err)
		}
		if spy.wroteSubject != subject {
			t.Fatalf("学科应透传到写侧: want %s got %s", subject, spy.wroteSubject)
		}
	}

	for _, bad := range []string{"体育", "物理", "math", "美 术"} {
		err := d.AddGrounding(ctx, "mingming", bad, "教材", "内容")
		if err == nil || !errors.Is(err, usecase.ErrInvalidInput) {
			t.Fatalf("非法学科 %q 应拒: %v", bad, err)
		}
	}

	// 空学科 = 旧语义，走不分科写入。
	if err := d.AddGrounding(ctx, "mingming", "", "教材", "内容"); err != nil {
		t.Fatalf("空学科应前向兼容: %v", err)
	}
	if !spy.wroteLegacy {
		t.Fatal("空学科应走不分科写入路径")
	}
}

// TestAddGrounding_SubjectWithoutCapabilityRejected 写侧不支持分科时，带学科的请求
// 必须报错，不得静默丢掉学科降级为不分科。
func TestAddGrounding_SubjectWithoutCapabilityRejected(t *testing.T) {
	spy := &legacyGroundingSpy{}
	d := usecase.Deps{Grounding: spy}
	if err := d.AddGrounding(context.Background(), "mingming", "数学", "教材", "内容"); err == nil {
		t.Fatal("写侧无分科能力时带学科写入应报错")
	}
	if spy.wrote {
		t.Fatal("不得静默降级为不分科写入")
	}
	// 不带学科仍可写（旧语义不受影响）。
	if err := d.AddGrounding(context.Background(), "mingming", "", "教材", "内容"); err != nil {
		t.Fatalf("旧语义写入应可用: %v", err)
	}
}
