package cron

// BUG-20260704「no items found in data structure」：定时任务「百度热搜 TOP20 采集」
// 编译出的脚本臆造解析路径（hotList / cateList[0].items / content.list —— 真机取证
// 真实结构是 s-data → data.data.cards[0].content[].word），每 tick 报错；自愈触发后
// 重编译依旧报同一个错（实机 run 历史：error×3 → healed → error）。三层缺陷叠加：
//
//  1. 确定性百度模板门槛过窄 —— compile_templates.go 只认「百度热搜+知识库+写入动词」
//     prompt；「百度热搜 TOP20 采集」这类纯采集 prompt 漏网，落入 LLM 盲猜路径，而
//     真实页面结构 LLM 不可知。
//  2. 自愈重编译 prompt 不带失败脚本 —— maybeSelfHeal 只拼 SourcePrompt+错误文案，
//     LLM 不知道哪些解析路径已被证伪，只能重复盲猜。
//  3. 编译契约不要求提取失败自描述 —— 脚本只 emit「no items found in data structure」
//     这类无线索错误，自愈拿不到真实结构证据（如已解码 dict 的 keys），永不收敛。
//
// 本文件按缺陷逐条锁定；LiveTop20 用真实 top.baidu.com 端到端验证采集模板可用
// （网络门禁：-short 跳过）。

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

// --- 缺陷 1：纯采集 prompt 必须命中确定性模板（不再走 LLM 盲猜） ---

func TestBug20260704_CollectPromptHitsDeterministicTemplate(t *testing.T) {
	// 实机复现用的原始 prompt（E2E 库 cron-zAHFWADE.source_prompt 逐字拷贝）。
	spec := deterministicCompiledSpec("百度热搜 TOP20 采集")
	if spec == nil {
		t.Fatal("[BUG-20260704] 纯采集 prompt「百度热搜 TOP20 采集」必须命中确定性模板，不得落入 LLM 盲猜路径")
	}
	if spec.Runtime != RuntimeStarlark {
		t.Fatalf("模板 runtime 应为 starlark，got %q", spec.Runtime)
	}
	// 模板必须走真机取证的稳定路径：s-data → cards → content → word。
	for _, marker := range []string{"s-data", "cards", "word"} {
		if !strings.Contains(spec.Script, marker) {
			t.Errorf("[BUG-20260704] 采集模板缺少稳定解析路径标记 %q", marker)
		}
	}
	// 纯采集 prompt 没有要求写知识库 —— 不得静默 kb_ingest。
	if strings.Contains(spec.Script, "kb_ingest") {
		t.Error("[BUG-20260704] 纯采集模板不得静默写入知识库（用户没要求入库）")
	}
	// 模板必须通过引擎校验（与 CompileWithProgress 的内置模板校验同一闸）。
	if err := NewStarlarkEngine().Validate(spec.Script); err != nil {
		t.Fatalf("采集模板必须通过 StarlarkEngine.Validate：%v", err)
	}
}

func TestBug20260704_TemplateGateVariants(t *testing.T) {
	cases := []struct {
		prompt   string
		wantHit  bool
		wantKB   bool
		scenario string
	}{
		{"百度热搜 TOP20 采集", true, false, "实机失败 prompt（纯采集）"},
		{"每天 23:30 抓取百度热搜前 20 条", true, false, "采集动词变体"},
		{"获取百度热搜榜单", true, false, "获取+榜单"},
		{"每天采集百度热搜写入知识库", true, true, "原有 KB 路径必须保持命中且仍入库"},
		{"采集微博热搜", false, false, "非百度数据源不得误命中"},
		{"百度热搜是什么", false, false, "无采集意图不得命中"},
	}
	for _, c := range cases {
		spec := deterministicCompiledSpec(c.prompt)
		if (spec != nil) != c.wantHit {
			t.Errorf("[BUG-20260704] %s：prompt=%q 模板命中=%v，期望 %v", c.scenario, c.prompt, spec != nil, c.wantHit)
			continue
		}
		if spec == nil {
			continue
		}
		hasKB := strings.Contains(spec.Script, "kb_ingest")
		if hasKB != c.wantKB {
			t.Errorf("[BUG-20260704] %s：prompt=%q kb_ingest=%v，期望 %v", c.scenario, c.prompt, hasKB, c.wantKB)
		}
	}
}

// --- 缺陷 2：自愈重编译 prompt 必须携带已失败脚本 ---

// promptCapturingCompiler 记录 maybeSelfHeal 递给 compiler 的完整 prompt。
type promptCapturingCompiler struct {
	captured string
}

func (c *promptCapturingCompiler) Compile(_ context.Context, prompt string, _ CompileHints) (*JobSpec, error) {
	c.captured = prompt
	return minimalSpec(), nil
}

func insertBug20260704FailedRun(t *testing.T, db *sql.DB, jobID string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO cron_job_runs (job_id, status, result, error, duration_ms, run_at, stdout, stderr, exit_code, data_json)
		 VALUES (?, 'error', '', 'no items found in data structure', 900, ?, '', '', 0, 'null')`,
		jobID, at); err != nil {
		t.Fatalf("插入失败 run: %v", err)
	}
}

func TestBug20260704_HealPromptCarriesFailingScript(t *testing.T) {
	db := setupTestDB(t)
	comp := &promptCapturingCompiler{}
	exec := NewScriptExecutor().WithWorkdir(t.TempDir())
	s := NewScheduler(db, comp, exec)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}

	// 复刻实机失败脚本的特征解析路径 —— 自愈 prompt 必须把它带给 LLM。
	failingScript := `def run():
    resp = http_get("https://top.baidu.com/board?tab=realtime", headers = {"User-Agent": "UA"})
    data = json_decode(resp["body"])
    items = data.get("hotList")
    if items == None:
        return {"status": "error", "error": "no items found in data structure"}
    return {"status": "success", "data": {"count": len(items)}}
emit(run())`
	job := &Job{
		Name: "百度热搜 TOP20 采集", Type: JobTypeCron, Schedule: "@daily",
		UserID: "u1", Status: StatusActive, SourcePrompt: "某站热搜采集",
		Spec: &JobSpec{Runtime: RuntimeStarlark, Script: failingScript, TimeoutSec: 60},
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("add job: %v", err)
	}
	now := time.Now()
	for i := 3; i >= 1; i-- {
		insertBug20260704FailedRun(t, db, job.ID, now.Add(-time.Duration(i)*time.Minute))
	}

	s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "no items found in data structure"})

	if comp.captured == "" {
		t.Fatal("自愈应触发重编译（前置条件未满足，测试基建问题）")
	}
	if !strings.Contains(comp.captured, "no items found in data structure") {
		t.Error("自愈 prompt 应包含最近错误文案（现状已有，回归保护）")
	}
	if !strings.Contains(comp.captured, `data.get("hotList")`) {
		t.Errorf("[BUG-20260704] 自愈重编译 prompt 必须携带已失败脚本正文（否则 LLM 重复盲猜同类解析路径）。captured:\n%s", comp.captured)
	}
}

// --- 缺陷 3：编译契约必须要求「提取失败自描述」（错误里带结构线索） ---

func TestBug20260704_CompilePromptMandatesSelfDescribingErrors(t *testing.T) {
	p := buildCompileSystemPrompt(CompileHints{})
	if !strings.Contains(p, "结构线索") {
		t.Error("[BUG-20260704] 编译 system prompt 必须包含「提取失败自描述」契约（错误需带结构线索），否则自愈拿不到证据永不收敛")
	}
	if !strings.Contains(p, "top-level keys") {
		t.Error("[BUG-20260704] 编译 system prompt 的范例必须演示 keys 证据模式（top-level keys），LLM 才会照做")
	}
}

// --- 端到端：采集模板打真实百度热搜（网络门禁） ---

func TestBug20260704_BaiduCollectTemplate_LiveTop20(t *testing.T) {
	if os.Getenv("HEX_LIVE_BAIDU_COLLECT_E2E") != "1" {
		t.Skip("需要当前执行轮的真实公网授权；设置 HEX_LIVE_BAIDU_COLLECT_E2E=1 后才运行真实百度 E2E")
	}
	if testing.Short() {
		t.Skip("-short：跳过真实网络 E2E")
	}
	spec := deterministicCompiledSpec("百度热搜 TOP20 采集")
	if spec == nil {
		t.Fatal("[BUG-20260704] 采集 prompt 未命中确定性模板（见 CollectPromptHitsDeterministicTemplate）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	res, err := NewStarlarkEngine().Execute(ctx, spec)
	if err != nil {
		t.Fatalf("引擎执行失败: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("[BUG-20260704] 采集模板打真实百度必须成功，got status=%q error=%q", res.Status, res.Error)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("成功结果 data 应为对象，got %T", res.Data)
	}
	count := 0
	switch v := data["count"].(type) {
	case float64:
		count = int(v)
	case int64:
		count = int(v)
	case int:
		count = v
	}
	if count < 10 {
		t.Errorf("[BUG-20260704] 热搜条目数异常：count=%v（期望 ≥10，正常为 20）", data["count"])
	}
	msg, _ := data["message"].(string)
	if !strings.Contains(msg, "1. ") {
		t.Errorf("[BUG-20260704] 投递正文（data.message）应含编号榜单，got 前 120 字符：%q", clipForHeal(msg, 120))
	}
}
