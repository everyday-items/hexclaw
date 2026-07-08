package egress

import "testing"

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
