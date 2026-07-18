package usecase

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 本文件是 GradingOrchestrator 的单机运行时扩展（架构设计 §6.15）：
//
//   - 阶段中间产物落盘（runDir/<jobID>/image.bin + run.json）：检查点只固化摘要（§5.4），
//     真实产物（原图/识别题目/锚点结论/批改结果）落本地文件——崩溃恢复据此回放，
//     不重复调用模型（§6.7 规则 3 / K12-INV-021）。
//     图片载体设计申报：submission 原图不入 records 的 Fields JSON（base64 过大），
//     以本地文件承载；SubmissionID = "photo-"+sha1(原图) 兼作内容校验（加载时必验）。
//   - 异步执行模型：StartAsync 用与请求解耦的进程级 context + 有界并发信号量 + panic recover
//     推进任务（任务 goroutine 不挂 HTTP 请求 context，请求结束不误杀在途批改）。
//   - 崩溃恢复扫描：RecoverGradingJobs 列非终态 Job，从落盘运行时重建 run；
//     自动阶段重新入列续跑，awaiting_confirmation 保持等待，failed_retryable(可重试) 回队。

// GradingOrchestratorOption 编排器组装可选项。
type GradingOrchestratorOption func(*GradingOrchestrator)

// WithGradingRunDir 启用阶段产物落盘目录（空 = 仅内存 run，崩溃后不可恢复）。
func WithGradingRunDir(dir string) GradingOrchestratorOption {
	return func(o *GradingOrchestrator) { o.runDir = dir }
}

// WithGradingBaseContext 异步推进的基座 context（默认 context.Background()；
// 传进程生命周期 ctx 可让退出信号取消在途阶段）。
func WithGradingBaseContext(ctx context.Context) GradingOrchestratorOption {
	return func(o *GradingOrchestrator) { o.baseCtx = ctx }
}

// WithGradingConcurrency 异步推进的有界并发上限（默认 2，与整卷逐题批改并发一致）。
func WithGradingConcurrency(n int) GradingOrchestratorOption {
	return func(o *GradingOrchestrator) {
		if n > 0 {
			o.sem = make(chan struct{}, n)
		}
	}
}

// StartAsync 异步推进一个 Job 到下一停点/终态。与调用方（HTTP 请求）context 解耦；
// 同一 Job 已在推进中则幂等跳过；panic 不逃逸（§6.15：进程不因单任务崩溃）。
func (o *GradingOrchestrator) StartAsync(jobID string) {
	o.mu.Lock()
	if o.active == nil {
		o.active = map[string]bool{}
	}
	if o.active[jobID] {
		o.mu.Unlock()
		return
	}
	o.active[jobID] = true
	o.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("K12 批改任务异步推进 panic（已捕获，任务留在当前落库状态）", "job", jobID, "panic", r)
			}
			o.mu.Lock()
			delete(o.active, jobID)
			o.mu.Unlock()
		}()
		o.sem <- struct{}{}
		defer func() { <-o.sem }()
		ctx := o.baseCtx
		if ctx == nil {
			ctx = context.Background()
		}
		if _, err := o.RunGradingJob(ctx, jobID); err != nil {
			// 阶段失败已由状态机安全落 failed_retryable/failed_terminal；此处仅取证。
			slog.Warn("K12 批改任务异步推进结束于错误（状态机已落库对应失败态）", "job", jobID, "err", err)
		}
	}()
}

// GradingQuestionCorrection 家长对识别结果的逐题确认/修正（§6.7 公共命令③的结构化载荷）。
// 空字段 = 该维度按识别结果确认不改。
type GradingQuestionCorrection struct {
	Index         int         `json:"index"`
	Question      string      `json:"question,omitempty"`
	StudentAnswer string      `json:"student_answer,omitempty"`
	AnswerState   AnswerState `json:"answer_state,omitempty"`
	Subject       string      `json:"subject,omitempty"`
}

// ConfirmPhotoGradingInput 桌面确认输入：整卷学科/年级 + 逐题修正。
type ConfirmPhotoGradingInput struct {
	Subject     string
	Grade       string
	Corrections []GradingQuestionCorrection
}

// ConfirmPhotoGradingJob 结构化确认（桌面入口）：把家长修正应用到已固化的识别产物
// （确认冻结 canonical 输入，§6.7 规则 1/6 语义），落确认检查点后**异步**续跑到终态。
// ok=false 表示该 Job 无在途运行时（非编排器创建），调用方回退纯状态机确认。
func (o *GradingOrchestrator) ConfirmPhotoGradingJob(ctx context.Context, jobID string, in ConfirmPhotoGradingInput) (GradingJobView, bool, error) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return GradingJobView{}, false, nil
	}
	l := o.jobLock(jobID)
	l.Lock()
	applyGradingCorrections(run, in)
	if perr := o.persistRun(jobID, run); perr != nil {
		l.Unlock()
		return GradingJobView{}, true, fmt.Errorf("usecase: 固化确认后的识别产物: %w", perr)
	}
	v, err := o.deps.ConfirmGradingJob(ctx, run.agentName, jobID, gradingCorrectionStrings(in))
	l.Unlock()
	if err != nil {
		return GradingJobView{}, true, err
	}
	o.StartAsync(jobID)
	return v, true, nil
}

// RetryPhotoGradingJob 安全重试（桌面入口）：回 queued 后**异步**从检查点续跑。
// ok=false 表示无在途运行时，调用方回退纯状态机重试。
func (o *GradingOrchestrator) RetryPhotoGradingJob(ctx context.Context, jobID string) (GradingJobView, bool, error) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return GradingJobView{}, false, nil
	}
	l := o.jobLock(jobID)
	l.Lock()
	v, err := o.deps.RetryGradingJob(ctx, run.agentName, jobID)
	l.Unlock()
	if err != nil {
		return GradingJobView{}, true, err
	}
	o.StartAsync(jobID)
	return v, true, nil
}

// RecognizedQuestions 取识别停点产物（锚点已回位时含 BBox），供确认界面回显。
func (o *GradingOrchestrator) RecognizedQuestions(ctx context.Context, jobID string) ([]RecognizedQuestion, bool) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return nil, false
	}
	l := o.jobLock(jobID)
	l.Lock()
	defer l.Unlock()
	if len(run.questions) == 0 {
		return nil, false
	}
	qs := run.questions
	if run.anchored != nil {
		qs = run.anchored
	}
	return append([]RecognizedQuestion(nil), qs...), true
}

// RecoverGradingJobs 启动扫描（§6.15 崩溃恢复）：逐实例列非终态 GradingJob，从落盘运行时
// 重建 run 并按阶段处置——awaiting_confirmation（锚点已回位）保持等待；failed_retryable
// 且可重试回 queued 重新入列；其余自动阶段直接异步续跑（RunGradingJob 从当前 stage 起，
// 已固化检查点的阶段回放产物，不重复调模型）。恢复不产生重复 Submission（同 Job 续跑）。
func (o *GradingOrchestrator) RecoverGradingJobs(ctx context.Context, agents []string) (int, error) {
	if o.runDir == "" {
		return 0, nil
	}
	recovered := 0
	for _, agent := range agents {
		views, err := o.deps.ListGradingJobs(ctx, agent, "")
		if err != nil {
			return recovered, fmt.Errorf("usecase: 崩溃恢复扫描列任务(%s): %w", agent, err)
		}
		for _, v := range views {
			if k12.GradingStageTerminal(v.Record.Status) {
				continue
			}
			jobID := v.Record.RecordID
			run, err := o.ensureRun(ctx, jobID)
			if err != nil {
				slog.Warn("K12 批改任务崩溃恢复: 运行时产物缺失/校验失败，任务留在当前状态等人工处置",
					"job", jobID, "stage", v.Record.Status, "err", err)
				continue
			}
			recovered++
			switch v.Record.Status {
			case k12.GradingStageAwaitingConfirmation:
				// 等待态不动（等家长）；锚点增强分支未回位时仅补跑锚点后停。
				if v.Fields.AnchorState == k12.GradingAnchorPending {
					o.StartAsync(jobID)
				}
			case k12.GradingStageFailedRetryable:
				if !v.Fields.Retryable {
					continue // 不可重试残留（正常应已收敛 failed_terminal）：留人工处置
				}
				l := o.jobLock(jobID)
				l.Lock()
				_, rerr := o.deps.RetryGradingJob(ctx, run.agentName, jobID)
				l.Unlock()
				if rerr != nil {
					slog.Warn("K12 批改任务崩溃恢复: 重新入列失败", "job", jobID, "err", rerr)
					continue
				}
				o.StartAsync(jobID)
			default:
				o.StartAsync(jobID)
			}
		}
	}
	return recovered, nil
}

// --- 家长修正应用 ---

// applyGradingCorrections 把逐题修正同时应用到核心识别产物与锚点增强产物（assessing 消费
// 的是两者回放，必须同步改）；整卷 subject/grade 覆盖批改请求。
func applyGradingCorrections(run *gradingRun, in ConfirmPhotoGradingInput) {
	if strings.TrimSpace(in.Subject) != "" {
		run.req.Subject = in.Subject
	}
	if strings.TrimSpace(in.Grade) != "" {
		run.req.Grade = in.Grade
	}
	for _, c := range in.Corrections {
		for _, list := range [][]RecognizedQuestion{run.questions, run.anchored} {
			if c.Index < 0 || c.Index >= len(list) {
				continue
			}
			q := &list[c.Index]
			if strings.TrimSpace(c.Question) != "" {
				q.Question = c.Question
			}
			if c.StudentAnswer != "" {
				q.StudentAnswer = c.StudentAnswer
				q.AnswerState = AnswerStatePresent
			}
			if c.AnswerState != "" {
				q.AnswerState = c.AnswerState
				if c.AnswerState != AnswerStatePresent {
					q.StudentAnswer = ""
				}
			}
			if strings.TrimSpace(c.Subject) != "" {
				q.Subject = c.Subject
			}
			*q = NormalizeRecognizedQuestion(*q)
		}
	}
}

// gradingCorrectionStrings 结构化修正 → 确认检查点摘要输入（canonical 串，稳定可复算）。
func gradingCorrectionStrings(in ConfirmPhotoGradingInput) []string {
	if in.Subject == "" && in.Grade == "" && len(in.Corrections) == 0 {
		return nil
	}
	out := make([]string, 0, len(in.Corrections)+1)
	out = append(out, fmt.Sprintf("sheet|%s|%s", in.Subject, in.Grade))
	for _, c := range in.Corrections {
		out = append(out, fmt.Sprintf("%d|%s|%s|%s|%s", c.Index, c.Question, c.StudentAnswer, c.AnswerState, c.Subject))
	}
	return out
}

// --- 运行时落盘 ---

// gradingRunFile run.json 结构（原图独立存 image.bin，避免每次改写都重写大字节）。
type gradingRunFile struct {
	AgentName     string               `json:"agent_name"`
	Subject       string               `json:"subject,omitempty"`
	Grade         string               `json:"grade,omitempty"`
	SourceSession string               `json:"source_session,omitempty"`
	Questions     []RecognizedQuestion `json:"questions,omitempty"`
	Anchored      []RecognizedQuestion `json:"anchored,omitempty"`
	AnchorFailed  bool                 `json:"anchor_failed,omitempty"`
	RenderFailure string               `json:"render_failure,omitempty"`
	Result        *PhotoGradeResult    `json:"result,omitempty"`
}

func (o *GradingOrchestrator) runPath(jobID string, file string) string {
	return filepath.Join(o.runDir, jobID, file)
}

// persistRun 落盘运行时状态（runDir 未启用时为 no-op）。原图只写一次（内容不变）。
func (o *GradingOrchestrator) persistRun(jobID string, run *gradingRun) error {
	if o.runDir == "" {
		return nil
	}
	dir := filepath.Join(o.runDir, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("建运行时目录: %w", err)
	}
	imgPath := o.runPath(jobID, "image.bin")
	if _, err := os.Stat(imgPath); err != nil {
		if werr := atomicWriteFile(imgPath, run.req.Image); werr != nil {
			return fmt.Errorf("固化原图: %w", werr)
		}
	}
	meta := gradingRunFile{
		AgentName: run.agentName, Subject: run.req.Subject, Grade: run.req.Grade,
		SourceSession: run.req.SourceSession,
		Questions:     run.questions, Anchored: run.anchored, AnchorFailed: run.anchorFailed,
		RenderFailure: run.renderFailure, Result: run.result,
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal 运行时状态: %w", err)
	}
	if err := atomicWriteFile(o.runPath(jobID, "run.json"), raw); err != nil {
		return fmt.Errorf("固化运行时状态: %w", err)
	}
	return nil
}

// ensureRun 取（或从落盘恢复）Job 的运行时状态。
func (o *GradingOrchestrator) ensureRun(ctx context.Context, jobID string) (*gradingRun, error) {
	if run := o.lookup(jobID); run != nil {
		return run, nil
	}
	if o.runDir == "" {
		return nil, fmt.Errorf("usecase: 批改任务 %s 无在途运行时状态（未启用落盘恢复）", jobID)
	}
	raw, err := os.ReadFile(o.runPath(jobID, "run.json"))
	if err != nil {
		return nil, fmt.Errorf("usecase: 批改任务 %s 运行时状态不可读: %w", jobID, err)
	}
	var meta gradingRunFile
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("usecase: 批改任务 %s 运行时状态损坏: %w", jobID, err)
	}
	image, err := os.ReadFile(o.runPath(jobID, "image.bin"))
	if err != nil {
		return nil, fmt.Errorf("usecase: 批改任务 %s 原图不可读: %w", jobID, err)
	}
	// 内容校验：SubmissionID = photo-sha1(原图)，防落盘文件被替换/半写。
	v, err := o.deps.GetGradingJob(ctx, meta.AgentName, jobID)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(image)
	if want := "photo-" + hex.EncodeToString(sum[:]); v.Fields.SubmissionID != want {
		return nil, fmt.Errorf("usecase: 批改任务 %s 原图校验失败（submission=%s 实测=%s）", jobID, v.Fields.SubmissionID, want)
	}
	run := &gradingRun{
		agentName: meta.AgentName,
		req: PhotoGradeRequest{
			AgentName: meta.AgentName, Subject: meta.Subject, Grade: meta.Grade,
			SourceSession: meta.SourceSession, Image: image,
		},
		questions: meta.Questions, anchored: meta.Anchored, anchorFailed: meta.AnchorFailed,
		renderFailure: meta.RenderFailure, result: meta.Result,
	}
	o.mu.Lock()
	if existing, ok := o.runs[jobID]; ok {
		run = existing // 并发恢复竞态：先注册者赢
	} else {
		o.runs[jobID] = run
	}
	o.mu.Unlock()
	return run, nil
}

// releaseRunFiles 删除落盘运行时（投递完成/终态清理）。
func (o *GradingOrchestrator) releaseRunFiles(jobID string) {
	if o.runDir == "" || strings.TrimSpace(jobID) == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(o.runDir, jobID))
}

// atomicWriteFile 先写临时文件再原子改名，防中断留半文件（§6.15 不允许阶段产物半写）。
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
