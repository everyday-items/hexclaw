package engine

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
	"github.com/hexagon-codes/toolkit/os/sandbox"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// 多 Agent（拆分）vs 单 Agent 执行 —— 质量 A/B 对照（devtestops Step 5.5 量化对照）。
//
// 三臂（同一模型，隔离每层边际价值）：
//
//	A 单Agent·裸     ：一次 LLM 直答，无工具（裸推理 → 暴露算术/推理幻觉）
//	B 单Agent·带code ：一个 Agent 可调 code_exec（Program-of-Thought），无独立校验
//	C 多Agent·solve  ：完整 solve 管线（solver + fresh-context code 校验 + triage/method-diversity + 诚实并列）
//
// 指标矩阵：准确率 / false-confidence（答错且未标注复核）/ caught（答错但被标注请复核——安全网生效）。
//
// 两个执行后端：
//   - TestSolve_AB_MultiAgentVsSingleAgent：scSolveExec 极简工具环（HEXCLAW_REALMODEL=1）。强模型 tool-call
//     流式格式它可能漏解析，用工具的臂会被 harness 噪声污染——只适合粗看。
//   - TestSolve_AB_RealEngine：**产线 react 循环**（eng.Process，HEXCLAW_AB_REAL=1）。ai-core 适配器正确解析
//     tool_call，code_exec 真跑本机 python3——这一条才用来「坐实准确率增益维度」。

type abProb struct {
	q  string
	gt string
}

var abMixed = []abProb{
	{"小明有 6 盒铅笔，每盒 7 支，一共多少支？", "42"},
	{"57 × 43 等于多少？", "2451"},
	{"计算 1 到 100 所有偶数的和。", "2550"},
	{"一件商品原价 80 元，先涨价 25%，再降价 25%，现价多少元？", "75"},
	{"一个长方形周长 24 厘米，长是宽的 2 倍，求它的面积（平方厘米）。", "32"},
	{"甲、乙两数之和为 20，差为 4，求这两个数。", "12和8"},
	{"比较 3/8 和 0.4，哪个更大？", "0.4"},
}

// abTrap：大数阶乘/多位连乘/大区间累加——单 Agent 裸推理几乎必错、但 code 一算就对，
// 用来在「能可靠调 code 的中等模型」上隔离 code-grounding 的准确率增益。
var abTrap = []abProb{
	{"13 的阶乘（13!）等于多少？", "6227020800"},
	{"123456 × 7890 等于多少？", "974067840"},
	{"37 × 41 × 43 等于多少？", "65231"},
	{"求 1 到 1000 之间所有 7 的倍数的和。", "71071"},
	{"8 的 9 次方等于多少？", "134217728"},
	{"17 × 19 × 23 等于多少？", "7429"},
}

func abProbsFromEnv() []abProb {
	if os.Getenv("HEXCLAW_AB_SET") == "trap" {
		return abTrap
	}
	return abMixed
}

// abProseNumRe 抓散文里的数字（整数/小数/分数/百分数）。
var abProseNumRe = regexp.MustCompile(`[+-]?\d+(?:\.\d+)?(?:/\d+(?:\.\d+)?)?%?`)

func abNumsInProse(s string) []float64 {
	var out []float64
	for _, m := range abProseNumRe.FindAllString(s, -1) {
		if v, ok := numericValue(m); ok {
			out = append(out, v)
		}
	}
	return out
}

// abCorrect 测量用比对（非产线）：语义等值 → 字符串 contains → 散文抽数要求 GT 每个数都出现
// （治"甲=12，乙=8"≡GT"12和8"这类带标签散文多元答案，否则把模型的正确答案误记成错、污染 AB）。
func abCorrect(ans, gt string) bool {
	if answersEqual(ans, gt) {
		return true
	}
	na, ngt := normalizeAnswer(ans), normalizeAnswer(gt)
	if ngt != "" && strings.Contains(na, ngt) {
		return true
	}
	if gtNums, ok := numberSet(gt); ok {
		ansNums := abNumsInProse(ans)
		for _, g := range gtNums {
			hit := false
			for _, a := range ansNums {
				if floatsClose(a, g) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		}
		return true
	}
	return false
}

type abTally struct{ correctN, falseConf, caught int }

// runABComparison 跑三臂对照并打印矩阵；exec 是子 Agent 执行后端（harness 或产线 loop）。
func runABComparison(t *testing.T, modelLabel string, exec SubAgentExecFunc, probs []abProb) {
	t.Helper()
	repeat := getenvInt("HEXCLAW_AB_REPEAT", 1)
	o := NewSolveSkill(exec, nil)

	// 单 Agent 一臂：arm A 用 "__none__" 白名单（产线 loop 里空 ToolAllow=不限=会拿到全部工具，
	// 必须用一个匹配不到任何工具的哨兵把它收成"无工具"；harness 里它也等于无工具）。
	singleArm := func(ctx context.Context, q string, withCode bool) string {
		allow := []string{"__none__"}
		if withCode {
			allow = []string{codeExecToolName}
		}
		spec := SubAgentSpec{RunID: "ab-single", Agent: solverAgentName, Task: buildSolverPrompt(q, "", "", "", ""),
			ToolAllow: allow, Mode: "run", Depth: maxSpawnDepth, Source: solveDispatchSource}
		res, err := exec(ctx, spec)
		if err != nil {
			return ""
		}
		return extractFinalAnswer(res.Output)
	}
	acc := func(tl *abTally, ok, flagged, single bool) {
		if ok {
			tl.correctN++
			return
		}
		if single || !flagged {
			tl.falseConf++
		} else {
			tl.caught++
		}
	}

	var A, B, C abTally
	t.Logf("=== 多Agent vs 单Agent 质量 AB：%s（N=%d 题 × %d 次/臂/题 = %d trials/臂）===", modelLabel, len(probs), repeat, len(probs)*repeat)

	for i, p := range probs {
		aC, bC, cC := 0, 0, 0 // 本题各臂答对次数
		for r := 0; r < repeat; r++ {
			ctx, cancel := context.WithTimeout(context.Background(), 280*time.Second)

			aOK := abCorrect(singleArm(ctx, p.q, false), p.gt)
			bOK := abCorrect(singleArm(ctx, p.q, true), p.gt)

			cFlagged, cAns := false, ""
			if cRes, cErr := o.Execute(ctx, map[string]any{"problem": p.q}); cErr == nil && cRes != nil {
				cAns = extractFinalAnswer(cRes.Content)
				v := cRes.Metadata["solve_verdict"]
				cFlagged = v == "disagree" || v == "unverifiable" ||
					strings.Contains(cRes.Content, "⚠️") || strings.Contains(cRes.Content, "复核") || strings.Contains(cRes.Content, "存疑")
			}
			cOK := abCorrect(cAns, p.gt)
			cancel()

			acc(&A, aOK, false, true)
			acc(&B, bOK, false, true)
			acc(&C, cOK, cFlagged, false)
			if aOK {
				aC++
			}
			if bOK {
				bC++
			}
			if cOK {
				cC++
			}
		}
		qShort := string([]rune(p.q)[:min2(24, len([]rune(p.q)))])
		t.Logf("#%-2d %-24s GT=%-10s | A %d/%d  B %d/%d  C %d/%d", i+1, qShort, p.gt, aC, repeat, bC, repeat, cC, repeat)
	}

	trials := len(probs) * repeat
	wrong := func(tl abTally) int { return trials - tl.correctN }
	accPct := func(tl abTally) string {
		return fmt.Sprintf("%d/%d (%.0f%%)", tl.correctN, trials, 100*float64(tl.correctN)/float64(trials))
	}
	t.Logf("──────────────────────────────────────────────")
	t.Logf("准确率   A 单裸=%s   B 单+code=%s   C 多Agent=%s", accPct(A), accPct(B), accPct(C))
	t.Logf("false-conf(答错且自信)  A=%d B=%d C=%d   |   caught(答错但标注复核)  A=%d B=%d C=%d", A.falseConf, B.falseConf, C.falseConf, A.caught, B.caught, C.caught)

	// 卡方显著性：omnibus 3×2(arms×{对,错}, df=2) + 关键 pairwise 2×2 Yates(df=1)。
	chiO, dfO := chiSqRxC([][]int{{A.correctN, wrong(A)}, {B.correctN, wrong(B)}, {C.correctN, wrong(C)}})
	pO := chiPValue(chiO, dfO)
	t.Logf("卡方 omnibus A/B/C×{对,错}: χ²=%.2f df=%d p=%.4f %s", chiO, dfO, pO, sigStar(pO))
	logPair := func(name string, x, y abTally) {
		c2, p := chiSq2x2Yates(x.correctN, wrong(x), y.correctN, wrong(y))
		t.Logf("卡方 %s (2×2 Yates): χ²=%.2f df=1 p=%.4f %s", name, c2, p, sigStar(p))
	}
	logPair("A vs C", A, C)
	logPair("B vs C", B, C)
	logPair("A vs B", A, B)
	t.Logf("ℹ️ 准确率 A→B→C 逐层增益；C.caught=诚实标注请复核(安全网)；p<0.05=差异统计显著。")
	t.Logf("GAINCURVE\t%s\trepeat=%d\tN=%d\tA=%.3f\tB=%.3f\tC=%.3f\tfcA=%d\tfcB=%d\tfcC=%d",
		modelLabel, repeat, len(probs),
		float64(A.correctN)/float64(trials), float64(B.correctN)/float64(trials), float64(C.correctN)/float64(trials),
		A.falseConf, B.falseConf, C.falseConf)
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// chiPValue 卡方上尾 p 值：df=1 用 erfc(√(x/2))（标准正态平方），df=2 用 exp(-x/2)（闭式）。仅这两档精确。
func chiPValue(chi2 float64, df int) float64 {
	switch df {
	case 1:
		return math.Erfc(math.Sqrt(chi2 / 2))
	default: // df==2（本测 omnibus 3×2 恒为 2）
		return math.Exp(-chi2 / 2)
	}
}

func sigStar(p float64) string {
	switch {
	case p < 0.01:
		return "**显著(p<0.01)**"
	case p < 0.05:
		return "*显著(p<0.05)*"
	default:
		return "(不显著)"
	}
}

// chiSqRxC 行×列列联表卡方统计量 + 自由度（皮尔逊，期望<1 也照算，小样本配合上尾近似看方向）。
func chiSqRxC(obs [][]int) (float64, int) {
	rows, cols := len(obs), len(obs[0])
	rowSum := make([]float64, rows)
	colSum := make([]float64, cols)
	total := 0.0
	for i := 0; i < rows; i++ {
		for j := 0; j < cols; j++ {
			v := float64(obs[i][j])
			rowSum[i] += v
			colSum[j] += v
			total += v
		}
	}
	chi2 := 0.0
	if total > 0 {
		for i := 0; i < rows; i++ {
			for j := 0; j < cols; j++ {
				e := rowSum[i] * colSum[j] / total
				if e > 0 {
					o := float64(obs[i][j])
					chi2 += (o - e) * (o - e) / e
				}
			}
		}
	}
	return chi2, (rows - 1) * (cols - 1)
}

// chiSq2x2Yates 2×2 带 Yates 连续性校正（小样本更稳）：χ²=N(|ad-bc|-N/2)²/[(a+b)(c+d)(a+c)(b+d)]。
func chiSq2x2Yates(a, b, c, d int) (float64, float64) {
	n := float64(a + b + c + d)
	den := float64((a + b) * (c + d) * (a + c) * (b + d))
	if den == 0 || n == 0 {
		return 0, 1
	}
	adbc := math.Abs(float64(a*d - b*c))
	corr := adbc - n/2
	if corr < 0 {
		corr = 0 // Yates 校正不让其变负
	}
	chi2 := n * corr * corr / den
	return chi2, chiPValue(chi2, 1)
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── harness 后端（粗看）────────────────────────────────────────────────
func TestSolve_AB_MultiAgentVsSingleAgent(t *testing.T) {
	if os.Getenv("HEXCLAW_REALMODEL") == "" {
		t.Skip("设 HEXCLAW_REALMODEL=1 + _KEY 跑 harness 版 AB（粗看；产线版用 HEXCLAW_AB_REAL=1）")
	}
	base := getenvDefault("HEXCLAW_REALMODEL_BASE", "https://api.siliconflow.cn/v1/chat/completions")
	model := getenvDefault("HEXCLAW_REALMODEL_MODEL", "deepseek-ai/DeepSeek-V3")
	key := os.Getenv("HEXCLAW_REALMODEL_KEY")
	if key == "" {
		t.Skip("需 HEXCLAW_REALMODEL_KEY")
	}
	runABComparison(t, model+"（harness）", scSolveExec(base, model, key), abProbsFromEnv())
}

// newRealEngineExec 起一个产线 react 引擎（真 LLM 路由 + 真 code_exec/python3），返回与 main.go
// agentExecFn 同形的子 Agent 执行器 + 模型名。供 AB / 跑偏取证测共用。
func newRealEngineExec(t *testing.T) (SubAgentExecFunc, string) {
	t.Helper()
	yamlPath := os.Getenv("HEXCLAW_AB_YAML")
	if yamlPath == "" {
		home, _ := os.UserHomeDir()
		yamlPath = filepath.Join(home, ".hexclaw", "hexclaw.yaml.bak.before-siliconflow-models-20260626-224056")
	}
	model := getenvDefault("HEXCLAW_AB_REALMODEL", "Qwen/Qwen2.5-14B-Instruct")

	cfg, err := config.Load(yamlPath)
	if err != nil {
		t.Skipf("加载 yaml 失败：%v", err)
	}
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "on"

	// 强制走 SiliconFlow provider + 指定中等模型（直接改 provider，绕开 models 白名单限制）。
	var picked string
	for name, p := range cfg.LLM.Providers {
		if strings.Contains(p.BaseURL, "siliconflow") && p.APIKey != "" {
			p.Model = model
			p.Models = []string{model}
			cfg.LLM.Providers[name] = p
			cfg.LLM.Default = name
			picked = name
			break
		}
	}
	if picked == "" {
		t.Skip("yaml 里没有带 key 的 siliconflow provider")
	}

	ctx := context.Background()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "real.db"))
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store 失败：%v", err)
	}

	skills := skill.NewRegistry()
	sbCfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 30}
	sb, err := sandbox.New(sbCfg)
	if err != nil {
		t.Fatalf("建 sandbox 失败：%v", err)
	}
	if err := skills.Register(builtin.NewCodeExecSkill(sb, sbCfg)); err != nil {
		t.Fatalf("注册 code_exec 失败：%v", err)
	}

	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Skipf("建 router 失败：%v", err)
	}
	eng := NewReActEngine(cfg, router, store, skills)
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("引擎启动失败：%v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	eng.SetToolCollector(NewToolCollector(skills, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(skills, nil))

	exec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		msg := &adapter.Message{
			ID: "real-" + idgen.NanoID(), Platform: adapter.PlatformAPI, UserID: "system",
			Content: spec.Task, Metadata: map[string]string{},
		}
		ApplySpecToMessage(msg, spec)
		// 每消息强制 provider+model（cfg.LLM.Default 不被引擎路由采纳，得走 metadata 覆盖）。
		msg.Metadata["provider"] = picked
		msg.Metadata["model"] = model
		reply, perr := eng.Process(ctx, msg)
		if perr != nil {
			return SubAgentResult{}, perr
		}
		return SubAgentResult{Output: reply.Content, SessionID: msg.SessionID}, nil
	}
	t.Logf("=== 产线 react 引擎就绪：provider=%s model=%s（真 code_exec/python3）===", picked, model)
	return exec, model
}

// ── 产线 react 循环 AB（HEXCLAW_AB_REAL=1）──────────────────────────────────
func TestSolve_AB_RealEngine(t *testing.T) {
	if os.Getenv("HEXCLAW_AB_REAL") != "1" {
		t.Skip("设 HEXCLAW_AB_REAL=1 跑产线 react 循环版 AB（真引擎 + 真 code_exec）")
	}
	exec, model := newRealEngineExec(t)
	runABComparison(t, model+"（产线loop）", exec, abProbsFromEnv())
}

// ─────────────────────────────────────────────────────────────────────────
//  「跑偏」取证：solve 把自己框成"已核验=高置信"(隐含准确率/正确性)，但
//   ① REPEAT=5+卡方：拆分不显著提准确率(见 AB 上方, p≈0.95)；
//   ② 下面两测：solve 确实做了高置信宣称(结构锁) + 该宣称会落在**错答案**上(真模型取证)。
//   两者合起来 = 数据证明"准确率/高置信"框架名不副实 → 真实价值是安全(诚实标注)而非准确率。
// ─────────────────────────────────────────────────────────────────────────

// TestSolve_Audit_HighConfidenceFramingClaim 锁住「跑偏」前半（确定性，无需模型）：
// solve 同意路径会打"✅ 已由独立校验员…核验一致（高置信）"，工具自述也宣称 verified——
// 即产品确实对外做了"已核验=高置信(≈正确)"的宣称。这正是被 AB 数据证否的那个框架。
func TestSolve_Audit_HighConfidenceFramingClaim(t *testing.T) {
	agree := formatSolve(
		[]answerGroup{{answer: "42", sols: []solverSolution{{output: "解题过程…\n答案：42", answer: "42"}}}},
		verdictAgree, "42", 1, false)
	for _, kw := range []string{"✅", "高置信", "核验"} {
		if !strings.Contains(agree, kw) {
			t.Errorf("solve 同意路径徽标应含 %q（坐实其高置信宣称），实得：%s", kw, agree)
		}
	}
	desc := strings.ToLower((&SolveSkill{}).Description())
	if !strings.Contains(desc, "verified") {
		t.Errorf("solve Description 应宣称 verified（坐实正确性框架），实得：%s", desc)
	}
	t.Logf("✅ 坐实(跑偏前半)：solve 对外宣称『已核验=高置信/verified』。被检验徽标=「%s」", strings.TrimSpace(agree[strings.Index(agree, "✅"):]))
	t.Logf("ℹ️ 这条只证明『宣称存在』；其名不副实由 AB(p≈0.95 准确率不显著) + TestSolve_Prove_ConfidentButWrong 坐实。")
}

// TestSolve_Prove_ConfidentButWrong 坐实「跑偏」后半（真模型产线 loop）：用**建模陷阱**题
// （答案算术可表达，但审题/列式有坑——code 只接地算术，救不了建模错），跑 solve，统计
// confident-but-wrong = 打了"✅高置信/已核验"却答错。>0 即证明高置信未校准、宣称的"已核验=正确"
// 兑现不了——这就是"拆分=更准确"框架跑偏的硬证据。
func TestSolve_Prove_ConfidentButWrong(t *testing.T) {
	if os.Getenv("HEXCLAW_AB_REAL") != "1" {
		t.Skip("设 HEXCLAW_AB_REAL=1 跑跑偏取证（真引擎）")
	}
	exec, model := newRealEngineExec(t)
	o := NewSolveSkill(exec, nil)
	repeat := getenvInt("HEXCLAW_AB_REPEAT", 3)

	// 建模陷阱：经典 off-by-one / 列式坑。code 算得出，但若审题就错，solver+verifier 会一致地错 → 假高置信。
	probs := []abProb{
		{"一根木头锯成 6 段，每锯断一次用 3 分钟，一共用多少分钟？", "15"},      // 5 刀×3；陷阱 6×3=18
		{"从 1 楼走到 6 楼，每两层之间有 18 级台阶，一共要走多少级台阶？", "90"}, // 5 段×18；陷阱 6×18=108
		{"5 支球队进行单循环赛（每两队各赛一场），一共要赛多少场？", "10"},        // C(5,2)；陷阱 5×4=20
		{"钟面 3 点 15 分时，时针与分针的夹角是多少度？", "7.5"},          // 7.5°；陷阱 0°
		{"睡莲每天面积翻一倍，30 天长满整个池塘，第几天长满一半？", "29"},        // 29；陷阱 15
	}

	confidentWrong, confidentTotal, wrong, total := 0, 0, 0, 0
	for _, p := range probs {
		for r := 0; r < repeat; r++ {
			ctx, cancel := context.WithTimeout(context.Background(), 280*time.Second)
			res, err := o.Execute(ctx, map[string]any{"problem": p.q})
			cancel()
			if err != nil || res == nil {
				continue
			}
			total++
			ans := extractFinalAnswer(res.Content)
			ok := abCorrect(ans, p.gt)
			v := res.Metadata["solve_verdict"]
			flagged := v == "disagree" || v == "unverifiable" ||
				strings.Contains(res.Content, "⚠️") || strings.Contains(res.Content, "复核") || strings.Contains(res.Content, "存疑")
			confident := !flagged // 未标注 = 以"已核验/高置信"姿态端给用户
			if confident {
				confidentTotal++
			}
			if !ok {
				wrong++
			}
			if confident && !ok {
				confidentWrong++
				snippet := strings.ReplaceAll(res.Content, "\n", " ⏎ ")
				if n := len([]rune(snippet)); n > 360 {
					snippet = "…" + string([]rune(snippet)[n-360:])
				}
				t.Logf("🔴 SMOKING GUN [%s]: 答错却端出『✅高置信/已核验』 — 题=%q 抽取答案=%q 正解=%q\n    实际正文尾部: %s", model, p.q, ans, p.gt, snippet)
			}
		}
	}
	t.Logf("=== 跑偏取证 %s：%d trials | 答错 %d | 端出高置信 %d | **高置信却答错(confident-wrong)=%d** ===",
		model, total, wrong, confidentTotal, confidentWrong)
	t.Logf("解读：confident-wrong>0 ⇒『已核验=高置信』未校准、宣称的正确性兑现不了 ⇒『拆分提准确率』框架跑偏；其真实价值是安全(诚实标注)。")
	if total == 0 {
		t.Skip("无有效 trial（引擎/网络）")
	}
	if confidentWrong == 0 {
		t.Logf("⚠️ 本批未抓到 confident-wrong（模型偶然全对或全标注）；加大 HEXCLAW_AB_REPEAT 重跑。非确定性，不硬判失败。")
	}
}
