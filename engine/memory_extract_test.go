package engine

import (
	"context"
	"strings"
	"testing"
)

func globalEntryCount(t *testing.T, e *ReActEngine) int {
	t.Helper()
	return len(e.fileMem.ParseEntries())
}

// efacts 把若干无主语正文包成 []extractedFact，便于测试沿用旧的字符串字面量。
func efacts(ss ...string) []extractedFact {
	out := make([]extractedFact, len(ss))
	for i, s := range ss {
		out[i] = extractedFact{Content: s}
	}
	return out
}

// 原子化拆分：去 bullet / 去 NONE / 同批去重；不剥数字前缀。
func TestSplitAtomicFacts(t *testing.T) {
	got := splitAtomicFacts("- 用户是Go开发者\n* 用户喜欢Vim\nNONE\n用户是Go开发者\n3年后端经验")
	want := []string{"用户是Go开发者", "用户喜欢Vim", "3年后端经验"}
	if len(got) != len(want) {
		t.Fatalf("拆分应得 %d 条，得 %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条应为 %q，得 %q", i, want[i], got[i])
		}
	}
}

func TestClassifyMemoryType(t *testing.T) {
	cases := map[string]string{
		"我是一名Go后端工程师":   "identity",
		"用户喜欢简洁的代码":     "preference",
		"以后回答都用中文":      "instruction",
		"用户的项目用 Go+Vue": "fact",
	}
	for content, want := range cases {
		if got := classifyMemoryType(content); got != want {
			t.Errorf("%q 应分类为 %s，得 %s", content, want, got)
		}
	}
}

// 写链命脉：离散事实逐条入库；完全重复不重复写（discard）。
func TestIngest_InsertAndExactDedup(t *testing.T) {
	fm := newFileMem(t, 200)
	eng := engineWithFileMem(t, fm)
	ctx := context.Background()

	if n := eng.ingestExtractedFacts(ctx, efacts("用户喜欢深色主题界面"), ""); n != 1 {
		t.Fatalf("首次应写入 1 条，得 %d", n)
	}
	if c := globalEntryCount(t, eng); c != 1 {
		t.Fatalf("应有 1 条记忆，得 %d", c)
	}
	// 再次摄入完全相同 → discard，不增。
	if n := eng.ingestExtractedFacts(ctx, efacts("用户喜欢深色主题界面"), ""); n != 0 {
		t.Fatalf("完全重复应 discard（写入 0），得 %d", n)
	}
	if c := globalEntryCount(t, eng); c != 1 {
		t.Fatalf("重复后仍应只有 1 条，得 %d", c)
	}
}

// 原子化：一段多行抽取 → 多条独立记忆（旧实现存成 1 整段）。
func TestIngest_AtomicMultiFact(t *testing.T) {
	fm := newFileMem(t, 200)
	eng := engineWithFileMem(t, fm)
	facts := parseExtractedFacts("用户是Go后端开发者\n用户的项目用 Vue 前端\n用户在杭州工作")
	if n := eng.ingestExtractedFacts(context.Background(), facts, ""); n != 3 {
		t.Fatalf("应写入 3 条独立事实，得 %d", n)
	}
	if c := globalEntryCount(t, eng); c != 3 {
		t.Fatalf("应有 3 条记忆，得 %d", c)
	}
}

// 修 P5「只增不整合」：近义改写应**就地 update**，不累积成两条。
func TestIngest_UpdateNearDuplicate(t *testing.T) {
	fm := newFileMem(t, 200)
	eng := engineWithFileMem(t, fm)
	ctx := context.Background()

	eng.ingestExtractedFacts(ctx, efacts("用户喜欢简洁的代码风格"), "")
	// 改写版（高字符二元组重叠）→ 应 update 旧条，而非新增。
	eng.ingestExtractedFacts(ctx, efacts("用户喜欢简洁的代码命名风格"), "")

	entries := eng.fileMem.ParseEntries()
	if len(entries) != 1 {
		t.Fatalf("近义改写应就地整合为 1 条，却得 %d 条: %+v", len(entries), entries)
	}
	if !strings.Contains(entries[0].Content, "命名") {
		t.Fatalf("应更新为新内容（含「命名」），得 %q", entries[0].Content)
	}
}

// G3-extractor：`[主语] 正文` 解析 —— 有效主语入 Subject；无前缀/空/过长主语兜底为整行正文。
func TestParseExtractedFacts_Subject(t *testing.T) {
	got := parseExtractedFacts(
		"[居住地] 用户住在北京\n" +
			"用户喜欢喝咖啡\n" +
			"[这是一个超级长的主语名称会被判非法] 这条主语过长应兜底\n" +
			"[] 空主语兜底")
	if len(got) != 4 {
		t.Fatalf("应解析 4 条，得 %d: %+v", len(got), got)
	}
	if got[0].Subject != "居住地" || got[0].Content != "用户住在北京" {
		t.Fatalf("第1条主语应「居住地」，得 %+v", got[0])
	}
	if got[1].Subject != "" || got[1].Content != "用户喜欢喝咖啡" {
		t.Fatalf("无前缀应主语空，得 %+v", got[1])
	}
	if got[2].Subject != "" || !strings.HasPrefix(got[2].Content, "[这是一个超级长") {
		t.Fatalf("过长主语应兜底整行为正文，得 %+v", got[2])
	}
	if got[3].Subject != "" || !strings.HasPrefix(got[3].Content, "[]") {
		t.Fatalf("空主语应兜底整行为正文，得 %+v", got[3])
	}
}

// G3-extractor 命脉：带主语的属性型事实，同主语异值经实时抽取写链 → 时序取代留史（supersede 实时点火）。
func TestIngest_SubjectSupersedeKeepsHistory(t *testing.T) {
	fm := newFileMem(t, 200)
	eng := engineWithFileMem(t, fm)
	ctx := context.Background()

	eng.ingestExtractedFacts(ctx, parseExtractedFacts("[居住地] 用户住在北京"), "")
	eng.ingestExtractedFacts(ctx, parseExtractedFacts("[居住地] 用户现在住在上海"), "")

	all := eng.fileMem.ParseEntries()
	if len(all) != 2 {
		t.Fatalf("留史：北京失效 + 上海有效共 2 条，得 %d: %+v", len(all), all)
	}
	var valid, invalid int
	for _, e := range all {
		if e.ValidTo == "" {
			valid++
			if !strings.Contains(e.Content, "上海") {
				t.Fatalf("当前有效条应是上海，得 %q", e.Content)
			}
		} else {
			invalid++
			if !strings.Contains(e.Content, "北京") {
				t.Fatalf("失效历史条应是北京，得 %q", e.Content)
			}
		}
	}
	if valid != 1 || invalid != 1 {
		t.Fatalf("应 1 有效 + 1 失效留史，得 valid=%d invalid=%d", valid, invalid)
	}
}

// 类型分类路由：身份类事实分类为 identity → 落 _global（常驻层保证带）。
func TestIngest_IdentityRoutedToGlobal(t *testing.T) {
	fm := newFileMem(t, 200)
	eng := engineWithFileMem(t, fm)
	eng.ingestExtractedFacts(context.Background(), efacts("我是一名Go后端工程师"), "hexbot")

	// identity 强制落 _global（跨角色可见、常驻保证带），而非 hexbot 角色目录。
	if g := eng.fileMem.GetMemory(); !strings.Contains(g, "工程师") {
		t.Fatalf("identity 应落 _global，GetMemory 未含: %q", g)
	}
}
