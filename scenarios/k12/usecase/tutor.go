package usecase

import (
	"context"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 深度对话编排（PRD §3.3.4 渐进提示三阶段 + §3.3.4-6 情绪守门）。
//
// 架构：这是**领域行为**（"该给第几阶段的提示 / 要不要先安抚"），是确定性状态机，落在
// 场景包 usecase，不塞进 engine/solve（评审 P0-2：给 engine 解耦，K12 领域行为不回灌内核）。
// 本层只产出**分阶段指令**（PromptHint）+ 守门标志，交给上游把指令注入 LLM 系统提示生成
// 面向家长的话术；阶段三的完整讲解另经 Solver 验算链取得可信解（不给未验证答案）。

// TutorStage 渐进提示阶段（PRD §3.3.4-4）。
type TutorStage int

const (
	StageHint1 TutorStage = 1 // 方向提示：1~2 条方向性提示，不含具体数字/算式
	StageHint2 TutorStage = 2 // 具体提示：更具体，含批改（家长报了孩子作答时）
	StageFull  TutorStage = 3 // 完整讲解：分步讲解 + 关键步标注 + 变式题
)

// TutorTurnRequest 一轮辅导输入信号（确定性驱动阶段推进 + 情绪守门）。
type TutorTurnRequest struct {
	AgentName     string     // 归属实例（T2.4：阶段三讲解完成时据此 + 题目定位错题推进 explained）
	PriorStage    TutorStage // 上一轮阶段（0 = 尚未开始，起于阶段一）
	ParentMessage string     // 家长本轮消息（检测升级/情绪/孩子作答意图）
	StudentAnswer string     // 家长转述的孩子作答（非空 → 至少进阶段二批改）
}

// TutorDirective 一轮辅导的编排结论（喂给上游注入 LLM 提示）。
type TutorDirective struct {
	Stage      TutorStage `json:"stage"`
	Comfort    bool       `json:"comfort"`     // 情绪守门命中：先安抚、暂停解题推进
	EmotionCue string     `json:"emotion_cue"` // 命中的情绪信号词（可空）
	Escalated  bool       `json:"escalated"`   // 本轮较上轮是否升级
	PromptHint string     `json:"prompt_hint"` // 分阶段（或安抚）行为指令，注入系统提示
}

// 情绪信号词（PRD §3.3.4-6：孩子哭了/生气/烦了 → 切安抚，暂停解题推进）。
var emotionCues = []string{
	"哭了", "哭", "生气", "发火", "发脾气", "闹脾气", "烦了", "不耐烦", "急了",
	"不想学", "崩溃", "摔", "闹", "急哭", "委屈", "沮丧",
}

// 直接要完整讲解的信号（跳到阶段三）。
var fullRequestCues = []string{
	"直接讲", "完整讲解", "看讲解", "讲一遍", "给答案", "直接说答案", "别提示了", "直接看",
}

// 请求更具体提示 / 表示不会（阶段 +1）。
var advanceCues = []string{
	"不会", "不懂", "没懂", "还不会", "还是不会", "再提示", "多提示", "更具体", "提示多点",
}

// PlanTutorTurn 是渐进提示 + 情绪守门的确定性核心。纯函数，无 I/O，完全可单测。
func PlanTutorTurn(req TutorTurnRequest) TutorDirective {
	msg := req.ParentMessage

	// 情绪守门优先：命中即切安抚，**不推进阶段**（暂停解题推进，PRD §3.3.4-6）。
	if cue := firstHit(msg, emotionCues); cue != "" {
		stage := req.PriorStage
		if stage == 0 {
			stage = StageHint1
		}
		return TutorDirective{
			Stage:      stage,
			Comfort:    true,
			EmotionCue: cue,
			PromptHint: "孩子情绪不好了，先别急着讲题：共情安抚孩子的感受，肯定他已经付出的努力，把题目拆成更小的步骤、降低难度，等孩子平静下来再继续。这一轮不推进解题。",
		}
	}

	prior := req.PriorStage
	if prior == 0 {
		prior = StageHint1
	}
	stage := prior
	switch {
	case firstHit(msg, fullRequestCues) != "":
		stage = StageFull
	case req.StudentAnswer != "": // 家长报了孩子作答 → 进入批改（至少阶段二）
		if stage < StageHint2 {
			stage = StageHint2
		}
	case firstHit(msg, advanceCues) != "":
		if stage < StageFull {
			stage++
		}
	}

	escalated := req.PriorStage != 0 && stage > req.PriorStage
	return TutorDirective{
		Stage:      stage,
		Escalated:  escalated,
		PromptHint: stagePromptHint(stage, req.StudentAnswer != ""),
	}
}

// stagePromptHint 返回某阶段注入 LLM 的行为指令（PRD §3.3.4-4 三阶段口径）。
func stagePromptHint(stage TutorStage, hasStudentAnswer bool) string {
	switch stage {
	case StageHint1:
		return "只给 1~2 条方向性提示，不要出现具体数字或算式，结尾鼓励孩子先自己动手试一试。默认不直接给答案。"
	case StageHint2:
		if hasStudentAnswer {
			return "家长报了孩子的作答：先独立解出正确答案再逐步对比；孩子对了就肯定并追问一句思路（防蒙对），错了就指出第一个出错的步骤和错因，但**不要**直接给出正确答案，给家长一句引导话术让孩子自己发现。"
		}
		return "给更具体一点的提示（可以点到关键方法或第一步怎么起），但仍**不要**直接给出完整答案，留一步让孩子自己走。"
	case StageFull:
		return "现在给完整分步讲解：分步骤写清楚、标注关键步，讲完再留一道同知识点的变式题让孩子巩固。"
	default:
		return ""
	}
}

// firstHit 返回 s 中命中的第一个关键词（按 cues 顺序）；无命中返回空串。
func firstHit(s string, cues []string) string {
	for _, c := range cues {
		if strings.Contains(s, c) {
			return c
		}
	}
	return ""
}

// TutorTurnResult 辅导一轮的产物：编排指令 +（阶段三）验算过的完整解。
type TutorTurnResult struct {
	Directive TutorDirective
	Solution  string        // 仅阶段三填：经 Solver 验算链的完整解
	Evidence  SolveEvidence // 验算证据（阶段三）
}

// TutorTurn 计算本轮编排；阶段三且给了题目时，调 Solver 验算链取可信完整解
// （渐进提示不给未验证答案；阶段一二只出指令，由上游 LLM 生成话术）。
func (d Deps) TutorTurn(ctx context.Context, req TutorTurnRequest, problem, grade string) (TutorTurnResult, error) {
	dir := PlanTutorTurn(req)
	res := TutorTurnResult{Directive: dir}
	// 仅在进入完整讲解且未被情绪守门暂停、且有题目时，取验算过的解。
	if dir.Stage == StageFull && !dir.Comfort && strings.TrimSpace(problem) != "" && d.Solver != nil {
		sr, err := d.Solver.Solve(ctx, problem, grade, d.constraintFor(ctx, grade))
		if err != nil {
			return res, err
		}
		res.Solution = sr.Solution
		res.Evidence = sr.Evidence
		// T2.4：完整讲解完成 → 若该题在错题本(new) 自动推进 explained（PRD §5.3.1）。
		// best-effort：推进失败不影响辅导产物（讲解已给出）。
		d.advanceMistakeToExplained(ctx, req.AgentName, problem)
	}
	return res, nil
}

// advanceMistakeToExplained 讲解完成时把同题的 new 错题推进 explained（按题干规范化匹配定位；
// tutor 不带 source_session，故用「同实例内题干规范化相等的 new 错题」匹配，取第一条推进）。
func (d Deps) advanceMistakeToExplained(ctx context.Context, agentName, problem string) {
	if d.Records == nil || agentName == "" || strings.TrimSpace(problem) == "" {
		return
	}
	news, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, k12.StatusNew)
	if err != nil {
		return
	}
	want := k12.NormalizeQuestion(problem)
	for _, r := range news {
		f, _ := k12.ParseMistakeFields(r.Fields)
		if k12.NormalizeQuestion(f.Question) == want {
			_ = d.Records.UpdateStatus(ctx, r.RecordID, k12.StatusExplained, r.DueAt, r.Version)
			return
		}
	}
}
