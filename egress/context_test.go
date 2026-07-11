package egress

import (
	"context"
	"testing"
)

func TestContextGuardMissingEnvelopeFailsClosed(t *testing.T) {
	if err := (&Policy{}).GuardContext(context.Background()); err == nil {
		t.Fatal("云调用缺少 purpose/data-class envelope 必须 fail-closed")
	}
}

func TestContextEnvelopeAccumulatesAndDeduplicatesClasses(t *testing.T) {
	ctx := WithRequest(context.Background(), PurposeGeneralChat, "audit-1", ClassGeneral)
	ctx = AddDataClasses(ctx, ClassDocument, ClassDocument)

	var audited []Request
	p := &Policy{OnAudit: func(req Request, _ Decision) { audited = append(audited, req) }}
	if err := p.GuardContext(ctx); err != nil {
		t.Fatalf("known non-sensitive classes should pass: %v", err)
	}
	if len(audited) != 2 {
		t.Fatalf("deduplicated general+document should audit twice, got=%+v", audited)
	}
	for _, req := range audited {
		if req.Purpose != PurposeGeneralChat || req.AuditID != "audit-1" {
			t.Fatalf("envelope metadata lost: %+v", req)
		}
	}
}

func TestContextEnvelopeSensitiveClassIsEnforced(t *testing.T) {
	ctx := WithRequest(context.Background(), PurposeGeneralChat, "", ClassGeneral)
	ctx = AddDataClasses(ctx, ClassMemory)
	if err := (&Policy{}).GuardContext(ctx); err == nil {
		t.Fatal("general chat carrying memory must be denied for cloud")
	}
}

func TestWithRequestCreatesIsolatedChildEnvelope(t *testing.T) {
	parent := WithRequest(context.Background(), PurposeGeneralChat, "parent", ClassMemory)
	child := WithRequest(parent, PurposeSolveVerify, "child", ClassGeneral, ClassSensitiveProfile)

	p := &Policy{}
	if err := p.GuardContext(child); err != nil {
		t.Fatalf("solve child must not inherit parent's general-chat memory class: %v", err)
	}
	if err := p.GuardContext(parent); err == nil {
		t.Fatal("parent envelope must remain independently denied")
	}
}

func TestContextGuardAuditsEveryClassEvenWhenOneIsDenied(t *testing.T) {
	ctx := WithRequest(context.Background(), PurposeGeneralChat, "", DataClass(""), ClassGeneral)
	audits := 0
	p := &Policy{OnAudit: func(Request, Decision) { audits++ }}
	if err := p.GuardContext(ctx); err == nil {
		t.Fatal("unknown class must deny")
	}
	if audits != 2 {
		t.Fatalf("all accumulated classes must leave an audit decision, got=%d", audits)
	}
}
