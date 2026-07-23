package engineadapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// WorkFeedbackGenerateFunc 是作品证据化点评的轻量生成闭包（composition root 注入，
// 与 TutoringTipsReviewGenerateFunc 同模式）：单次 reasoning 生成，不派 verifier / 不
// self-consistency——点评是形成性反馈，不是判分，无需全对抗验算链。
// subject 用中文学科名路由（写作=语文，美术=美术）；prompt 已含作品可见证据与红线指令。
type WorkFeedbackGenerateFunc func(ctx context.Context, subject, prompt, grade string) (string, error)

// WithWorkFeedbackGen 注入作品点评生成闭包。
func WithWorkFeedbackGen(fn WorkFeedbackGenerateFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.workFeedbackGen = fn }
}

// SetWorkFeedbackGen 事后注入作品点评生成闭包（composition root 在 Deps.Solver 建好后回填）。
func (a *SolveAdapter) SetWorkFeedbackGen(fn WorkFeedbackGenerateFunc) { a.workFeedbackGen = fn }

// WithWorkFeedbackVision 注入美术作品点评的视觉闭包（复用识题链 VisionFunc 原语：
// 原图 bytes + 提示词 → 视觉模型文本；真实现在 cmd/hexclaw/main.go 用 RouteForVision +
// 多模态 Complete 构造，与识题 visionFn 同一批模型同一调用方式）。
func WithWorkFeedbackVision(fn VisionFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.workFeedbackVision = fn }
}

// SetWorkFeedbackVision 事后注入美术点评视觉闭包（composition root 回填）。
func (a *SolveAdapter) SetWorkFeedbackVision(fn VisionFunc) { a.workFeedbackVision = fn }

// SkillContentLoader 按 skill 名（不含 .md，如 "writing-feedback"）返回**盘上** skill 的
// 原始内容（含 frontmatter）。窄闭包注入而非 import skill/marketplace——依赖方向裁决：
// marketplace 是 composition root 从 config 构造的有状态单例（Skills.Enabled=false 时不存在），
// 包 import 拿不到实例；传 *Marketplace 又会把 Install/Uninstall/seed 整个管理面漏进场景
// adapter。窄闭包与本文件 workFeedbackGen/vision 的注入模式同构，场景层不新增对平台管理层的
// 依赖边。每次调用都应读盘取当前内容（hub Refresh/Install 更新后无需重启即生效）。
type SkillContentLoader func(name string) (string, error)

// WithWorkFeedbackSkillLoader 注入盘上 skill 内容加载闭包（marketplace 消费缝）。
func WithWorkFeedbackSkillLoader(fn SkillContentLoader) SolveAdapterOption {
	return func(a *SolveAdapter) { a.workFeedbackSkillLoader = fn }
}

// SetWorkFeedbackSkillLoader 事后注入盘上 skill 加载闭包（composition root 回填）。
func (a *SolveAdapter) SetWorkFeedbackSkillLoader(fn SkillContentLoader) {
	a.workFeedbackSkillLoader = fn
}

var _ usecase.WorkFeedbackGenerator = (*SolveAdapter)(nil)

// GenerateWorkFeedback 实现 usecase.WorkFeedbackGenerator（PRD §3.10 / INV-011）：
// 写作 = 好句摘出 + 一处具体建议，走纯文本生成闭包；美术 = 观察描述式点评，走视觉闭包
// （原图随请求发给视觉模型，观察只依据可见证据）。只点评不打分不代写——红线在提示词与
// 用例层双重钉死（生成端约束 + 入库端拒绝）。未注入闭包/原图缺失时诚实报错，
// 绝不回退 solve 全链（点评不是解题，跑验算链既慢又语义错位）。
// 返回值带 SkillStamp：本次点评实际使用的方法论基座来源戳（盘上/内嵌/硬编码），随点评落库可追溯。
func (a *SolveAdapter) GenerateWorkFeedback(ctx context.Context, req usecase.WorkFeedbackRequest) (usecase.WorkFeedbackOutput, error) {
	subject, prompt, stamp, err := buildWorkFeedbackPrompt(req, a.workFeedbackSkillLoader)
	if err != nil {
		return usecase.WorkFeedbackOutput{}, err
	}
	if req.WorkType == k12.WorkTypeArt {
		out, aerr := a.generateArtFeedback(ctx, req, prompt)
		if aerr != nil {
			return usecase.WorkFeedbackOutput{}, aerr
		}
		return usecase.WorkFeedbackOutput{Feedback: out, SkillStamp: stamp}, nil
	}
	if a.workFeedbackGen == nil {
		return usecase.WorkFeedbackOutput{}, fmt.Errorf("work feedback: 未注入作品点评生成闭包")
	}
	out, err := a.workFeedbackGen(ctx, subject, prompt, req.Grade)
	if err != nil {
		return usecase.WorkFeedbackOutput{}, err
	}
	return usecase.WorkFeedbackOutput{Feedback: stripReports(out), SkillStamp: stamp}, nil
}

// generateArtFeedback 美术观察式点评：解析原图 asset → 多模态视觉调用。
// 「需要视觉通道」的诚实报错只保留两种情形：视觉闭包未接入、原图确实缺失/读不到。
func (a *SolveAdapter) generateArtFeedback(ctx context.Context, req usecase.WorkFeedbackRequest, prompt string) (string, error) {
	if a.workFeedbackVision == nil {
		return "", fmt.Errorf("work feedback: 美术观察式点评需要读取原图的视觉通道，当前未接入，暂不能生成")
	}
	image, err := loadWorkArtImage(req.SourceAssetID)
	if err != nil {
		return "", fmt.Errorf("work feedback: %w", err)
	}
	if req.Grade != "" {
		prompt += "\n（点评口径贴合" + req.Grade + "孩子的水平，用家长和孩子都能懂的话。）"
	}
	out, err := a.workFeedbackVision(ctx, image, prompt)
	if err != nil {
		return "", err
	}
	return stripReports(out), nil
}

// loadWorkArtImage 解析美术作品的原图载体（最小可用 asset 约定，设计申报见 PR）：
// SourceAssetID 允许三种真实图片载体——
//  1. data:image/...;base64,<payload> 内联图片（API/IM 客户端直接上送）；
//  2. asset://<agent>/<sha256>.<ext> 资产 ID（POST /assets 真实上传落盘的内容寻址载体，
//     经 assetstore 解析本地路径；归属校验在用例层完成——作品 agent 必须等于资产 owner）；
//  3. 本地文件路径（桌面端与服务端同机，图片落盘后传路径）。
//
// 三种载体都在发模型前校验真实存在且魔数为 image/*；空值/读不到/非图片一律诚实报错，
// 不猜存储位置、不虚构画面（INV-011 的另一半：观察必须有真实可见证据）。
func loadWorkArtImage(assetID string) ([]byte, error) {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return nil, fmt.Errorf("美术点评需要作品原图（最新版本未携带图片资产）")
	}
	var raw []byte
	switch {
	case strings.HasPrefix(assetID, "data:"):
		_, payload, ok := strings.Cut(assetID, ";base64,")
		if !ok {
			return nil, fmt.Errorf("美术点评原图 data URL 缺少 base64 载荷")
		}
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, fmt.Errorf("美术点评原图 base64 解码失败: %w", err)
		}
		raw = decoded
	case assetstore.IsAssetID(assetID):
		path, err := assetstore.PathFromID(assetID)
		if err != nil {
			return nil, fmt.Errorf("美术点评解析资产失败: %w", err)
		}
		read, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("美术点评读取资产原图失败: %w", err)
		}
		raw = read
	default:
		read, err := os.ReadFile(assetID)
		if err != nil {
			return nil, fmt.Errorf("美术点评读取原图失败（asset=%q）: %w", assetID, err)
		}
		raw = read
	}
	if mime := http.DetectContentType(raw); !strings.HasPrefix(mime, "image/") {
		return nil, fmt.Errorf("美术点评 asset 不是图片（探测到 %s）", mime)
	}
	return raw, nil
}

// workFeedbackSkillsFS 是作品点评方法论基座的**第二级**来源：发版内嵌快照（go:embed，
// 与首启 SeedFromFS 写盘到 ~/.hexclaw/skills/ 的是同一份字节）。加载链：
// ① 盘上 marketplace 版本（SkillContentLoader 注入，hub Install/Refresh/seed 升级后
//
//	不重新编译即用新正文）→ ② 本内嵌快照（盘上缺失/损坏/不兼容时兜底，永远可用）
//
// → ③ 硬编码红线提示词（最终兜底）。每级降级 slog 记原因。
// 包级变量仅为测试注入缺失场景；生产恒为 k12.BundledSkillsFS()。
var workFeedbackSkillsFS fs.FS = k12.BundledSkillsFS()

// 作品点评消费的 hub skill（hub v0.0.7 新增，随 seed 同步进内嵌 FS）。
// 盘上按 skill 名经 SkillContentLoader 取；内嵌按文件路径取——同名同源。
const (
	writingFeedbackSkillName = "writing-feedback"
	artFeedbackSkillName     = "art-feedback"
	writingFeedbackSkillFile = "skills/writing-feedback.md"
	artFeedbackSkillFile     = "skills/art-feedback.md"
)

// workFeedbackStampBuiltin 硬编码兜底的来源戳（落库 feedback_skill 字段值）。
const workFeedbackStampBuiltin = "builtin"

// 红线锚点（盘上版本完整性守卫，克制选择——只锁**红线级语义**，不锁正文措辞/结构，
// skill 措辞可随 hub 自由演进）：
//   - 写作：「零代写」（INV-011 写作侧核心：不替孩子写正文）+「不打分」（形成性反馈的根）。
//   - 美术：「不打分」（同上）+「不重画」（INV-011 美术侧核心：不替孩子重画/出示范）。
//
// 宽松 Contains 检查——锚点是红线小节的稳定词根，任何保留红线语义的改版都不会误伤；
// 丢失锚点即视为盘上文件被改坏（或被恶意删红线），降级内嵌快照。
var (
	writingFeedbackRedlineAnchors = []string{"零代写", "不打分"}
	artFeedbackRedlineAnchors     = []string{"不打分", "不重画"}
)

// workFeedbackSkillStamp 组装来源戳："<skill>@<version>/<source>"；frontmatter 无 version
// 时省略 "@<version>"。source ∈ {disk, embedded}。
func workFeedbackSkillStamp(name, version, source string) string {
	if version == "" {
		return name + "/" + source
	}
	return name + "@" + version + "/" + source
}

// resolveWorkFeedbackSkill 按「盘上 → 内嵌」顺序解析作品反馈 skill 正文；两级都不可用时
// 返回 ok=false，调用方落到硬编码红线提示词（第三级）。返回的 stamp 标记实际来源，随点评落库。
func resolveWorkFeedbackSkill(loader SkillContentLoader, name, embeddedFile string, redlineAnchors []string) (body, stamp string, ok bool) {
	// ① 盘上 marketplace 版本（优先：可随 hub 演进，不重编译生效）。
	if loader != nil {
		raw, err := loader(name)
		if err != nil {
			slog.Warn("k12 作品点评：盘上 skill 读取失败，降级内嵌快照", "skill", name, "err", err)
		} else if diskBody, verr := validateDiskWorkFeedbackSkill(raw, redlineAnchors); verr != nil {
			slog.Warn("k12 作品点评：盘上 skill 校验失败（视为损坏/不兼容），降级内嵌快照",
				"skill", name, "reason", verr.Error())
		} else {
			ver := skillFrontmatterField(raw, "version")
			slog.Info("k12 作品点评：盘上 marketplace skill 正文已加载", "skill", name, "version", ver, "source", "disk")
			return diskBody, workFeedbackSkillStamp(name, ver, "disk"), true
		}
	}
	// ② 内嵌发版快照。快照随二进制出厂、经现有契约测试锁红线，此处只做非空校验。
	raw, err := fs.ReadFile(workFeedbackSkillsFS, embeddedFile)
	if err != nil {
		slog.Warn("k12 作品点评：内嵌 skill 读取失败，回退硬编码红线提示词", "skill", embeddedFile, "err", err)
		return "", "", false
	}
	embBody := stripSkillFrontmatter(string(raw))
	if embBody == "" {
		slog.Warn("k12 作品点评：内嵌 skill 正文为空，回退硬编码红线提示词", "skill", embeddedFile)
		return "", "", false
	}
	return embBody, workFeedbackSkillStamp(name, skillFrontmatterField(string(raw), "version"), "embedded"), true
}

// validateDiskWorkFeedbackSkill 盘上内容校验（防用户改坏，只对盘上级生效）：
// ① 非空；② 声明的 min_engine_version 不高于当前应用版本（frontmatter 已有该字段，
// 此处补运行时消费——盘上 skill 要求更新引擎时不硬塞给旧引擎，降级内嵌）；
// ③ frontmatter 可剥离且正文非空；④ 红线锚点齐全（见锚点清单注释）。
// 通过则返回剥离后的正文。
func validateDiskWorkFeedbackSkill(raw string, redlineAnchors []string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("内容为空")
	}
	if minEngine := skillFrontmatterField(raw, "min_engine_version"); minEngine != "" {
		if compareLooseVersions(minEngine, hexclaw.Version) > 0 {
			return "", fmt.Errorf("min_engine_version %s 高于当前应用 %s", minEngine, hexclaw.Version)
		}
	}
	body := stripSkillFrontmatter(raw)
	if body == "" {
		return "", fmt.Errorf("剥离 frontmatter 后正文为空")
	}
	for _, anchor := range redlineAnchors {
		if !strings.Contains(body, anchor) {
			return "", fmt.Errorf("正文缺失红线锚点 %q", anchor)
		}
	}
	return body, nil
}

// skillFrontmatterField 从 skill markdown 原始文本的 YAML frontmatter 取单个标量字段
// （行级扫描，定界与 stripSkillFrontmatter / marketplace.parseFrontmatter 一致）。
// 只服务 version / min_engine_version 这类简单标量，不做完整 YAML。缺失返回空串。
func skillFrontmatterField(raw, key string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "---") {
		return ""
	}
	endIdx := strings.Index(raw[3:], "\n---")
	if endIdx < 0 {
		return ""
	}
	for _, line := range strings.Split(raw[3:endIdx+3], "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

// compareLooseVersions 宽松比较点分版本号，返回 -1/0/1：剥前缀 v 与预发布后缀
// （-beta/+meta——所以 min_engine_version "0.5.0" 兼容应用 "0.5.0-beta"，宽松放行）；
// 数字段按整数比、缺段补 0、非数字段回退字典序；空串视为最低。
// 与 skill/marketplace.compareSkillVersions 同语义但本地实现——依赖方向裁决见
// SkillContentLoader 注释：场景 adapter 不 import 平台管理层。
func compareLooseVersions(a, b string) int {
	norm := func(s string) string {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		if i := strings.IndexAny(s, "-+"); i >= 0 {
			s = s[:i]
		}
		return s
	}
	a, b = norm(a), norm(b)
	if a == b {
		return 0
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := "0", "0" // 缺段补 0（"0.5" == "0.5.0"）
		if i < len(as) && as[i] != "" {
			av = as[i]
		}
		if i < len(bs) && bs[i] != "" {
			bv = bs[i]
		}
		ai, aerr := strconv.Atoi(av)
		bi, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
			continue
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// stripSkillFrontmatter 剥离 markdown skill 的 YAML frontmatter（与
// skill/marketplace/skill_md.go parseFrontmatter 的定界逻辑一致：`---` 开头、
// `\n---` 收尾）；无 frontmatter 或格式残缺时原文返回，不丢正文。
func stripSkillFrontmatter(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "---") {
		return text
	}
	endIdx := strings.Index(text[3:], "\n---")
	if endIdx < 0 {
		return text
	}
	return strings.TrimSpace(text[endIdx+3+4:])
}

// buildWorkFeedbackPrompt 按作品类型构造点评提示词：按「盘上 marketplace → 内嵌快照 →
// 硬编码」链解析作品反馈 skill 正文作方法论基座（写作→writing-feedback，美术→art-feedback）。
// 硬编码红线段所有路径都无条件叠加（不打分/不代写/不重画——与 skill 内红线、usecase 层
// INV-011 确定性拦截构成三道保险），红线随任务下发，不依赖模型默认行为。
// 返回 skillStamp 标记本次实际使用的基座来源（随点评落库追溯）。
func buildWorkFeedbackPrompt(req usecase.WorkFeedbackRequest, loader SkillContentLoader) (subject, prompt, skillStamp string, err error) {
	var b strings.Builder
	skillStamp = workFeedbackStampBuiltin
	switch req.WorkType {
	case k12.WorkTypeWriting:
		subject = "语文"
		if body, stamp, ok := resolveWorkFeedbackSkill(loader, writingFeedbackSkillName, writingFeedbackSkillFile, writingFeedbackRedlineAnchors); ok {
			skillStamp = stamp
			b.WriteString("以下是「语文写作反馈」技能的方法论基座，本次点评的评价框架、输出信封与红线全部遵此执行：\n\n")
			b.WriteString(body)
			b.WriteString("\n\n——以下是本次点评任务——\n")
			b.WriteString("对下面这篇孩子的作文按上述技能给形成性反馈。\n")
		} else {
			b.WriteString("对下面这篇孩子的作文给证据化点评，两部分输出：\n")
			b.WriteString("1. 好句摘出：原样引用 1～2 个原文好句，各用一句话说明好在哪里；\n")
			b.WriteString("2. 一处具体建议：只挑最值得改的一处，指出在原文哪里、怎么改，控制在两三句话。\n")
		}
		b.WriteString("红线：只点评不打分——禁止输出任何分数、等第、评级、排名；不代写——禁止给范文、禁止改写或重写全文。\n")
		if req.Title != "" {
			b.WriteString("作文题目：" + req.Title + "\n")
		}
		if req.Task != "" {
			b.WriteString("题目要求：" + req.Task + "\n")
		}
		if strings.TrimSpace(req.ContentMarkdown) == "" {
			return "", "", "", fmt.Errorf("work feedback: 写作点评需要作文原文（最新版本无文字内容）")
		}
		b.WriteString("作文原文：\n" + req.ContentMarkdown)
	case k12.WorkTypeArt:
		subject = "美术"
		if body, stamp, ok := resolveWorkFeedbackSkill(loader, artFeedbackSkillName, artFeedbackSkillFile, artFeedbackRedlineAnchors); ok {
			skillStamp = stamp
			b.WriteString("以下是「美术作品反馈」技能的方法论基座，本次点评的观察框架、反馈流程与红线全部遵此执行：\n\n")
			b.WriteString(body)
			b.WriteString("\n\n——以下是本次点评任务——\n")
			b.WriteString("对孩子的美术作品按上述技能给观察描述式点评。作品原图已随本条消息附上，只依据图中可见证据，不虚构画面细节。\n")
		} else {
			b.WriteString("对孩子的美术作品给观察描述式点评：先描述画面里可见的构图、色彩、线条、空间与表达证据，")
			b.WriteString("再给一个孩子可执行的小建议。作品原图已随本条消息附上，只依据图中可见证据，不虚构画面细节。\n")
		}
		b.WriteString("红线：不打分、不评级、不排名、不做审美排名；不替孩子重画。\n")
		if req.Title != "" {
			b.WriteString("作品名：" + req.Title + "\n")
		}
		if req.Task != "" {
			b.WriteString("创作任务：" + req.Task + "\n")
		}
		if req.Intent != "" {
			b.WriteString("孩子想表达的内容：" + req.Intent + "\n")
		}
		if req.Task != "" || req.Intent != "" {
			b.WriteString("证据覆盖：逐项核对创作任务和孩子意图中明确提到的具体画面元素；看得见就必须在观察证据中点名，看不见则明确说明没有观察到，不得静默遗漏或凭文字说明虚构为可见。\n")
		}
		if req.Task != "" {
			b.WriteString("强制覆盖清单：" + req.Task + "\n")
			b.WriteString("最终正文必须逐字包含清单中的每个具体名词，并对每项分别写明图中可见的位置/颜色证据或明确没有观察到；遗漏任一项即为不合格。\n")
		}
		if strings.TrimSpace(req.ContentMarkdown) != "" {
			b.WriteString("画面文字说明：\n" + req.ContentMarkdown)
		}
	default:
		return "", "", "", fmt.Errorf("work feedback: 未知作品类型 %q", req.WorkType)
	}
	return subject, b.String(), skillStamp, nil
}
