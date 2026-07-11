package egress

import (
	"strings"
	"testing"
)

func TestEvaluate_SensitiveBlockedByDefault(t *testing.T) {
	p := &Policy{}
	// 敏感档案/记录/记忆在通用对话用途下 → 拦截（不得出本机）
	for _, dc := range []DataClass{ClassSensitiveProfile, ClassRecord, ClassMemory} {
		d := p.Evaluate(Request{Purpose: PurposeGeneralChat, DataClass: dc})
		if d.AllowCloud {
			t.Errorf("敏感类 %s 在 general_chat 下应被拦截, got allow=true (%s)", dc, d.Reason)
		}
	}
}

func TestEvaluate_WhitelistedPurposes(t *testing.T) {
	p := &Policy{}
	// 图像识别：敏感媒体放行
	if d := p.Evaluate(Request{Purpose: PurposeVisionOCR, DataClass: ClassSensitiveMedia}); !d.AllowCloud {
		t.Errorf("vision_ocr + sensitive_media 应放行, got block (%s)", d.Reason)
	}
	// 受验证推理：敏感档案放行
	if d := p.Evaluate(Request{Purpose: PurposeSolveVerify, DataClass: ClassSensitiveProfile}); !d.AllowCloud {
		t.Errorf("solve_verify + sensitive_profile 应放行, got block (%s)", d.Reason)
	}
	// 但记录/记忆即便在受验证推理下也不放行（只白名单了档案）
	if d := p.Evaluate(Request{Purpose: PurposeSolveVerify, DataClass: ClassRecord}); d.AllowCloud {
		t.Errorf("solve_verify + record 不应放行, got allow (%s)", d.Reason)
	}
}

func TestEvaluate_NonSensitiveAllowed(t *testing.T) {
	p := &Policy{}
	for _, dc := range []DataClass{ClassDocument, ClassGeneral} {
		if d := p.Evaluate(Request{Purpose: PurposeRAGEmbed, DataClass: dc}); !d.AllowCloud {
			t.Errorf("非敏感类 %s 应放行, got block (%s)", dc, d.Reason)
		}
	}
}

func TestGuard_And_Audit(t *testing.T) {
	var audited int
	p := &Policy{OnAudit: func(Request, Decision) { audited++ }}
	if err := p.Guard(Request{Purpose: PurposeGeneralChat, DataClass: ClassSensitiveProfile}); err == nil {
		t.Error("敏感类出网应被 Guard 拦截返回 error")
	}
	if err := p.Guard(Request{Purpose: PurposeVisionOCR, DataClass: ClassSensitiveMedia}); err != nil {
		t.Errorf("白名单请求 Guard 应放行, got %v", err)
	}
	if audited != 2 {
		t.Errorf("OnAudit 应被调用 2 次, got %d", audited)
	}
}

func TestEvaluate_FailsClosedForUnknownMetadataAndSensitiveMedia(t *testing.T) {
	tests := []struct {
		name string
		req  Request
	}{
		{name: "unknown purpose", req: Request{Purpose: Purpose("typo"), DataClass: ClassGeneral}},
		{name: "empty purpose", req: Request{DataClass: ClassGeneral}},
		{name: "unknown data class", req: Request{Purpose: PurposeGeneralChat, DataClass: DataClass("typo")}},
		{name: "empty data class", req: Request{Purpose: PurposeGeneralChat}},
		{name: "sensitive media in general chat", req: Request{Purpose: PurposeGeneralChat, DataClass: ClassSensitiveMedia}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&Policy{}).Evaluate(tt.req); got.AllowCloud {
				t.Fatalf("Evaluate(%+v) allowed cloud egress: %+v", tt.req, got)
			}
		})
	}
}

func TestEvaluate_AuditReceivesExactDeniedDecisionOnce(t *testing.T) {
	req := Request{Purpose: Purpose("unknown"), DataClass: ClassGeneral, AuditID: "audit-1"}
	var calls int
	var gotReq Request
	var gotDecision Decision
	p := &Policy{OnAudit: func(audited Request, decision Decision) {
		calls++
		gotReq = audited
		gotDecision = decision
	}}

	err := p.Guard(req)
	if err == nil {
		t.Fatal("Guard allowed request with unknown purpose")
	}
	if calls != 1 {
		t.Fatalf("audit calls = %d, want exactly 1", calls)
	}
	if gotReq != req {
		t.Fatalf("audited request = %+v, want %+v", gotReq, req)
	}
	if gotDecision.AllowCloud || gotDecision.Reason == "" || !strings.Contains(err.Error(), gotDecision.Reason) {
		t.Fatalf("audit decision/error mismatch: decision=%+v err=%v", gotDecision, err)
	}
}

func TestAllDeclaredPurposesAreRecognizedForGeneralData(t *testing.T) {
	for _, purpose := range []Purpose{
		PurposeVisionOCR, PurposeSolveVerify, PurposeGeneralChat, PurposeRAGEmbed,
		PurposeRAGEnrich, PurposeProviderProbe, PurposeAutomationBuild,
	} {
		if decision := (&Policy{}).Evaluate(Request{Purpose: purpose, DataClass: ClassGeneral}); !decision.AllowCloud {
			t.Errorf("declared purpose %q is not registered: %+v", purpose, decision)
		}
	}
}
