// Package usecase 是 K12 场景的用例编排层（架构 §6.5）——业务闭环
// （识题 → 年级校验 → 解题验算 → 批改 → 记录 → 学情）只在此落地。
//
// 依赖倒置：用例依赖 port 接口（Solver/Grader/Recognizer/Insights），
// 真实实现（engine/solve、OCR、memory）作为 adapter 注入。故本层可用 fake 全闭环测试。
package usecase

// Verdict 验算结论（PRD §5.3.2）。
type Verdict string

const (
	VerdictAgree        Verdict = "agree"
	VerdictDisagree     Verdict = "disagree"
	VerdictUnverifiable Verdict = "unverifiable"
	VerdictOutOfScope   Verdict = "out_of_scope"
	VerdictVerbatim     Verdict = "verbatim" // 语英字词再练：原词重现，一字不差字符比对（确定性）
)

// EvidenceType 验证方式（决定徽章强弱）。
type EvidenceType string

const (
	EvidenceNumericExec        EvidenceType = "numeric_exec"        // 数值执行（code_exec）
	EvidenceSymbolicExec       EvidenceType = "symbolic_exec"       // 符号执行（SymPy MCP）
	EvidenceHeterogeneousModel EvidenceType = "heterogeneous_model" // 异构模型复核
	EvidenceHeuristic          EvidenceType = "heuristic"           // 同源模型启发（弱）
	EvidenceVerbatim           EvidenceType = "verbatim"            // 确定性字符比对（语英字词，一字不差，强）
	EvidenceNone               EvidenceType = "none"                // 无验证
)

// SolveEvidence solve 输出的证据对象（评审 P0-4）——徽章强弱由**实际验证方式**决定，
// 同源模型复核不得包装成"已程序验算"。
type SolveEvidence struct {
	Verdict             Verdict
	EvidenceType        EvidenceType
	SolverModel         string
	VerifierModel       string
	RecognizedInputHash string // 绑定 OCR 回显后的输入；输入被改则徽章失效
}

// StrongTrust 是否可显"强信任"徽章（PRD §5.3.2）：
//   - 数值/符号执行 = 强（客观程序验算）；
//   - 异构模型复核 = 强，但**必须真异构**（solver ≠ verifier 模型）；
//   - 其余（同源启发/无）= 弱。
func (e SolveEvidence) StrongTrust() bool {
	switch e.EvidenceType {
	case EvidenceNumericExec, EvidenceSymbolicExec, EvidenceVerbatim:
		return true
	case EvidenceHeterogeneousModel:
		return e.SolverModel != "" && e.SolverModel != e.VerifierModel
	default:
		return false
	}
}

// Badge 返回前台徽章语义（供 verify-badge 渲染，PRD §3.3.6 / §5.3.2）。
func (e SolveEvidence) Badge() string {
	switch e.Verdict {
	case VerdictAgree:
		if e.StrongTrust() {
			return "verified-strong" // ✅ 已程序验算
		}
		return "verified-weak" // 「AI 自检一致·未程序验算」
	case VerdictDisagree:
		return "disagree" // ⚠️ 并列双答 + 请复核
	case VerdictOutOfScope:
		return "out-of-scope" // ⛔ 已按学段内方法重解
	case VerdictVerbatim:
		return "verbatim-recall" // 📝 语英原词重现·一字不差判定
	default:
		return "unverifiable" // 无徽章 + 文字注明
	}
}
