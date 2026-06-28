package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/memory"
)

func newManageMem(t *testing.T) (*ManageMemorySkill, *memory.FileMemory) {
	t.Helper()
	fm, err := memory.New(memory.Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 200})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	return NewManageMemorySkill(fm), fm
}

func mmRun(t *testing.T, s *ManageMemorySkill, args map[string]any) string {
	t.Helper()
	res, err := s.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("execute %v: %v", args, err)
	}
	return res.Content
}

func TestManageMemory_AddAndDedup(t *testing.T) {
	s, fm := newManageMem(t)
	mmRun(t, s, map[string]any{"action": "add", "content": "用户喜欢深色主题", "type": "preference"})
	if n := len(fm.ParseEntries()); n != 1 {
		t.Fatalf("add 后应 1 条，得 %d", n)
	}
	out := mmRun(t, s, map[string]any{"action": "add", "content": "用户喜欢深色主题", "type": "preference"})
	if n := len(fm.ParseEntries()); n != 1 {
		t.Fatalf("重复 add 不应增，得 %d", n)
	}
	if !strings.Contains(out, "已存在") {
		t.Fatalf("重复应提示已存在，得 %q", out)
	}
}

func TestManageMemory_AddSubjectSupersede(t *testing.T) {
	s, fm := newManageMem(t)
	mmRun(t, s, map[string]any{"action": "add", "content": "用户住在北京", "subject": "居住地"})
	out := mmRun(t, s, map[string]any{"action": "add", "content": "用户住在上海", "subject": "居住地"})
	if !strings.Contains(out, "取代") {
		t.Fatalf("同主语异值应取代，得 %q", out)
	}
	var valid int
	for _, e := range fm.ParseEntries() {
		if e.ValidTo == "" {
			valid++
			if !strings.Contains(e.Content, "上海") {
				t.Fatalf("当前有效应是上海，得 %q", e.Content)
			}
		}
	}
	if valid != 1 {
		t.Fatalf("当前有效应 1 条，得 %d", valid)
	}
}

func TestManageMemory_AddSensitiveBlocked(t *testing.T) {
	s, fm := newManageMem(t)
	out := mmRun(t, s, map[string]any{"action": "add", "content": "我的密码是 hunter2xyz"})
	if n := len(fm.ParseEntries()); n != 0 {
		t.Fatalf("敏感内容不应入库，得 %d", n)
	}
	if !strings.Contains(out, "安全") {
		t.Fatalf("应提示安全拦截，得 %q", out)
	}
}

func TestManageMemory_UpdateRemovePinByContent(t *testing.T) {
	s, fm := newManageMem(t)
	mmRun(t, s, map[string]any{"action": "add", "content": "用户的项目用 Go 语言"})

	// update by substring
	mmRun(t, s, map[string]any{"action": "update", "target": "Go 语言", "content": "用户的项目用 Go 和 Rust"})
	es := fm.ParseEntries()
	if len(es) != 1 || !strings.Contains(es[0].Content, "Rust") {
		t.Fatalf("update 应就地改写，得 %+v", es)
	}

	// pin by substring → Pinned true
	mmRun(t, s, map[string]any{"action": "pin", "target": "Rust"})
	if e := fm.ParseEntries(); len(e) != 1 || !e[0].Pinned {
		t.Fatalf("pin 后应 Pinned，得 %+v", e)
	}
	// unpin
	mmRun(t, s, map[string]any{"action": "unpin", "target": "Rust"})
	if e := fm.ParseEntries(); len(e) != 1 || e[0].Pinned {
		t.Fatalf("unpin 后不应 Pinned，得 %+v", e)
	}

	// remove
	mmRun(t, s, map[string]any{"action": "remove", "target": "Rust"})
	if n := len(fm.ParseEntries()); n != 0 {
		t.Fatalf("remove 后应空，得 %d", n)
	}
}

func TestManageMemory_AmbiguousTargetErrors(t *testing.T) {
	s, _ := newManageMem(t)
	mmRun(t, s, map[string]any{"action": "add", "content": "用户喜欢咖啡"})
	mmRun(t, s, map[string]any{"action": "add", "content": "用户喜欢茶"})
	_, err := s.Execute(context.Background(), map[string]any{"action": "remove", "target": "用户喜欢"})
	if err == nil {
		t.Fatal("多条匹配应报错防误删")
	}
}

func TestManageMemory_UnknownAction(t *testing.T) {
	s, _ := newManageMem(t)
	if _, err := s.Execute(context.Background(), map[string]any{"action": "nuke"}); err == nil {
		t.Fatal("未知 action 应报错")
	}
}
