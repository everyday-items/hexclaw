// continuous.go 实现「持续型任务 + 跨 tick 检查点」——桌面版的有界"慢循环"。
//
// 动机：长目标（如"持续整理某主题资料进知识库""逐章梳理一份长文档"）不该在单个 tick 里一次做完
// （会超时、会把 cron 拖成长任务），也不该每个 tick 从头重做。Continuous 任务把长目标摊到多次廉价
// 唤醒上累积推进：每个 tick 读上次的进度档案(checkpoint) → 只做**下一个最小增量** → 写回档案。
//
// 对齐"loop 工程"的纪律（与 multi-agent 的总量闸、supervisor 反馈环同源），有三道硬终止闸，绝不死刷：
//  1. 完成信号：agent 报 TASK_COMPLETE: yes → 目标达成 → 收为 done，停止调度。
//  2. 无进展停滞：连续 maxNoProgressStreak 个 tick 无新进展 → 暂停（防原地空转烧 token）。
//  3. 总量 backstop：推进次数达 maxContinuousTicks → 暂停（防无限推进，对齐 fanoutBudget 思路）。
//
// 检查点存 StateStore（sqlite，per-job KV，重启不丢）——桌面 app 常被强退，进度必须落盘可续。
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// continuousStateKey 是检查点在 StateStore 里的 key（per-job 隔离）。
	continuousStateKey = "continuous_checkpoint"
	// maxContinuousTicks 是单个持续任务的总推进次数硬顶（远高于真实长目标所需，纯防失控 backstop）。
	maxContinuousTicks = 100
	// maxNoProgressStreak 是连续无新进展容忍次数，超过即判停滞收工。
	maxNoProgressStreak = 3
	// continuousHistoryCap 是进度档案保留的最近条数（界 prompt 体积，避免逐 tick 膨胀）。
	continuousHistoryCap = 8
)

// continuousCheckpoint 是一个持续任务的跨 tick 进度档案。
type continuousCheckpoint struct {
	Tick       int      `json:"tick"`        // 已推进次数
	Completed  bool     `json:"completed"`   // 整个长目标是否已完成
	History    []string `json:"history"`     // 最近 N 条进度（注入下次 prompt 的"已完成"上下文）
	NoProgress int      `json:"no_progress"` // 连续无新进展计数
}

// loadContinuousCheckpoint 从 StateStore 读检查点（无 / 解析失败 → 全零的新档案，等于从头开始）。
func (s *Scheduler) loadContinuousCheckpoint(jobID string) continuousCheckpoint {
	var cp continuousCheckpoint
	if s.state == nil {
		return cp
	}
	if raw, ok := s.state.Get(jobID, continuousStateKey); ok {
		_ = json.Unmarshal([]byte(raw), &cp) // 脏数据当作从头开始，不致命
	}
	return cp
}

// saveContinuousCheckpoint 把检查点落盘（落盘失败 loud 记录，不致命——下个 tick 会基于内存态重试）。
func (s *Scheduler) saveContinuousCheckpoint(jobID string, cp continuousCheckpoint) {
	if s.state == nil {
		return
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return
	}
	if err := s.state.Set(jobID, continuousStateKey, string(b)); err != nil {
		slog.Warn("[cron] 持续任务 checkpoint 落盘失败", "source", "cron", "id", jobID, "error", err)
	}
}

// composeContinuousPrompt 把目标 + 进度档案拼成"只做下一个增量"的渐进式 prompt，并要求 agent 回报
// PROGRESS / TASK_COMPLETE 两个控制标记（与 TASK_STATUS 同理，不翻译 ASCII，弱模型也能稳定解析）。
func composeContinuousPrompt(job *Job, cp continuousCheckpoint) string {
	var b strings.Builder
	b.WriteString(job.SourcePrompt)
	b.WriteString("\n\n---\n[持续任务模式 · 第 ")
	b.WriteString(strconv.Itoa(cp.Tick + 1))
	b.WriteString(" 次推进]\n")
	if len(cp.History) == 0 {
		b.WriteString("这是第一次推进，目标尚未开始。请只完成这个长期目标的**第一个最小增量**（一小块），不要试图一次做完。\n")
	} else {
		b.WriteString("你正在分多次推进上面这个长期目标。已完成的进度档案（越靠后越新）：\n")
		for i, h := range cp.History {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, h)
		}
		b.WriteString("请基于已完成的部分，只做**下一个尚未完成的最小增量**——不要重复已做过的，也不要试图一次做完剩余全部。\n")
	}
	b.WriteString("\n完成本次增量后，在回复中另起两行，逐行输出（不要翻译这两个 ASCII 标记）：\n")
	b.WriteString("PROGRESS: <一句话说明你这次具体推进了什么（供进度档案累积）>。若这次确实没能取得任何实质进展（卡住/无新内容可做/已无可推进），请改为输出 PROGRESS: NONE\n")
	b.WriteString("TASK_COMPLETE: yes 若整个长期目标已全部完成，否则 no\n")
	return b.String()
}

// 注意：值前后用「水平空白」类 [ \t]（而非 \s），否则 \s 会吃掉换行、让空 PROGRESS 误抓下一行
// （TASK_COMPLETE 行）当进度——本回归锁 TestContinuous_NoProgressStallPauses 钉死。`.` 默认不跨行。
var (
	continuousProgressRe = regexp.MustCompile("(?im)^[ \\t>*_`-]*PROGRESS[ \\t]*[:：][ \\t]*(.+?)[ \\t]*$")
	continuousCompleteRe = regexp.MustCompile("(?im)^[ \\t>*_`-]*TASK_COMPLETE[ \\t]*[:：][ \\t]*(.+?)[ \\t]*$")
	// 剥离控制行（含空值形式，如裸 "PROGRESS:"），让投递正文干净。
	continuousStripRe = regexp.MustCompile("(?im)^[ \\t>*_`-]*(?:PROGRESS|TASK_COMPLETE)[ \\t]*[:：].*$")
)

// parseContinuousProgress 抠出本 tick 的 PROGRESS 文案与 TASK_COMPLETE 判定，并返回剥掉这两个控制行后的
// 正文（cleaned）——控制标记不进用户通知。
func parseContinuousProgress(content string) (progress string, complete bool, cleaned string) {
	if m := continuousProgressRe.FindStringSubmatch(content); len(m) > 1 {
		progress = strings.TrimSpace(m[1])
	}
	if m := continuousCompleteRe.FindStringSubmatch(content); len(m) > 1 {
		v := strings.ToLower(strings.TrimSpace(m[1]))
		switch {
		case strings.HasPrefix(v, "yes"), strings.HasPrefix(v, "true"),
			strings.HasPrefix(v, "是"), strings.HasPrefix(v, "已完成"), strings.HasPrefix(v, "完成"):
			complete = true
		}
	}
	cleaned = strings.TrimSpace(continuousStripRe.ReplaceAllString(content, ""))
	return progress, complete, cleaned
}

// normalizeProgress 规整进度文案用于"无新进展"判定（小写 + 折叠空白）。
func normalizeProgress(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// noProgressSentinels 是 agent 显式自报"本 tick 无实质进展"的信号（最佳实践：让 agent 直接说，比
// 靠字面去重推断更可靠——换措辞但没真推进时字面去重照样漏，显式信号兜得住）。
var noProgressSentinels = map[string]bool{
	"none": true, "n/a": true, "na": true, "nil": true, "null": true, "-": true,
	"无": true, "没有": true, "无进展": true, "无新进展": true, "没有进展": true,
	"没有新进展": true, "卡住": true, "卡住了": true, "stuck": true, "no progress": true,
}

// isNoProgressSignal 判定一条 PROGRESS 文案是否表示"本 tick 没有实质进展"：空、或显式 NONE 类信号。
func isNoProgressSignal(progress string) bool {
	p := normalizeProgress(progress)
	return p == "" || noProgressSentinels[p]
}

// runContinuousAgentJob 跑持续任务的一个 tick：读档案 → 渐进式 prompt 跑一轮 → 解析进度 → 写回档案 →
// 检查三道终止闸。失败 tick 不推进档案（交既有失败/告警路径处理），成功 tick 才累积进度。
func (s *Scheduler) runContinuousAgentJob(ctx context.Context, job *Job) *RunResult {
	cp := s.loadContinuousCheckpoint(job.ID)

	// 已完成：不再烧 LLM——确保状态收为 done（防被手动恢复后空转），静默记一条历史。
	if cp.Completed {
		s.markContinuousFinished(job.ID, StatusDone)
		return &RunResult{Status: "success", Stdout: "[SILENT] 持续任务已完成，无需再推进。"}
	}

	// 复用 runAgentJobWithPrompt：注入渐进式 prompt，TASK_STATUS 仍由 runAgentJob 解析、ingest 证明仍校验。
	result := s.runAgentJobWithPrompt(ctx, job, composeContinuousPrompt(job, cp))
	if result.Status != "success" {
		return result // 本 tick 执行失败：不推进档案，进度不累积。
	}

	progress, complete, cleaned := parseContinuousProgress(result.Stdout)
	result.Stdout = cleaned // 控制标记不进用户通知

	cp.Tick++
	// 无新进展 = agent 显式自报 NONE/空（首选信号）∨ 与上一条字面重复（兜底）。
	advanced := !isNoProgressSignal(progress) &&
		(len(cp.History) == 0 || normalizeProgress(progress) != normalizeProgress(cp.History[len(cp.History)-1]))
	if advanced {
		cp.History = append(cp.History, progress)
		if len(cp.History) > continuousHistoryCap {
			cp.History = cp.History[len(cp.History)-continuousHistoryCap:]
		}
		cp.NoProgress = 0
	} else {
		cp.NoProgress++
	}
	if complete {
		cp.Completed = true
	}
	s.saveContinuousCheckpoint(job.ID, cp)

	// 三道终止闸：完成信号 / 无进展停滞 / 总量 backstop——任一触发即自动收工（停止后续 tick）。
	switch {
	case cp.Completed:
		s.markContinuousFinished(job.ID, StatusDone)
		s.notify(job, NotifyLevelSuccess, "持续任务已完成",
			fmt.Sprintf("「%s」经 %d 次推进已完成目标。", job.Name, cp.Tick))
	case cp.NoProgress >= maxNoProgressStreak:
		s.markContinuousFinished(job.ID, StatusPaused)
		s.notify(job, NotifyLevelWarning, "持续任务已暂停",
			fmt.Sprintf("「%s」连续 %d 次无新进展，已暂停（可在自动化页面调整目标后恢复）。", job.Name, cp.NoProgress))
	case cp.Tick >= maxContinuousTicks:
		s.markContinuousFinished(job.ID, StatusPaused)
		s.notify(job, NotifyLevelWarning, "持续任务达推进上限",
			fmt.Sprintf("「%s」已推进 %d 次达上限，已暂停（防无限推进）。", job.Name, cp.Tick))
	}
	return result
}

// markContinuousFinished 把持续任务收工：同步内存 + DB 状态（done/paused），使调度循环的
// `Status == StatusActive` 闸不再触发它（停止后续 tick）。
func (s *Scheduler) markContinuousFinished(jobID string, status JobStatus) {
	s.mu.Lock()
	if j, ok := s.jobs[jobID]; ok {
		j.Status = status
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.updateJobStatus(ctx, jobID, status); err != nil {
		slog.Warn("[cron] 持续任务收工状态落盘失败", "source", "cron", "id", jobID, "status", string(status), "error", err)
	}
}
