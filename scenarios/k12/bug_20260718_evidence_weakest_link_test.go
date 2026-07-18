package k12

import "testing"

// 2026-07-18 证据聚合裁决（外部评审 #4d，PRD §4.8 修订）：
// Assessment 的 evidence_level 不得取"最高等级"——OCR 识别错、计算验算对，
// 也可能被包装成"确定性验证"。规则改为 claim 链上**必要环节取最弱项**；
// 人工改判仍然最高优先（结论以人工为准）。
func TestAggregateEvidenceLevelWeakestNecessaryLink(t *testing.T) {
	// 经典漏洞场景：识别环节只有模型判断（necessary），验算环节 deterministic——
	// 旧规则取最高 = deterministic（谎报）；新规则取必要环节最弱 = model_only。
	got := AggregateEvidenceLevel([]EvidenceLink{
		{Level: EvidenceLevelModelOnly, Necessary: true},     // OCR/识别
		{Level: EvidenceLevelDeterministic, Necessary: true}, // 独立验算
	})
	if got != EvidenceLevelModelOnly {
		t.Fatalf("必要环节最弱项应为 model_only（OCR 弱环节不得被验算掩盖），got %s", got)
	}

	// 全链 deterministic → deterministic。
	got = AggregateEvidenceLevel([]EvidenceLink{
		{Level: EvidenceLevelDeterministic, Necessary: true},
		{Level: EvidenceLevelDeterministic, Necessary: true},
	})
	if got != EvidenceLevelDeterministic {
		t.Fatalf("全链确定性应为 deterministic，got %s", got)
	}

	// 非必要环节（旁证）不拉低整链：deterministic 主链 + model_only 旁证 → deterministic。
	got = AggregateEvidenceLevel([]EvidenceLink{
		{Level: EvidenceLevelDeterministic, Necessary: true},
		{Level: EvidenceLevelModelOnly, Necessary: false},
	})
	if got != EvidenceLevelDeterministic {
		t.Fatalf("旁证不应拉低必要链，got %s", got)
	}

	// 人工改判最高优先：结论以人工为准（PRD §4.8 规则 1 不变）。
	got = AggregateEvidenceLevel([]EvidenceLink{
		{Level: EvidenceLevelHumanConfirmed, Necessary: true},
		{Level: EvidenceLevelModelOnly, Necessary: true},
	})
	if got != EvidenceLevelHumanConfirmed {
		t.Fatalf("人工改判应最高优先，got %s", got)
	}

	// 空链 → insufficient（无证据不得乐观）。
	if got := AggregateEvidenceLevel(nil); got != EvidenceLevelInsufficient {
		t.Fatalf("空证据链应为 insufficient，got %s", got)
	}
}
