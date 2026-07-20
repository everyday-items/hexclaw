package usecase_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// mistakeCase 一道题的批改用例（child 答案铁定对/错，模型可靠抓判）。
type mistakeCase struct {
	session     string // 来源会话 = 幂等去重键的一半（同 session+题 = 同一道错题）
	subject     string
	problem     string
	answer      string   // 学生答案
	kp          []string // 识题产出的知识点（回填进错题库 knowledge_point）
	expectWrong bool     // 固定算术真值；真实验收必须与该判定一致
}

// mistakeCases 3 道明确打错 + 2 道明确答对（对/错各复用题面，但用独立 session 隔离）。
func mistakeCases() []mistakeCase {
	return []mistakeCase{
		{session: "sess-wrong-mul67", subject: "数学", problem: "6×7", answer: "13", kp: []string{"表内乘法"}, expectWrong: true},
		{session: "sess-wrong-mul128", subject: "数学", problem: "12×8", answer: "90", kp: []string{"多位数乘一位数"}, expectWrong: true},
		{session: "sess-wrong-sub4517", subject: "数学", problem: "45-17", answer: "30", kp: []string{"两位数退位减法"}, expectWrong: true},
		{session: "sess-right-mul67", subject: "数学", problem: "6×7", answer: "42", kp: []string{"表内乘法"}, expectWrong: false},
		{session: "sess-right-mul128", subject: "数学", problem: "12×8", answer: "96", kp: []string{"多位数乘一位数"}, expectWrong: false},
	}
}

// TestK12RealMistakeBookChain_RealModel 真模型多轮全链路门：解题→批改→错题入库→统计。
// 验证用户 5 大关切（打错入库/统计准确/步骤清晰/verifier code_exec/多轮幂等去重）。
// env 缺失 → Skip（同现有 real gate）。
func TestK12RealMistakeBookChain_RealModel(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_LLM_EVAL") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run the real K12 mistake-book chain")
	}
	key := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_LLM_KEY"))
	if key == "" {
		t.Fatal("HEXCLAW_REAL_LLM_EVAL=1 requires HEXCLAW_REAL_LLM_KEY")
	}
	base := envOr("HEXCLAW_REAL_LLM_BASE", "https://api.siliconflow.cn/v1/chat/completions")
	model := envOr("HEXCLAW_REAL_LLM_MODEL", "Qwen/Qwen3.6-35B-A3B")
	t.Logf("real mistake-book chain model=%q endpoint_configured=%t", model, strings.TrimSpace(base) != "")

	k := newMistakeK12(t, realEvalExec(base, model, key))
	// 免费 nano 模型每次 solve 20~90s；3 轮 × ~3~5 题 × (solve+grade) → 给足 20min。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	runMistakeChain(t, ctx, k.Deps, mistakeCases(), 3)
}

// TestK12MistakeBookChain_StubWiring 确定性桩预跑：验证装配可编译、断言逻辑不 panic、
// 幂等/统计不变量在可控 verdict 下成立（不联网，无 env gate）。
func TestK12MistakeBookChain_StubWiring(t *testing.T) {
	k := newMistakeK12(t, stubGradeExec())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runMistakeChain(t, ctx, k.Deps, mistakeCases(), 3)
}

// newMistakeK12 装配一套内存库 + 真实 SolveSkill/Grader（exec 决定桩/真模型）。
func newMistakeK12(t *testing.T, exec engine.SubAgentExecFunc) *assembly.K12 {
	t.Helper()
	db := openMigratedTestDB(t)
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('eval-agent')`); err != nil {
		t.Fatal(err)
	}
	registry := engine.NewSubAgentRegistry(t.TempDir() + "/subagent-runs.json")
	solveSkill := engine.NewSolveSkill(exec, registry)
	k, err := assembly.Wire(db, solveSkill)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// runMistakeChain 跑 rounds 轮批改，落全链路硬断言；固定算术题的 verdict 也是发布门，
// 不能把模型误判或全程无结果记成 PASS。
//
// 硬不变量：
//   - 判错 ⟹ 首次入库(RecordCreated) ⟹ 出现在错题库 ⟹ 记录 knowledge_point/error_cause 非空；WrongStep/ErrorCause 非空。
//   - 判对 ⟹ 不新入库。
//   - 多轮同 session+题 幂等：已入库后重判错不再 RecordCreated（计数不翻倍）。
//   - 统计准确：终态错题库计数 == 曾入库的去重 session 数。
func runMistakeChain(t *testing.T, ctx context.Context, deps usecase.Deps, cases []mistakeCase, rounds int) {
	t.Helper()
	const agent = "eval-agent"
	createdSessions := map[string]bool{}
	strongVerify := 0 // verifier 真用 code_exec / 强证据的题数（solve 方法论命脉）
	graded := 0       // 成功拿到模型判定的题次（区分"链路验证过"与"全被上游限流"）
	inconclusive := 0 // 上游/模型未产出可验收结果的题次

	for r := 1; r <= rounds; r++ {
		for caseIndex, c := range cases {
			// 幂等复跑只重跑错题（对题第二轮无新语义）。
			if r > 1 && !c.expectWrong {
				continue
			}
			already := createdSessions[c.session]

			req := usecase.GradeRequest{
				AgentName:       agent,
				Subject:         c.subject,
				SourceSession:   c.session,
				Problem:         c.problem,
				StudentAnswer:   c.answer,
				KnowledgePoints: c.kp,
			}
			var res usecase.GradeResult
			var err error
			// 免费模型偶发格式跑偏/429/超时 → 重试至多 3 次。
			for attempt := 1; attempt <= 3; attempt++ {
				res, err = deps.GradeHomeworkProblem(ctx, req)
				if err == nil || ctx.Err() != nil {
					break
				}
				t.Logf("round=%d case=%d attempt=%d failed error_type=%T; retrying", r, caseIndex+1, attempt, err)
			}
			if err != nil {
				// ErrSolveFailed = solver/grader 上游或输出格式失败（免费 nano reasoning 模型能力/429/超时），
				// 单题上游波动先记 inconclusive 并继续收集其它证据；若最终没有任何
				// 可验收判定，终态门必须 FAIL，不能形成 vacuous PASS。
				// 其余（ErrInvalidInput 等结构性错误）= 真链路 bug → FAIL。
				if errors.Is(err, usecase.ErrSolveFailed) || ctx.Err() != nil {
					inconclusive++
					t.Logf("🟡 inconclusive(模型能力/上游，非链路 bug): round=%d case=%d error_type=%T",
						r, caseIndex+1, err)
					continue
				}
				t.Fatalf("round=%d case=%d: 批改链路错误(链路 bug): error_type=%T", r, caseIndex+1, err)
			}
			if res.OutOfScope {
				t.Fatalf("round=%d case=%d: 意外超纲(OutOfScope=true, kp_chars=%d)——本组题不应触发超纲",
					r, caseIndex+1, len([]rune(res.OutOfScopeKP)))
			}

			graded++
			recs := mustList(t, ctx, deps, agent)
			badge := res.Evidence.Badge()
			if res.Evidence.StrongTrust() {
				strongVerify++
			}
			t.Logf("round=%d case=%d verdict=%v record_created=%v wrong_step_chars=%d error_cause_chars=%d kp_chars=%d badge=%q evidence_verdict=%q evidence_type=%q mistake_count=%d",
				r, caseIndex+1, res.Outcome.Verdict, res.RecordCreated,
				len([]rune(res.Outcome.WrongStep)), len([]rune(res.Outcome.ErrorCause)),
				len([]rune(res.Outcome.KnowledgePoint)), badge, res.Evidence.Verdict,
				res.Evidence.EvidenceType, len(recs))

			// 固定算术真值是 LIVE 门的一部分；模型误判不是可接受的链路通过证据。
			if (res.Outcome.Verdict == usecase.VerdictAgree) == c.expectWrong {
				t.Errorf("发布门判定错误: round=%d case=%d 期望判%s 实判%s",
					r, caseIndex+1, wrongWord(c.expectWrong), wrongWord(res.Outcome.Verdict != usecase.VerdictAgree))
			}

			// 硬不变量：链路只对"模型实际判定"负责，与期望无关。
			if res.Outcome.Verdict == usecase.VerdictDisagree {
				// 判错分支：步骤清晰 + 入库 + 统计。
				if strings.TrimSpace(res.Outcome.WrongStep) == "" {
					t.Errorf("链路 bug: round=%d case=%d 判错但 WrongStep 为空（步骤不清晰）", r, caseIndex+1)
				}
				if strings.TrimSpace(res.Outcome.ErrorCause) == "" {
					t.Errorf("链路 bug: round=%d case=%d 判错但 ErrorCause 为空（错因缺失）", r, caseIndex+1)
				}
				if !already {
					if !res.RecordCreated {
						t.Errorf("链路 bug: round=%d case=%d 首次判错但未入库(RecordCreated=false)", r, caseIndex+1)
					}
				} else {
					if res.RecordCreated {
						t.Errorf("链路 bug: round=%d case=%d 同 session 重复判错却再次入库(幂等去重失效)", r, caseIndex+1)
					}
				}
				// 判错题必须出现在错题库，且领域字段非空。
				rec := findMistake(recs, c.session, c.problem)
				if rec == nil {
					t.Errorf("链路 bug: round=%d case=%d 判错却未出现在错题库(ListByScope)", r, caseIndex+1)
				} else {
					var f k12.MistakeFields
					if err := json.Unmarshal([]byte(rec.Fields), &f); err != nil {
						t.Errorf("链路 bug: round=%d case=%d 错题记录 Fields 非法 JSON: error_type=%T", r, caseIndex+1, err)
					} else {
						if strings.TrimSpace(f.KnowledgePoint) == "" {
							t.Errorf("链路 bug: round=%d case=%d 错题记录 knowledge_point 为空", r, caseIndex+1)
						}
						if strings.TrimSpace(f.ErrorCause) == "" {
							t.Errorf("链路 bug: round=%d case=%d 错题记录 error_cause 为空", r, caseIndex+1)
						}
					}
				}
			} else {
				// 判对分支：不得新入库。
				if res.RecordCreated {
					t.Errorf("链路 bug: round=%d case=%d 判对却新入错题库(RecordCreated=true)", r, caseIndex+1)
				}
			}

			if res.RecordCreated {
				createdSessions[c.session] = true
			}
		}
	}

	// 统计准确（硬）：终态错题库计数 == 曾入库的去重 session 数（多轮重复同题不增加）。
	final := mustList(t, ctx, deps, agent)
	if len(final) != len(createdSessions) {
		t.Errorf("链路 bug: 统计不准——错题库计数=%d, 期望去重题数=%d", len(final), len(createdSessions))
	}
	t.Logf("==== 终态：错题库计数=%d 去重入库 session=%d 成功批改题次=%d inconclusive=%d 强验证(verifier code_exec/异构)题次=%d ====",
		len(final), len(createdSessions), graded, inconclusive, strongVerify)
	if graded == 0 {
		t.Fatalf("发布门无有效证据：成功批改题次=0 inconclusive=%d；必须在上游可用时重跑", inconclusive)
	}
	for recordIndex, rec := range final {
		var f k12.MistakeFields
		_ = json.Unmarshal([]byte(rec.Fields), &f)
		t.Logf("mistake_record=%d question_chars=%d knowledge_point_chars=%d error_cause_chars=%d status=%q",
			recordIndex+1, len([]rune(f.Question)), len([]rune(f.KnowledgePoint)),
			len([]rune(f.ErrorCause)), rec.Status)
	}
}

func mustList(t *testing.T, ctx context.Context, deps usecase.Deps, agent string) []*records.AgentRecord {
	t.Helper()
	recs, err := deps.Records.ListByScope(ctx, agent, k12.CollectionMistakes, "")
	if err != nil {
		t.Fatalf("ListByScope(错题本) 失败: error_type=%T", err)
	}
	return recs
}

func findMistake(recs []*records.AgentRecord, session, problem string) *records.AgentRecord {
	for _, rec := range recs {
		if rec.SourceSession != session {
			continue
		}
		var f k12.MistakeFields
		if err := json.Unmarshal([]byte(rec.Fields), &f); err != nil {
			continue
		}
		if f.Question == problem {
			return rec
		}
	}
	return nil
}

func wrongWord(wrong bool) string {
	if wrong {
		return "错"
	}
	return "对"
}

// ---- 确定性桩（stub）：让 SolveSkill 走完 solver→verifier→grader，判定由题面算术查表决定 ----

var (
	stubProblemRe = regexp.MustCompile(`题目\s*[:：]\s*(.+)`)
	stubAnswerRe  = regexp.MustCompile(`学生的答案\s*[:：]\s*(.+)`)
)

// stubCorrect 各题正确答案表（桩用；真模型不走此）。
var stubCorrect = map[string]string{"6×7": "42", "12×8": "96", "45-17": "28"}

// stubGradeExec 返回一个确定性 exec：solver 回显正确答案，verifier AGREE，grader 按查表判对/错。
func stubGradeExec() engine.SubAgentExecFunc {
	return func(_ context.Context, spec engine.SubAgentSpec) (engine.SubAgentResult, error) {
		switch spec.Agent {
		case "solver":
			ans := stubLookup(spec.Task)
			return engine.SubAgentResult{Output: fmt.Sprintf("解题步骤：按算术计算。\n答案：%s", ans)}, nil
		case "verifier":
			return engine.SubAgentResult{Output: "VERDICT: AGREE\nCOMPUTED: " + stubLookup(spec.Task)}, nil
		case "grader":
			problem := firstGroup(stubProblemRe, spec.Task)
			student := firstGroup(stubAnswerRe, spec.Task)
			correct := stubCorrect[strings.TrimSpace(problem)]
			if strings.TrimSpace(student) == correct && correct != "" {
				return engine.SubAgentResult{Output: "CORRECT: yes\nWRONG_STEP: \nMISCONCEPTION: \nGUIDANCE: 说说你的思路"}, nil
			}
			return engine.SubAgentResult{Output: fmt.Sprintf(
				"CORRECT: no\nWRONG_STEP: 计算 %s 时算成了 %s\nMISCONCEPTION: 计算失误\nGUIDANCE: 回到这一步再算一遍",
				strings.TrimSpace(problem), strings.TrimSpace(student))}, nil
		default:
			return engine.SubAgentResult{}, fmt.Errorf("stub: unexpected agent %q", spec.Agent)
		}
	}
}

// stubLookup 从任意 spec.Task 中找出已知题面并返回正确答案（找不到给占位）。
func stubLookup(task string) string {
	for p, a := range stubCorrect {
		if strings.Contains(task, p) {
			return a
		}
	}
	return "?"
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}
