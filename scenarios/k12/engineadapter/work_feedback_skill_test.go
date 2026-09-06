package engineadapter

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 本文件是「作品点评消费 hub skill」的契约锁（hub v0.0.7 writing-feedback / art-feedback）：
// 加载链 盘上 marketplace（SkillContentLoader 注入）→ 内嵌快照 → 硬编码红线，逐级契约：
// ① 盘上有效版本**优先于**内嵌（hub 演进不重编译生效），frontmatter 剥离、来源戳 /disk；
// ② 盘上缺失/读取失败 → 内嵌快照，来源戳 /embedded；
// ③ 盘上空/损坏/缺红线锚点/min_engine_version 过高 → 视为损坏，降级内嵌 + 不炸；
// ④ 盘上与内嵌全缺 → 硬编码红线提示词，来源戳 builtin；
// ⑤ 硬编码红线段**任何路径**都无条件叠加（与 skill 红线、usecase INV-011 三道保险）。

func writingReq() usecase.WorkFeedbackRequest {
	return usecase.WorkFeedbackRequest{
		WorkType:        k12.WorkTypeWriting,
		Title:           "《放学路上》",
		Task:            "写一段放学路上的见闻",
		ContentMarkdown: "今天放学下了小雨，雨点打在伞上嗒嗒响。",
	}
}

func artReq() usecase.WorkFeedbackRequest {
	return usecase.WorkFeedbackRequest{
		WorkType: k12.WorkTypeArt,
		Title:    "《我家门前》",
		Task:     "画一画自己家门前的景色",
		Intent:   "想画晴天的小房子",
	}
}

// 硬编码红线段（三道保险的第二道，所有路径必须原样在场）。
const (
	writingHardRedline = "给家长修改示范与完整参考稿，并说明先讲什么、怎样追问、卡住时如何引导、如何检查理解。原稿与参考稿分开，不编造孩子事实；不打分、不评级、不排名。直接给内容与讲法，不输出原则声明。"
	artHardRedline     = "红线：不打分、不评级、不排名、不做审美排名；不替孩子重画。"
)

// diskSkillDoc 造一份可区分的「盘上演进版」skill 文本：合法 frontmatter + 指定正文。
func diskSkillDoc(name, version, extraFrontmatter, body string) string {
	return "---\nname: " + name + "\nversion: \"" + version + "\"\n" + extraFrontmatter +
		"schema_version: 1\n---\n\n" + body
}

// validDiskWritingBody 携带家长参考与评分边界锚点的盘上演进版正文。
const validDiskWritingBody = "# 盘上演进版写作反馈 v9\n新增的演进方法论段落。\n家长参考稿与原稿分开，不打分。"

// validDiskArtBody 携带美术红线锚点（不打分/不重画/不得先追问）的盘上演进版正文。
const validDiskArtBody = "# 盘上演进版美术反馈 v9\n新增的观察演进段落。\n红线：不打分、不重画、不得先追问。"

// TestBuildWorkFeedbackPrompt_DiskPreferredOverEmbedded 盘上有效版本优先于内嵌：
// 提示词用盘上正文（内嵌独有句不出现）、frontmatter 剥离、来源戳 /disk、硬编码红线仍叠加。
func TestBuildWorkFeedbackPrompt_DiskPreferredOverEmbedded(t *testing.T) {
	t.Run("写作", func(t *testing.T) {
		loader := func(name string) (string, error) {
			if name != "writing-feedback" {
				return "", fmt.Errorf("盘上无 %q", name)
			}
			return diskSkillDoc("writing-feedback", "9.9.9", "", validDiskWritingBody), nil
		}
		_, prompt, stamp, err := buildWorkFeedbackPrompt(writingReq(), loader)
		if err != nil {
			t.Fatalf("盘上有效版本构造提示词失败: %v", err)
		}
		if !strings.Contains(prompt, validDiskWritingBody) {
			t.Error("提示词应注入盘上演进版正文")
		}
		if strings.Contains(prompt, "叶圣陶") {
			t.Error("盘上版本生效时不应再注入内嵌快照正文（叶圣陶是内嵌独有锚句）")
		}
		if strings.Contains(prompt, "schema_version") || strings.Contains(prompt, "version: \"9.9.9\"") {
			t.Error("盘上版本 frontmatter 必须剥离")
		}
		if stamp != "writing-feedback@9.9.9/disk" {
			t.Errorf("来源戳应为 writing-feedback@9.9.9/disk，got %q", stamp)
		}
		if !strings.Contains(prompt, writingHardRedline) {
			t.Error("盘上路径丢失硬编码红线段")
		}
		if !strings.Contains(prompt, "嗒嗒响") {
			t.Error("作品证据缺失")
		}
		if strings.Contains(prompt, "评价框架、输出信封与红线全部遵此执行") {
			t.Error("writing skill introduction must not require a competing output envelope")
		}
		if !strings.Contains(prompt, "专业方法、原文证据约束与红线") || !strings.Contains(prompt, "输出格式以文末四个固定二级标题为准") {
			t.Error("writing skill introduction must preserve professional methods and evidence while deferring format to the final four headings")
		}
		for _, heading := range []string{"## 可见证据", "## 先这样肯定", "## 家长可以这样问或讲", "## 下一次只试一个点"} {
			if !strings.Contains(prompt, heading) {
				t.Errorf("writing output envelope is missing %q", heading)
			}
		}
	})
	t.Run("美术", func(t *testing.T) {
		loader := func(name string) (string, error) {
			if name != "art-feedback" {
				return "", fmt.Errorf("盘上无 %q", name)
			}
			return diskSkillDoc("art-feedback", "2.0.0", "", validDiskArtBody), nil
		}
		_, prompt, stamp, err := buildWorkFeedbackPrompt(artReq(), loader)
		if err != nil {
			t.Fatalf("盘上有效版本构造提示词失败: %v", err)
		}
		if !strings.Contains(prompt, validDiskArtBody) {
			t.Error("提示词应注入盘上演进版正文")
		}
		if strings.Contains(prompt, "罗恩菲德") {
			t.Error("盘上版本生效时不应再注入内嵌快照正文（罗恩菲德是内嵌独有锚句）")
		}
		if stamp != "art-feedback@2.0.0/disk" {
			t.Errorf("来源戳应为 art-feedback@2.0.0/disk，got %q", stamp)
		}
		if !strings.Contains(prompt, artHardRedline) {
			t.Error("盘上路径丢失硬编码红线段")
		}
		for _, heading := range []string{"## 可见证据", "## 先这样肯定", "## 家长可以这样问或讲", "## 下一次只试一个点"} {
			if !strings.Contains(prompt, heading) {
				t.Errorf("art output envelope is missing %q", heading)
			}
		}
	})
}

// TestBuildWorkFeedbackPrompt_DiskUnavailable_FallsBackEmbedded 盘上缺失/读取失败 →
// 内嵌快照兜底（正文与来源戳都落 embedded），不炸。
func TestBuildWorkFeedbackPrompt_DiskUnavailable_FallsBackEmbedded(t *testing.T) {
	loader := func(name string) (string, error) { return "", fmt.Errorf("skill %q 未安装", name) }
	_, prompt, stamp, err := buildWorkFeedbackPrompt(writingReq(), loader)
	if err != nil {
		t.Fatalf("盘上缺失应降级内嵌而非报错: %v", err)
	}
	if !strings.Contains(prompt, "叶圣陶") {
		t.Error("盘上缺失时应注入内嵌快照正文")
	}
	if stamp != "writing-feedback@1.0.1/embedded" {
		t.Errorf("来源戳应为 writing-feedback@1.0.1/embedded，got %q", stamp)
	}
	if !strings.Contains(prompt, writingHardRedline) {
		t.Error("内嵌路径丢失硬编码红线段")
	}
}

// TestBuildWorkFeedbackPrompt_DiskCorrupt_FallsBackEmbedded 盘上内容校验（防用户改坏）：
// 空文件 / 只剩 frontmatter / 红线锚点被删 / min_engine_version 高于当前应用 →
// 一律视为损坏，降级内嵌 + 不炸，硬编码红线仍在。
func TestBuildWorkFeedbackPrompt_DiskCorrupt_FallsBackEmbedded(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"空内容", "   \n  "},
		{"只剩frontmatter", "---\nname: writing-feedback\nversion: \"9.9.9\"\n---\n\n   "},
		{"缺家长参考锚点", diskSkillDoc("writing-feedback", "9.9.9", "", "# 被改坏的正文\n红线：不打分。")},
		{"缺不打分红线锚点", diskSkillDoc("writing-feedback", "9.9.9", "", "# 被改坏的正文\n家长参考稿与原稿分开。")},
		{"min_engine_version过高", diskSkillDoc("writing-feedback", "9.9.9",
			"min_engine_version: \"99.0.0\"\n", validDiskWritingBody)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loader := func(string) (string, error) { return tc.content, nil }
			_, prompt, stamp, err := buildWorkFeedbackPrompt(writingReq(), loader)
			if err != nil {
				t.Fatalf("盘上损坏应降级内嵌而非报错: %v", err)
			}
			if strings.Contains(prompt, "被改坏的正文") || strings.Contains(prompt, "盘上演进版") {
				t.Error("损坏的盘上内容不得进提示词")
			}
			if !strings.Contains(prompt, "叶圣陶") {
				t.Error("盘上损坏时应降级注入内嵌快照正文")
			}
			if stamp != "writing-feedback@1.0.1/embedded" {
				t.Errorf("来源戳应落 embedded，got %q", stamp)
			}
			if !strings.Contains(prompt, writingHardRedline) {
				t.Error("降级路径丢失硬编码红线段")
			}
		})
	}
}

// TestBuildWorkFeedbackPrompt_DiskMinEngineCompatible min_engine_version 不高于当前应用
// （含等于当前主版本、当前为预发布 -beta 的宽松放行）→ 盘上版本正常生效。
func TestBuildWorkFeedbackPrompt_DiskMinEngineCompatible(t *testing.T) {
	// hexclaw.Version = "0.5.0-beta"：声明 0.5.0 的 skill 宽松兼容（预发布后缀剥掉再比）。
	loader := func(string) (string, error) {
		return diskSkillDoc("writing-feedback", "9.9.9", "min_engine_version: \"0.5.0\"\n", validDiskWritingBody), nil
	}
	_, prompt, stamp, err := buildWorkFeedbackPrompt(writingReq(), loader)
	if err != nil {
		t.Fatalf("兼容版本应生效: %v", err)
	}
	if !strings.Contains(prompt, "盘上演进版写作反馈 v9") || stamp != "writing-feedback@9.9.9/disk" {
		t.Errorf("min_engine_version 兼容时盘上版本应生效，stamp=%q", stamp)
	}
}

// TestBuildWorkFeedbackPrompt_AllMissing_Builtin 盘上与内嵌全缺 → 硬编码红线提示词
// 最终兜底：不炸、红线在、来源戳 builtin、不残留任何基座内容。
func TestBuildWorkFeedbackPrompt_AllMissing_Builtin(t *testing.T) {
	orig := workFeedbackSkillsFS
	workFeedbackSkillsFS = fstest.MapFS{} // 内嵌也读不到
	t.Cleanup(func() { workFeedbackSkillsFS = orig })
	loader := func(name string) (string, error) { return "", fmt.Errorf("skill %q 未安装", name) }

	_, wp, wstamp, err := buildWorkFeedbackPrompt(writingReq(), loader)
	if err != nil {
		t.Fatalf("全缺时写作提示词不得报错（应回退硬编码）: %v", err)
	}
	if wstamp != "builtin" {
		t.Errorf("写作来源戳应为 builtin，got %q", wstamp)
	}
	if strings.Contains(wp, "方法论基座") || strings.Contains(wp, "输出信封") {
		t.Error("全缺时写作提示词不应残留基座内容")
	}
	for _, kw := range []string{"好句摘出", "一处具体建议", writingHardRedline} {
		if !strings.Contains(wp, kw) {
			t.Errorf("写作硬编码回退缺指令 %q", kw)
		}
	}

	_, ap, astamp, err := buildWorkFeedbackPrompt(artReq(), loader)
	if err != nil {
		t.Fatalf("全缺时美术提示词不得报错（应回退硬编码）: %v", err)
	}
	if astamp != "builtin" {
		t.Errorf("美术来源戳应为 builtin，got %q", astamp)
	}
	if strings.Contains(ap, "方法论基座") || strings.Contains(ap, "罗恩菲德") {
		t.Error("全缺时美术提示词不应残留基座内容")
	}
	for _, kw := range []string{"先描述画面里可见的构图、色彩、线条、空间与表达证据", artHardRedline} {
		if !strings.Contains(ap, kw) {
			t.Errorf("美术硬编码回退缺指令 %q", kw)
		}
	}
}

// TestBuildWorkFeedbackPrompt_Writing_InjectsSkillBody 无盘上 loader（生产 marketplace
// 未启用）→ 内嵌快照基座生效：关键方法论句在、frontmatter 剥离、硬编码红线叠加。
func TestBuildWorkFeedbackPrompt_Writing_InjectsSkillBody(t *testing.T) {
	subject, prompt, stamp, err := buildWorkFeedbackPrompt(writingReq(), nil)
	if err != nil {
		t.Fatalf("构造写作提示词失败: %v", err)
	}
	if subject != "语文" {
		t.Errorf("写作学科应为语文, got %q", subject)
	}
	if stamp != "writing-feedback@1.0.1/embedded" {
		t.Errorf("无 loader 时来源戳应为 embedded，got %q", stamp)
	}
	// skill 正文关键方法论句（来自 hub v0.0.7 writing-feedback.md 正文）。
	for _, kw := range []string{
		"语文写作反馈",      // 基座抬头
		"最值得改的 1～3 处", // 输出信封第 3 段：一次最多 3 处
		"输出信封",        // 固定六段信封
		"叶圣陶",         // 方法论根基
		"家长参考",        // 参考稿与孩子原稿分开
	} {
		if !strings.Contains(prompt, kw) {
			t.Errorf("写作提示词缺 skill 正文关键句 %q", kw)
		}
	}
	// frontmatter 必须剥离：元数据键不得进提示词。
	for _, meta := range []string{"min_engine_version", "schema_version", "trust: first-party", "signature:"} {
		if strings.Contains(prompt, meta) {
			t.Errorf("写作提示词泄漏 frontmatter 元数据 %q", meta)
		}
	}
	if !strings.Contains(prompt, writingHardRedline) {
		t.Error("写作提示词丢失硬编码红线段（应与 skill 红线双保险共存）")
	}
	for _, kw := range []string{"《放学路上》", "作文原文", "嗒嗒响"} {
		if !strings.Contains(prompt, kw) {
			t.Errorf("写作提示词缺作品证据 %q", kw)
		}
	}
}

// TestBuildWorkFeedbackPrompt_Art_InjectsSkillBody 美术内嵌快照基座契约（同上）。
func TestBuildWorkFeedbackPrompt_Art_InjectsSkillBody(t *testing.T) {
	subject, prompt, stamp, err := buildWorkFeedbackPrompt(artReq(), nil)
	if err != nil {
		t.Fatalf("构造美术提示词失败: %v", err)
	}
	if subject != "美术" {
		t.Errorf("美术学科应为美术, got %q", subject)
	}
	if stamp != "art-feedback@1.0.1/embedded" {
		t.Errorf("无 loader 时来源戳应为 embedded，got %q", stamp)
	}
	// skill 正文关键方法论句（来自 hub v0.0.7 art-feedback.md 正文）。
	// 注：任务书里的「先描述后评价」是 frontmatter acceptance 里的措辞，剥离后不进提示词；
	// 正文等价锚点是费德门四步的「描述（先做，防编造）」流程标题。
	for _, kw := range []string{
		"美术作品反馈",     // 基座抬头
		"描述（先做，防编造）", // 先描述后评价（费德门四步适龄化，顺序不可换）
		"五要素观察框架",    // 构图/色彩/线条/空间/材料效果
		"罗恩菲德",       // 发展阶段标尺
		"不重画、不出示范图",  // skill 红线
	} {
		if !strings.Contains(prompt, kw) {
			t.Errorf("美术提示词缺 skill 正文关键句 %q", kw)
		}
	}
	for _, meta := range []string{"min_engine_version", "schema_version", "trust: first-party"} {
		if strings.Contains(prompt, meta) {
			t.Errorf("美术提示词泄漏 frontmatter 元数据 %q", meta)
		}
	}
	if !strings.Contains(prompt, artHardRedline) {
		t.Error("美术提示词丢失硬编码红线段（应与 skill 红线双保险共存）")
	}
	for _, kw := range []string{"《我家门前》", "创作任务", "想画晴天的小房子", "只依据图中可见证据"} {
		if !strings.Contains(prompt, kw) {
			t.Errorf("美术提示词缺作品证据/任务上下文 %q", kw)
		}
	}
}

// TestBuildWorkFeedbackPrompt_Art_RequiresNamedElementCoverage locks the real-fixture
// contract: when the task or intent names concrete visual elements, the vision model
// must verify every named element against the image instead of silently omitting one.
func TestBuildWorkFeedbackPrompt_Art_RequiresNamedElementCoverage(t *testing.T) {
	req := artReq()
	req.Task = "观察人物、猫、彩虹和地面的构图"
	req.Intent = "想画快乐的户外场景"
	_, prompt, _, err := buildWorkFeedbackPrompt(req, nil)
	if err != nil {
		t.Fatalf("构造美术提示词失败: %v", err)
	}
	for _, want := range []string{
		"逐项核对创作任务和孩子意图中明确提到的具体画面元素",
		"看得见就必须在观察证据中点名",
		"看不见则明确说明没有观察到",
		"强制覆盖清单：观察人物、猫、彩虹和地面的构图",
		"最终正文必须逐字包含清单中的每个具体名词",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("美术提示词缺具体元素覆盖约束 %q", want)
		}
	}
}

// TestStripSkillFrontmatter 剥离器边界：有/无 frontmatter、残缺定界都不丢正文。
func TestStripSkillFrontmatter(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"标准frontmatter", "---\nname: x\ntags: [k12]\n---\n\n# 正文\n内容", "# 正文\n内容"},
		{"无frontmatter", "# 正文\n内容", "# 正文\n内容"},
		{"残缺定界原文返回", "---\nname: x\n没有收尾", "---\nname: x\n没有收尾"},
	}
	for _, c := range cases {
		if got := stripSkillFrontmatter(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSkillFrontmatterField 标量字段提取（version / min_engine_version 运行时消费的地基）。
func TestSkillFrontmatterField(t *testing.T) {
	doc := "---\nname: writing-feedback\nversion: \"1.2.0\"\nmin_engine_version: '0.5.0'\ntags: [k12]\n---\n正文"
	if got := skillFrontmatterField(doc, "version"); got != "1.2.0" {
		t.Errorf("version got %q", got)
	}
	if got := skillFrontmatterField(doc, "min_engine_version"); got != "0.5.0" {
		t.Errorf("min_engine_version got %q", got)
	}
	if got := skillFrontmatterField(doc, "absent"); got != "" {
		t.Errorf("缺失字段应为空, got %q", got)
	}
	if got := skillFrontmatterField("无 frontmatter 的正文", "version"); got != "" {
		t.Errorf("无 frontmatter 应为空, got %q", got)
	}
}

// TestCompareLooseVersions 宽松版本比较：预发布后缀剥离、缺段补 0、空串最低。
func TestCompareLooseVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.5.0", "0.5.0-beta", 0}, // 预发布宽松放行（min_engine 0.5.0 兼容 0.5.0-beta 应用）
		{"0.5.1", "0.5.0-beta", 1},
		{"0.5", "0.5.0", 0},
		{"1.0.0", "0.9.9", 1},
		{"", "0.5.0", -1},
		{"v1.2.0", "1.2", 0},
		{"99.0.0", "0.5.0", 1},
	}
	for _, c := range cases {
		if got := compareLooseVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}
