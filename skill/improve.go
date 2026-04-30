// improve.go 实现 v0.4.x Skill 闭环 Phase 3+5：
//
//   - Phase 3：LLM-as-judge 自改进 — 每次执行后采集 trace、评分、低分写 v2 草稿
//   - Phase 5：组合学习 meta-Skill — 高频成功序列合成 meta 草稿
//
// 设计取舍：
//
//   - 评分阈值 < 7 触发 draft（计划 DoD），可调
//   - draft 写到 ~/.hexclaw/skills-drafts/，不直接覆盖原 Skill —— 用户审核后手动 mv
//   - 序列采集只保留最近 N=200 条（内存上限），出现 >= 3 次且成功率 >= 80% 触发 meta
//   - LLM judge 为可选注入：未注入时 Phase 3 跳过评分，但 Phase 5 仍能跑
package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Execution 一次 Skill 调用的快照，由调用方在执行后构造并 Record。
type Execution struct {
	SkillName  string
	UserInput  string
	Output     string
	ThoughtLog []string
	Duration   time.Duration
	Success    bool
	Timestamp  time.Time
}

// JudgeFunc LLM-as-judge 评分函数（0-10 分）。
//
// 由调用方注入，避免 skill 包硬依赖 ai-core/llm。返回非空 reason 时一并写入草稿，
// 用于 SkillWriter 改进时参考。
type JudgeFunc func(exec Execution) (score int, reason string)

// ImproveStore 自改进 + 组合学习中心。线程安全，进程内单例使用。
type ImproveStore struct {
	mu sync.Mutex

	// 草稿输出目录，默认 ~/.hexclaw/skills-drafts
	DraftDir string

	// JudgeFunc 评分注入（可空 → Phase 3 仅记录不评分）
	Judge JudgeFunc

	// LowScoreThreshold 低分阈值，分数 < 该值触发 v2 draft（默认 7）
	LowScoreThreshold int

	// SequenceTriggerCount 序列频次触发（默认 3）
	SequenceTriggerCount int

	// SequenceSuccessRate meta-Skill 触发的最小成功率（默认 0.8）
	SequenceSuccessRate float64

	// 滑动窗口最大保留 Execution 数（默认 200）
	WindowSize int

	executions []Execution
	// 最近一次 SkillSequence 重建时间，用于去抖
	lastSeqEval time.Time
}

// NewImproveStore 用合理默认值创建 store。
func NewImproveStore(draftDir string) *ImproveStore {
	return &ImproveStore{
		DraftDir:             draftDir,
		LowScoreThreshold:    7,
		SequenceTriggerCount: 3,
		SequenceSuccessRate:  0.8,
		WindowSize:           200,
	}
}

// Record 记录一次执行；如配了 Judge 且分数低于阈值，立即写 v2 草稿。
func (s *ImproveStore) Record(exec Execution) error {
	s.mu.Lock()
	s.executions = append(s.executions, exec)
	if w := s.WindowSize; w > 0 && len(s.executions) > w {
		s.executions = s.executions[len(s.executions)-w:]
	}
	judge := s.Judge
	threshold := s.LowScoreThreshold
	dir := s.DraftDir
	s.mu.Unlock()

	if judge == nil || dir == "" {
		return nil
	}
	score, reason := judge(exec)
	if score >= threshold {
		return nil
	}
	return s.writeDraft(exec, score, reason)
}

// writeDraft 把"v2 草稿"以可读 markdown 写到 DraftDir。
func (s *ImproveStore) writeDraft(exec Execution, score int, reason string) error {
	if err := os.MkdirAll(s.DraftDir, 0o755); err != nil {
		return fmt.Errorf("mkdir draft dir: %w", err)
	}
	ts := exec.Timestamp.Format("20060102-150405")
	if exec.Timestamp.IsZero() {
		ts = time.Now().Format("20060102-150405")
	}
	fn := fmt.Sprintf("%s-v2-%s.md", sanitizeName(exec.SkillName), ts)
	path := filepath.Join(s.DraftDir, fn)

	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "name: %s-v2\n", sanitizeName(exec.SkillName))
	fmt.Fprintf(&b, "type: improvement-draft\n")
	fmt.Fprintf(&b, "based_on: %s\n", sanitizeName(exec.SkillName))
	fmt.Fprintf(&b, "generated_at: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "score: %d\n", score)
	fmt.Fprintf(&b, "---\n\n")
	fmt.Fprintf(&b, "# 改进草稿（待用户审核）\n\n")
	fmt.Fprintf(&b, "原 Skill **%s** 在以下场景表现不佳（评分 %d/10）：\n\n", exec.SkillName, score)
	fmt.Fprintf(&b, "## Judge 反馈\n\n%s\n\n", strings.TrimSpace(reason))
	fmt.Fprintf(&b, "## 用户输入\n\n```\n%s\n```\n\n", exec.UserInput)
	fmt.Fprintf(&b, "## 实际输出\n\n```\n%s\n```\n\n", exec.Output)
	if len(exec.ThoughtLog) > 0 {
		fmt.Fprintf(&b, "## 思维轨迹\n\n")
		for _, l := range exec.ThoughtLog {
			fmt.Fprintf(&b, "- %s\n", l)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "## 改进建议\n\n*（请基于以上反馈手动改写 SKILL.md，确认后 mv 到 skills/）*\n")

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// SequenceCandidate 一条候选 meta-Skill：按 (skill_a, skill_b, ...) 顺序出现且成功率高。
type SequenceCandidate struct {
	Steps        []string
	OccurCount   int
	SuccessCount int
	SuccessRate  float64
}

// SuggestMetaSkills 扫描最近窗口内的 Execution 序列，返回符合阈值的候选。
//
// 简化算法：只考虑相邻两步组合（pair）。3-step 及以上可在 v0.5.x 扩展。
func (s *ImproveStore) SuggestMetaSkills() []SequenceCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()

	type stat struct {
		occur, succ int
	}
	pairs := map[[2]string]*stat{}
	for i := 1; i < len(s.executions); i++ {
		prev, curr := s.executions[i-1], s.executions[i]
		// 5 分钟内的相邻调用算同一会话内的序列
		if curr.Timestamp.Sub(prev.Timestamp) > 5*time.Minute {
			continue
		}
		key := [2]string{prev.SkillName, curr.SkillName}
		st := pairs[key]
		if st == nil {
			st = &stat{}
			pairs[key] = st
		}
		st.occur++
		if prev.Success && curr.Success {
			st.succ++
		}
	}

	var out []SequenceCandidate
	for k, v := range pairs {
		if v.occur < s.SequenceTriggerCount {
			continue
		}
		rate := float64(v.succ) / float64(v.occur)
		if rate < s.SequenceSuccessRate {
			continue
		}
		out = append(out, SequenceCandidate{
			Steps:        []string{k[0], k[1]},
			OccurCount:   v.occur,
			SuccessCount: v.succ,
			SuccessRate:  rate,
		})
	}
	return out
}

// WriteMetaDraft 把一条候选写成 meta-Skill 草稿。
func (s *ImproveStore) WriteMetaDraft(c SequenceCandidate) error {
	if s.DraftDir == "" {
		return fmt.Errorf("DraftDir 未配置")
	}
	if err := os.MkdirAll(s.DraftDir, 0o755); err != nil {
		return fmt.Errorf("mkdir draft dir: %w", err)
	}
	name := "meta-" + strings.Join(mapStr(c.Steps, sanitizeName), "-")
	fn := fmt.Sprintf("%s-%s.md", name, time.Now().Format("20060102-150405"))
	path := filepath.Join(s.DraftDir, fn)

	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "type: meta\n")
	fmt.Fprintf(&b, "composes:\n")
	for _, step := range c.Steps {
		fmt.Fprintf(&b, "  - %s\n", step)
	}
	fmt.Fprintf(&b, "occur_count: %d\n", c.OccurCount)
	fmt.Fprintf(&b, "success_rate: %.2f\n", c.SuccessRate)
	fmt.Fprintf(&b, "generated_at: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "---\n\n")
	fmt.Fprintf(&b, "# 组合 Skill 草稿（待用户审核）\n\n")
	fmt.Fprintf(&b, "在最近窗口中观察到序列 `%s` 出现 %d 次，成功率 %.0f%%。\n\n",
		strings.Join(c.Steps, " → "), c.OccurCount, c.SuccessRate*100)
	fmt.Fprintf(&b, "## 建议执行顺序\n\n")
	for i, step := range c.Steps {
		fmt.Fprintf(&b, "%d. 调用 `%s`\n", i+1, step)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "*若高频且效果稳定，请改写为正式 SKILL.md 后 mv 到 skills/。*\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// Snapshot 返回当前窗口内 Execution 副本（测试 / 诊断用）。
func (s *ImproveStore) Snapshot() []Execution {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Execution, len(s.executions))
	copy(out, s.executions)
	return out
}

func sanitizeName(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	var b strings.Builder
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == '_' || r == ' ':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "skill"
	}
	return b.String()
}

func mapStr(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}
