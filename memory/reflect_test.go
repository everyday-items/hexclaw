package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func globalArchive(t *testing.T, fm *FileMemory) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fm.Dir(), "_global", memoryArchiveFile))
	if err != nil {
		return ""
	}
	return string(data)
}

// B-3：两条同主语异值且**都有效**（绕过写时 supersede，直接落盘）→ 反思时序取代较早者。
// 旧条标 ValidTo 留史（仍在活跃区）、新条带 Supersedes；current-valid 收敛为 1。
func TestReflect_SupersedesContradiction(t *testing.T) {
	fm := newFM(t)
	if err := fm.SaveStructuredEntry("用户住在北京", "fact", "manual", "", EntryMeta{Subject: "居住地"}); err != nil {
		t.Fatal(err)
	}
	if err := fm.SaveStructuredEntry("用户住在上海", "fact", "manual", "", EntryMeta{Subject: "居住地"}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	rep, err := fm.ReflectRole("", now)
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if rep.Superseded != 1 {
		t.Fatalf("应取代 1 条，得 %+v", rep)
	}

	all := fm.ParseEntries()
	if len(all) != 2 {
		t.Fatalf("留史：两条都应在活跃区，得 %d", len(all))
	}
	if v := currentlyValid(all, now); len(v) != 1 || !strings.Contains(v[0].Content, "上海") {
		t.Fatalf("当前有效应仅「上海」，得 %+v", v)
	}
	bj := findByContent(all, "北京")
	if bj == nil || bj.ValidTo == "" {
		t.Fatalf("北京应标 ValidTo 留史，得 %+v", bj)
	}
	if sh := findByContent(all, "上海"); sh == nil || sh.Supersedes == "" {
		t.Fatalf("上海应带 Supersedes，得 %+v", sh)
	}

	// 幂等：已失效的北京下一轮归档；不再产生新 supersede；不报错。
	rep2, err := fm.ReflectRole("", now)
	if err != nil {
		t.Fatalf("reflect#2: %v", err)
	}
	if rep2.Superseded != 0 {
		t.Fatalf("二次反思不应再取代，得 %+v", rep2)
	}
	if rep2.Archived != 1 {
		t.Fatalf("失效历史「北京」应被归档，得 %+v", rep2)
	}
	if v := currentlyValid(fm.ParseEntries(), now); len(v) != 1 {
		t.Fatalf("二次后当前有效仍应为 1，得 %d", len(v))
	}
	if !strings.Contains(globalArchive(t, fm), "北京") {
		t.Fatalf("北京应留史到归档文件")
	}
}

// B-3：近重复（无主语）→ 反思去重删并，保留其一。
func TestReflect_DedupNearDuplicates(t *testing.T) {
	fm := newFM(t)
	_ = fm.SaveStructuredEntry("用户喜欢深色主题界面", "fact", "manual", "", EntryMeta{})
	_ = fm.SaveStructuredEntry("用户喜欢深色主题的界面", "fact", "manual", "", EntryMeta{})

	rep, err := fm.ReflectRole("", time.Now())
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if rep.Deduped != 1 {
		t.Fatalf("应去重 1 条，得 %+v", rep)
	}
	if n := len(fm.ParseEntries()); n != 1 {
		t.Fatalf("去重后应剩 1 条，得 %d", n)
	}
}

// B-3：陈旧低显著性事实（用未来 now 拉低 recency）→ 归档（移到归档文件、活跃区释放）。
func TestReflect_ArchivesStaleFact(t *testing.T) {
	fm := newFM(t)
	_ = fm.SaveStructuredEntry("会议室在三楼", "fact", "manual", "", EntryMeta{})

	future := time.Now().Add(200 * 24 * time.Hour) // recency≈0.01 < 0.05，importance≈0.5 < 0.55
	rep, err := fm.ReflectRole("", future)
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if rep.Archived != 1 {
		t.Fatalf("陈旧事实应归档，得 %+v", rep)
	}
	if n := len(fm.ParseEntries()); n != 0 {
		t.Fatalf("归档后活跃区应空，得 %d", n)
	}
	if !strings.Contains(globalArchive(t, fm), "会议室") {
		t.Fatalf("陈旧事实应留史到归档文件（可恢复，非删除）")
	}
}

// B-3：Pinned 逃生口 —— 即便陈旧也永不被反思移动/归档。
func TestReflect_PinnedSurvives(t *testing.T) {
	fm := newFM(t)
	_ = fm.SaveStructuredEntry("务必周五前交付", "fact", "manual", "", EntryMeta{Pinned: true})

	future := time.Now().Add(365 * 24 * time.Hour)
	rep, err := fm.ReflectRole("", future)
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if rep.Total() != 0 {
		t.Fatalf("Pinned 不应被任何操作触及，得 %+v", rep)
	}
	if n := len(fm.ParseEntries()); n != 1 {
		t.Fatalf("Pinned 应仍在活跃区，得 %d", n)
	}
}

// B-3 单遍重建安全：supersede + dedup + 归档同轮发生，行号不漂移、互不破坏。
func TestReflect_MultiOpSinglePassNoCorruption(t *testing.T) {
	fm := newFM(t)
	// 矛盾对（supersede）
	_ = fm.SaveStructuredEntry("用户时区 UTC+8", "fact", "manual", "", EntryMeta{Subject: "时区"})
	_ = fm.SaveStructuredEntry("用户时区 UTC+9", "fact", "manual", "", EntryMeta{Subject: "时区"})
	// 近重复对（dedup）
	_ = fm.SaveStructuredEntry("项目用 Go 语言开发", "fact", "manual", "", EntryMeta{})
	_ = fm.SaveStructuredEntry("项目用 Go 语言来开发", "fact", "manual", "", EntryMeta{})
	// 独立有效条
	_ = fm.SaveStructuredEntry("数据库是 PostgreSQL", "fact", "manual", "", EntryMeta{})

	now := time.Now()
	rep, err := fm.ReflectRole("", now)
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if rep.Superseded != 1 || rep.Deduped != 1 {
		t.Fatalf("应 supersede 1 + dedup 1，得 %+v", rep)
	}

	all := fm.ParseEntries()
	// 剩：UTC+8(失效留史)+UTC+9(有效) + Go(保留其一) + PostgreSQL = 4 条
	if len(all) != 4 {
		t.Fatalf("单遍重建后应剩 4 条，得 %d: %+v", len(all), contents(all))
	}
	valid := currentlyValid(all, now)
	want := map[string]bool{"UTC+9": false, "Go": false, "PostgreSQL": false}
	for _, e := range valid {
		for k := range want {
			if strings.Contains(e.Content, k) {
				want[k] = true
			}
		}
	}
	for k, hit := range want {
		if !hit {
			t.Fatalf("当前有效应含 %q，实际 valid=%v", k, contents(valid))
		}
	}
	if len(valid) != 3 {
		t.Fatalf("当前有效应为 3 条（UTC+9/Go/PostgreSQL），得 %d: %v", len(valid), contents(valid))
	}
	// 正文零改写：每条仍是原句（反思只搬运/标记）。
	if e := findByContent(all, "PostgreSQL"); e == nil || e.Content != "数据库是 PostgreSQL" {
		t.Fatalf("独立条正文不应被改写，得 %+v", e)
	}
}

// B-4：ReflectAll 覆盖 _global 与每个角色目录。
func TestReflectAll_CoversGlobalAndRoles(t *testing.T) {
	fm := newFM(t)
	// 全局矛盾对
	_ = fm.SaveStructuredEntry("用户住在北京", "fact", "manual", "", EntryMeta{Subject: "居住地"})
	_ = fm.SaveStructuredEntry("用户住在上海", "fact", "manual", "", EntryMeta{Subject: "居住地"})
	// 角色矛盾对（落角色目录）
	_ = fm.SaveStructuredEntry("项目状态进行中", "fact", "manual", "hexbot", EntryMeta{Subject: "项目状态"})
	_ = fm.SaveStructuredEntry("项目状态已完成", "fact", "manual", "hexbot", EntryMeta{Subject: "项目状态"})

	rep, err := fm.ReflectAll(time.Now())
	if err != nil {
		t.Fatalf("ReflectAll: %v", err)
	}
	if rep.Superseded != 2 {
		t.Fatalf("全局 + 角色各取代 1，应共 2，得 %+v", rep)
	}
}

// B-4：StartReflection 生命周期契约（确定性，不依赖墙钟/调度）——
// 返回可用 stop；ctx 取消后 goroutine 退出；stop 幂等可重复调不 panic。
// 循环体本身（每 tick 跑 ReflectAll）由 TestReflectAll_CoversGlobalAndRoles 等确定性用例覆盖，
// 这里只验包装层契约（避免「后台 goroutine 在 N 秒内被调度」式时序假失败）。
func TestStartReflection_Lifecycle(t *testing.T) {
	fm := newFM(t)
	ctx, cancel := context.WithCancel(context.Background())

	// 长间隔：测试期内不触发 tick，行为完全确定。
	stop := fm.StartReflection(ctx, time.Hour)
	if stop == nil {
		t.Fatal("StartReflection 应返回非 nil stop")
	}
	cancel() // ctx 取消 → 后台 goroutine 经 <-loopCtx.Done() 退出
	stop()   // 显式停止
	stop()   // 幂等：再次调用不应 panic（context.CancelFunc 多次调用安全）

	// 短间隔启动再立即停止：验证「起→停」不挂起、不 panic。
	stop2 := fm.StartReflection(context.Background(), time.Millisecond)
	stop2()
}

func contents(es []MemoryEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Content)
	}
	return out
}
