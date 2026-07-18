package usecase_test

import (
	"context"
	"errors"
	"strings"
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

// TestBuildPrepCardSubject_PrefersSubjectTextbook 注入侧契约：备课卡按当前题目学科
// 检索本学科教材（数学题带数学 subject 下推，不再裸查全部）。
func TestBuildPrepCardSubject_PrefersSubjectTextbook(t *testing.T) {
	d := newDataDeps(t)
	spy := &subjectGroundingSpy{text: "小数乘法竖式教材讲法"}
	d.Grounding = spy
	ctx := context.Background()

	card, err := d.BuildPrepCardSubject(ctx, "xiaoming", "五年级上", "数学", []string{"小数乘法"})
	if err != nil {
		t.Fatalf("BuildPrepCardSubject: %v", err)
	}
	if len(spy.queried) != 1 || spy.queried[0] != "数学" {
		t.Fatalf("检索应携带学科 数学, got %v (legacy=%d)", spy.queried, spy.legacyQueried)
	}
	if !strings.Contains(card.Sections[0].Content, "小数乘法竖式教材讲法") ||
		card.Sections[0].SourceLabel != usecase.SrcTextbook {
		t.Fatalf("①段应命中教材: %+v", card.Sections[0])
	}

	// 非法学科从紧拒绝。
	if _, err := d.BuildPrepCardSubject(ctx, "xiaoming", "五年级上", "体育", []string{"x"}); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("非法学科应拒: %v", err)
	}
}

// TestBuildPrepCard_LegacyNoSubjectCompatible 旧入口不带学科：分科读侧收到空 subject
// （不分科旧语义），老 adapter（无分科能力）仍走旧 Ground。
func TestBuildPrepCard_LegacyNoSubjectCompatible(t *testing.T) {
	d := newDataDeps(t)
	spy := &subjectGroundingSpy{text: "通用教材讲法"}
	d.Grounding = spy
	ctx := context.Background()
	if _, err := d.BuildPrepCard(ctx, "xiaoming", "五年级上", []string{"小数乘法"}); err != nil {
		t.Fatalf("BuildPrepCard: %v", err)
	}
	if len(spy.queried) != 1 || spy.queried[0] != "" {
		t.Fatalf("旧入口应以空学科检索, got %v", spy.queried)
	}

	legacy := &legacyGroundingSpy{}
	d.Grounding = legacy
	if _, err := d.BuildPrepCardSubject(ctx, "xiaoming", "五年级上", "数学", []string{"小数乘法"}); err != nil {
		t.Fatalf("老 adapter 兼容: %v", err)
	}
}
