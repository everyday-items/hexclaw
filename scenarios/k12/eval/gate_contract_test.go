package eval

// SubjectVerifierGate 翻门流程契约（§5.7 验证器质量门联动）：
// 翻门必须携带 holdout 报告 ID。历史达门（数学/语文/英语）的 legacy 依据是
// k12.VerifierGateBaselineEvidence（deterministic-baseline，执行计划 §5.7 翻门治理明文允许，
// 正式分学科 holdout 报告落库后替换）；此后任何新翻门的证据必须是本包 runner 落盘的
// blind holdout 报告 ID（内容寻址 "k12eval-"+16 hex）。usecase 侧 TestVerifierGateGovernance
// 钉「非空 + 与运行时一致」，本契约进一步钉**证据格式**，防止随手填一个字符串蒙混翻门。

import (
	"regexp"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var holdoutReportIDPattern = regexp.MustCompile(`^k12eval-[0-9a-f]{16}$`)

func TestVerifierGateHoldoutEvidence(t *testing.T) {
	entries := k12.VerifierGateEntries()
	if len(entries) == 0 {
		t.Fatal("验证器门治理记录不应为空")
	}
	// legacy 白名单：仅数学/语文/英语可持 deterministic-baseline 基线证据（历史裁决）。
	legacyAllowed := map[string]bool{"数学": true, "语文": true, "英语": true}
	for subject, gate := range entries {
		if !gate.Passed {
			if gate.EvalReportID != "" {
				t.Fatalf("学科 %q 未达门却带证据标识 %q——门状态与证据不一致", subject, gate.EvalReportID)
			}
			continue
		}
		switch {
		case gate.EvalReportID == "":
			t.Fatalf("学科 %q 已达门却无 eval 证据标识——禁止无证据翻门", subject)
		case gate.EvalReportID == k12.VerifierGateBaselineEvidence:
			if !legacyAllowed[subject] {
				t.Fatalf("学科 %q 不得使用 legacy 基线证据翻门——必须携带 blind holdout 报告 ID（k12eval-*）", subject)
			}
		case holdoutReportIDPattern.MatchString(gate.EvalReportID):
			// 正式证据：内容寻址 holdout 报告 ID，格式合规。
		default:
			t.Fatalf("学科 %q 翻门证据 %q 格式非法——只接受 %q（legacy）或 holdout 报告 ID（k12eval-+16hex）",
				subject, gate.EvalReportID, k12.VerifierGateBaselineEvidence)
		}
	}
	// 现状钉死：科学/信息科技在第 4 套 eval holdout 报告落库前不得翻门。
	// 将来翻门时：把学科置 Passed=true + 填 holdout 报告 ID，并同步修订本断言（显式治理动作）。
	for _, subject := range []string{"科学", "信息科技"} {
		gate, ok := entries[subject]
		if !ok {
			t.Fatalf("治理表缺学科 %q", subject)
		}
		if gate.Passed {
			t.Fatalf("学科 %q 翻门必须携带第 4 套 eval 的 holdout 报告 ID 并同步修订本契约（当前不应为 Passed）", subject)
		}
	}
}
