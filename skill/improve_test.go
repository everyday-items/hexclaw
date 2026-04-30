package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImproveStore_LowScoreWritesDraft(t *testing.T) {
	dir := t.TempDir()
	s := NewImproveStore(dir)
	s.Judge = func(e Execution) (int, string) { return 4, "答案错误，缺少计算过程" }

	exec := Execution{
		SkillName: "math-tutor",
		UserInput: "2x+3=7 求 x",
		Output:    "x=2",
		Timestamp: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
	}
	if err := s.Record(exec); err != nil {
		t.Fatalf("Record: %v", err)
	}

	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("应写 1 个 draft；got %d", len(files))
	}
	if !strings.HasPrefix(files[0].Name(), "math-tutor-v2-") {
		t.Errorf("draft 文件名格式不对：%s", files[0].Name())
	}
	body, _ := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if !strings.Contains(string(body), "score: 4") {
		t.Errorf("draft 应含分数；body=%s", body)
	}
}

func TestImproveStore_HighScoreSkipsDraft(t *testing.T) {
	dir := t.TempDir()
	s := NewImproveStore(dir)
	s.Judge = func(e Execution) (int, string) { return 9, "good" }

	if err := s.Record(Execution{SkillName: "x", Timestamp: time.Now()}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Errorf("高分不应写 draft；got %d", len(files))
	}
}

func TestImproveStore_NoJudgeSkipsScore(t *testing.T) {
	dir := t.TempDir()
	s := NewImproveStore(dir)
	if err := s.Record(Execution{SkillName: "a", Timestamp: time.Now()}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := len(s.Snapshot()); got != 1 {
		t.Errorf("无 judge 也应记录到窗口；got %d", got)
	}
}

func TestImproveStore_SuggestMetaSkills(t *testing.T) {
	dir := t.TempDir()
	s := NewImproveStore(dir)
	base := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	// 序列 (a, b) 出现 3 次，全部成功
	for i := 0; i < 3; i++ {
		t0 := base.Add(time.Duration(i*10) * time.Second)
		_ = s.Record(Execution{SkillName: "a", Success: true, Timestamp: t0})
		_ = s.Record(Execution{SkillName: "b", Success: true, Timestamp: t0.Add(time.Second)})
	}

	cands := s.SuggestMetaSkills()
	if len(cands) != 1 {
		t.Fatalf("应返回 1 条候选；got %d", len(cands))
	}
	if cands[0].Steps[0] != "a" || cands[0].Steps[1] != "b" {
		t.Errorf("候选顺序错；got=%v", cands[0].Steps)
	}
	if cands[0].SuccessRate != 1.0 {
		t.Errorf("成功率应=1；got=%.2f", cands[0].SuccessRate)
	}

	// 写 meta draft
	if err := s.WriteMetaDraft(cands[0]); err != nil {
		t.Fatalf("WriteMetaDraft: %v", err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("应写 1 个 meta draft；got %d", len(files))
	}
	if !strings.HasPrefix(files[0].Name(), "meta-a-b-") {
		t.Errorf("文件名应为 meta-a-b-*：%s", files[0].Name())
	}
}

func TestImproveStore_SequenceFiltersLowSuccess(t *testing.T) {
	dir := t.TempDir()
	s := NewImproveStore(dir)
	base := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	// 3 次序列，2 次失败 → 成功率 33% 应过滤
	for i := 0; i < 3; i++ {
		t0 := base.Add(time.Duration(i*10) * time.Second)
		_ = s.Record(Execution{SkillName: "a", Success: true, Timestamp: t0})
		_ = s.Record(Execution{SkillName: "b", Success: i == 0, Timestamp: t0.Add(time.Second)})
	}
	cands := s.SuggestMetaSkills()
	if len(cands) != 0 {
		t.Errorf("成功率不达标应过滤；got %d", len(cands))
	}
}

func TestImproveStore_WindowTrim(t *testing.T) {
	s := NewImproveStore(t.TempDir())
	s.WindowSize = 3
	for i := 0; i < 10; i++ {
		_ = s.Record(Execution{SkillName: "x", Timestamp: time.Now()})
	}
	if got := len(s.Snapshot()); got != 3 {
		t.Errorf("窗口应保持 3；got %d", got)
	}
}

func TestSanitizeName(t *testing.T) {
	if got := sanitizeName("Math Tutor_v2"); got != "math-tutor-v2" {
		t.Errorf("sanitize 错；got=%s", got)
	}
	if got := sanitizeName("中文名"); got != "skill" {
		t.Errorf("纯中文应兜底为 skill；got=%s", got)
	}
}
