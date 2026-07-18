package k12

// 证据等级（PRD §4.8）。字符串值与文档/存储一致。
const (
	EvidenceLevelDeterministic  = "deterministic"
	EvidenceLevelCorroborated   = "corroborated"
	EvidenceLevelModelOnly      = "model_only"
	EvidenceLevelHumanConfirmed = "human_confirmed"
	EvidenceLevelInsufficient   = "insufficient"
)

// EvidenceLink claim 链上的一个环节（识别、题目对齐、验算…）。
// Necessary=true 表示该环节是结论成立的必要条件；旁证（Necessary=false）不拉低整链。
type EvidenceLink struct {
	Level     string
	Necessary bool
}

// evidenceRank 越大越强；insufficient 最弱。human_confirmed 不参与弱强排序——
// 它走规则 1 的人工优先分支（结论以人工为准）。
var evidenceRank = map[string]int{
	EvidenceLevelInsufficient:  0,
	EvidenceLevelModelOnly:     1,
	EvidenceLevelCorroborated:  2,
	EvidenceLevelDeterministic: 3,
}

// AggregateEvidenceLevel 聚合 Assessment 的 evidence_level（2026-07-18 修订，PRD §4.8）：
//  1. 存在人工改判（human_confirmed）时以人工为准；
//  2. 否则取 claim 链上**必要环节的最弱项**——识别（OCR）错而验算对，也不得包装成
//     "已通过确定性验证"；总可信度由最弱必要环节决定；
//  3. 无必要环节证据时为 insufficient，不允许取乐观值。
func AggregateEvidenceLevel(links []EvidenceLink) string {
	weakest := ""
	for _, l := range links {
		if l.Level == EvidenceLevelHumanConfirmed && l.Necessary {
			return EvidenceLevelHumanConfirmed
		}
		if !l.Necessary {
			continue
		}
		if weakest == "" || evidenceRank[l.Level] < evidenceRank[weakest] {
			weakest = l.Level
		}
	}
	if weakest == "" {
		return EvidenceLevelInsufficient
	}
	return weakest
}
