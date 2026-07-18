package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// fakeWorkFeedbackSolver 同时实现 Solver（占位）与 WorkFeedbackGenerator（fake executor），
// 契约测试用：可控输出/可控失败，并记录用例传给 Skill 层的生成请求。
type fakeWorkFeedbackSolver struct {
	feedback   string
	skillStamp string // 方法论基座来源戳（如 "writing-feedback@1.0.0/disk"），可为空
	err        error
	calls      int
	lastReq    usecase.WorkFeedbackRequest
}

func (f *fakeWorkFeedbackSolver) Solve(context.Context, string, string, string) (usecase.SolveResult, error) {
	return usecase.SolveResult{}, nil
}

func (f *fakeWorkFeedbackSolver) GenerateWorkFeedback(_ context.Context, req usecase.WorkFeedbackRequest) (usecase.WorkFeedbackOutput, error) {
	f.calls++
	f.lastReq = req
	return usecase.WorkFeedbackOutput{Feedback: f.feedback, SkillStamp: f.skillStamp}, f.err
}

func newWritingWork(t *testing.T, d usecase.Deps, agent string) string {
	t.Helper()
	id, _, err := d.CreateCreativeWork(context.Background(), agent, "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "《春天的校园》", Task: "观察春景写一段",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "柳枝像绿色的丝带，随风轻轻摆动。"}},
	})
	if err != nil {
		t.Fatalf("创建写作作品: %v", err)
	}
	return id
}

// TestGenerateWorkFeedback_Writing_AI 写作点评生成：draft → feedback_ready，
// 点评落最新版本且来源标记 ai（与家长手写区分）。
func TestGenerateWorkFeedback_Writing_AI(t *testing.T) {
	d := newDataDeps(t)
	gen := &fakeWorkFeedbackSolver{feedback: "「柳枝像绿色的丝带」这个比喻贴切生动；建议在结尾补一个听觉细节，比如风吹过的声音。"}
	d.Solver = gen
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")

	v, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
	if err != nil {
		t.Fatalf("生成点评: %v", err)
	}
	if v.Record.Status != k12.WorkStatusFeedbackReady {
		t.Fatalf("生成点评后应为 feedback_ready，got %s", v.Record.Status)
	}
	last := v.Fields.Versions[len(v.Fields.Versions)-1]
	if last.Feedback != gen.feedback {
		t.Fatalf("点评应写入最新版本，got %q", last.Feedback)
	}
	if last.FeedbackSource != k12.FeedbackSourceAI {
		t.Fatalf("AI 点评来源应为 %q，got %q", k12.FeedbackSourceAI, last.FeedbackSource)
	}
	// 生成请求必须携带作品可见证据（题目要求 + 原文），不发明输入。
	if gen.lastReq.WorkType != k12.WorkTypeWriting || gen.lastReq.Task == "" ||
		!strings.Contains(gen.lastReq.ContentMarkdown, "绿色的丝带") {
		t.Fatalf("生成请求缺少作品证据: %+v", gen.lastReq)
	}
}

// TestGenerateWorkFeedback_SkillStampPersisted 追溯契约：生成器申报的方法论基座来源戳
// 随点评落库到版本记录 feedback_skill 字段（能查到每条点评用的哪版方法论）。
func TestGenerateWorkFeedback_SkillStampPersisted(t *testing.T) {
	cases := []struct{ name, stamp string }{
		{"盘上版本", "writing-feedback@1.2.0/disk"},
		{"内嵌快照", "writing-feedback@1.0.0/embedded"},
		{"硬编码兜底", "builtin"},
		{"未申报为空", ""}, // 前向兼容：老生成器不申报，落空值不猜
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDataDeps(t)
			d.Solver = &fakeWorkFeedbackSolver{
				feedback:   "「柳枝像绿色的丝带」比喻好；建议结尾补一个听觉细节。",
				skillStamp: tc.stamp,
			}
			ctx := context.Background()
			id := newWritingWork(t, d, "xiaoming")
			v, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
			if err != nil {
				t.Fatalf("生成点评: %v", err)
			}
			last := v.Fields.Versions[len(v.Fields.Versions)-1]
			if last.FeedbackSkill != tc.stamp {
				t.Fatalf("feedback_skill 应落库 %q，got %q", tc.stamp, last.FeedbackSkill)
			}
		})
	}
}

// TestAttachFeedback_ParentNoSkillStamp 家长手写点评不经方法论基座 → feedback_skill 恒空。
func TestAttachFeedback_ParentNoSkillStamp(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	v, err := d.AttachFeedback(ctx, "xiaoming", id, "切题；比喻好，可加感官细节。")
	if err != nil {
		t.Fatal(err)
	}
	if v.Fields.Versions[0].FeedbackSkill != "" {
		t.Fatalf("家长手写点评不应带 feedback_skill，got %q", v.Fields.Versions[0].FeedbackSkill)
	}
}

// TestGenerateWorkFeedback_INV011_Rejected INV-011 契约：生成输出出现分数/等第/代写成文
// → 拒绝入库，作品保持原状态、不留假点评。
func TestGenerateWorkFeedback_INV011_Rejected(t *testing.T) {
	cases := []struct {
		name, out string
	}{
		{"打分", "整体不错，可以打 90 分。"},
		{"满分口径", "距离满分只差一点点。"},
		{"等第", "这篇作文可评为优等。"},
		{"代写范文", "范文如下：春天来了，校园里……"},
		{"改写全文", "我帮你改写全文：春天的校园真美……"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDataDeps(t)
			d.Solver = &fakeWorkFeedbackSolver{feedback: tc.out}
			ctx := context.Background()
			id := newWritingWork(t, d, "xiaoming")

			if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err == nil {
				t.Fatalf("输出 %q 违反 INV-011，应拒绝", tc.out)
			}
			v, _ := d.GetCreativeWork(ctx, "xiaoming", id)
			if v.Record.Status != k12.WorkStatusDraft {
				t.Fatalf("拒绝后作品应保持 draft，got %s", v.Record.Status)
			}
			if v.Fields.Versions[0].Feedback != "" || v.Fields.Versions[0].FeedbackSource != "" {
				t.Fatalf("拒绝入库不得残留点评: %+v", v.Fields.Versions[0])
			}
		})
	}
}

// TestGenerateWorkFeedback_FeedbackNotFalselyRejected 正常点评不应被 INV-011 误杀
// （如“10分钟”“百分数”这类含“分”字但非打分的表述）。
func TestGenerateWorkFeedback_FeedbackNotFalselyRejected(t *testing.T) {
	d := newDataDeps(t)
	d.Solver = &fakeWorkFeedbackSolver{feedback: "「柳枝像绿色的丝带」比喻好；建议每天花 10 分钟朗读，结尾再补一个细节。"}
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err != nil {
		t.Fatalf("“10 分钟”不是打分，不应被拒: %v", err)
	}
}

// TestGenerateWorkFeedback_Art_Observational 美术作品：观察描述式点评，
// 生成请求携带创作任务、孩子意图与原图 asset。
func TestGenerateWorkFeedback_Art_Observational(t *testing.T) {
	d := newDataDeps(t)
	gen := &fakeWorkFeedbackSolver{feedback: "画面主体偏右，大片天空的留白让雨后显得安静；地面的倒影再加深一点，安静的感觉会更足。"}
	d.Solver = gen
	ctx := context.Background()
	id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "《雨后的校园》", Task: "画一处雨后场景",
		Intent:   "想画出雨后安静的感觉",
		Versions: []k12.CreativeWorkVersion{{SourceAssetID: "asset-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
	if err != nil {
		t.Fatalf("美术点评生成: %v", err)
	}
	if v.Record.Status != k12.WorkStatusFeedbackReady {
		t.Fatalf("应为 feedback_ready，got %s", v.Record.Status)
	}
	last := v.Fields.Versions[len(v.Fields.Versions)-1]
	if last.FeedbackSource != k12.FeedbackSourceAI {
		t.Fatalf("来源应为 ai，got %q", last.FeedbackSource)
	}
	if gen.lastReq.WorkType != k12.WorkTypeArt || gen.lastReq.Intent == "" || gen.lastReq.SourceAssetID != "asset-1" {
		t.Fatalf("美术生成请求缺少可见证据来源: %+v", gen.lastReq)
	}
}

// TestGenerateWorkFeedback_Art_INV011_Rejected INV-011 拦截对美术输出同样生效：
// 视觉通道生成的点评出现打分/等第/重画口径 → 拒绝入库，作品保持 draft、不留假点评。
func TestGenerateWorkFeedback_Art_INV011_Rejected(t *testing.T) {
	cases := []struct {
		name, out string
	}{
		{"打分", "构图完整，色彩可以打 85 分。"},
		{"等第", "这幅画能评甲等。"},
		{"排名", "在同龄孩子里排名前列。"},
		{"重画", "我来重画一幅给你参考。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDataDeps(t)
			d.Solver = &fakeWorkFeedbackSolver{feedback: tc.out}
			ctx := context.Background()
			id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
				WorkType: k12.WorkTypeArt, Title: "《雨后的校园》", Task: "画一处雨后场景",
				Versions: []k12.CreativeWorkVersion{{SourceAssetID: "asset-1"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err == nil {
				t.Fatalf("美术输出 %q 违反 INV-011，应拒绝", tc.out)
			}
			v, _ := d.GetCreativeWork(ctx, "xiaoming", id)
			if v.Record.Status != k12.WorkStatusDraft ||
				v.Fields.Versions[0].Feedback != "" || v.Fields.Versions[0].FeedbackSource != "" {
				t.Fatalf("拒绝后不得残留点评: status=%s %+v", v.Record.Status, v.Fields.Versions[0])
			}
		})
	}
}

// TestGenerateWorkFeedback_OwnerIsolation 归属隔离：别的实例不能给他人作品生成点评。
func TestGenerateWorkFeedback_OwnerIsolation(t *testing.T) {
	d := newDataDeps(t, "xiaoming", "xiaohong")
	gen := &fakeWorkFeedbackSolver{feedback: "好句在开头；建议结尾具体化。"}
	d.Solver = gen
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	if _, err := d.GenerateWorkFeedback(ctx, "xiaohong", id); err == nil {
		t.Fatal("跨实例生成点评应被拒")
	}
	if gen.calls != 0 {
		t.Fatal("归属校验必须在调用 Skill 之前，不应触发生成")
	}
}

// TestGenerateWorkFeedback_StatusGuard draft/revised 以外状态拒绝生成。
func TestGenerateWorkFeedback_StatusGuard(t *testing.T) {
	d := newDataDeps(t)
	d.Solver = &fakeWorkFeedbackSolver{feedback: "好句：开头比喻；建议：结尾补细节。"}
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")

	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err != nil {
		t.Fatalf("draft 生成: %v", err)
	}
	// 已是 feedback_ready → 再生成应拒（需先提交修改稿）。
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err == nil {
		t.Fatal("feedback_ready 状态不应可再次生成点评")
	}
	// revised 可再点评。
	if _, err := d.SubmitRevision(ctx, "xiaoming", id, "柳枝像绿色的丝带，风一吹沙沙响。", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err != nil {
		t.Fatalf("revised 应可再生成点评: %v", err)
	}
	// archived 拒。
	id2 := newWritingWork(t, d, "xiaoming")
	if err := d.ArchiveCreativeWork(ctx, "xiaoming", id2); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id2); err == nil {
		t.Fatal("archived 不应可生成点评")
	}
}

// TestGenerateWorkFeedback_ExecutorFailureHonest 生成失败：诚实报错，不留假点评。
func TestGenerateWorkFeedback_ExecutorFailureHonest(t *testing.T) {
	d := newDataDeps(t)
	d.Solver = &fakeWorkFeedbackSolver{err: errors.New("provider down")}
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err == nil {
		t.Fatal("生成失败应报错")
	}
	v, _ := d.GetCreativeWork(ctx, "xiaoming", id)
	if v.Record.Status != k12.WorkStatusDraft || v.Fields.Versions[0].Feedback != "" {
		t.Fatalf("失败后不得留下假点评: status=%s %+v", v.Record.Status, v.Fields.Versions[0])
	}
}

// TestGenerateWorkFeedback_GeneratorUnconfigured 未接线生成能力 → 诚实报错。
func TestGenerateWorkFeedback_GeneratorUnconfigured(t *testing.T) {
	d := newDataDeps(t) // Solver 为 nil
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err == nil {
		t.Fatal("未配置点评生成能力应报错")
	}
}

// TestAttachFeedback_ParentSourceMarked 家长手写点评来源标记 parent（与 AI 区分；
// 老数据空值前向兼容由字段 omitempty 保证）。
func TestAttachFeedback_ParentSourceMarked(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	v, err := d.AttachFeedback(ctx, "xiaoming", id, "切题；比喻好，可加感官细节。")
	if err != nil {
		t.Fatal(err)
	}
	if v.Fields.Versions[0].FeedbackSource != k12.FeedbackSourceParent {
		t.Fatalf("家长手写点评来源应为 parent，got %q", v.Fields.Versions[0].FeedbackSource)
	}
}
