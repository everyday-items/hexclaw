package main

// 真实模型（gpt-5.6 系列 via ~/.hexclaw/hexclaw.yaml）的 K12 作品点评真机探针：
// ① 写作点评走纯文本 completion；② 美术点评走视觉通道（程序生成一张儿童画风格 PNG，
// 原图多模态发给视觉模型做观察式点评）。全链走生产用例 GenerateWorkFeedback：
// 归属校验 → adapter 构造提示词/载图 → 真机调用 → INV-011 入库拦截 → feedback_ready。
// 若模型输出违反 INV-011 被拦截，探针如实记录（拦截器工作，不是 bug）。运行示例：
//
//	HEXCLAW_K12_WORKFB_PROBE=1 \
//	go test ./cmd/hexclaw -run TestK12WorkFeedback_RealModel -v -count=1 -timeout 10m
import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// §13.2 发布级要求：作品点评探针必须支持显式真实文件输入，并断言 FX-WRITING-001 /
// FX-ART-001 的真实 SHA 确实进入模型请求（合成作文/合成画不能通过 LIVE-WRITING/LIVE-ART）。
// 提供真实 fixture 绝对路径即切换到真素材通道；缺省仍走内置合成样本（PR 必跑不外泄隐私）。
const (
	fxWritingImageEnv = "HEXCLAW_K12_WRITING_FILE"
	fxArtImageEnv     = "HEXCLAW_K12_ART_FILE"
	fxWritingSHA256   = "3b238c46e0ae4515f7b35a28bcfd37081ba1d59a9dfa2b30bf17784aaf3e9157"
	fxArtSHA256       = "7eb16fdbe398236cdf2ce31ea6d2fac5e4787ea3004b96ab74a3eebd540f1d93"
)

// readWorkFeedbackFixture 读取真实 fixture（只读），断言其字节 SHA-256 与 §1.2 冻结值一致，
// 并确认是真图片。返回原始字节；任何不一致 fail-closed，杜绝拿错图/被压缩重编码的素材冒充。
func readWorkFeedbackFixture(t *testing.T, path, wantSHA string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取真实 fixture 失败 path=%q: %v", path, err)
	}
	if mime := http.DetectContentType(raw); !strings.HasPrefix(mime, "image/") {
		t.Fatalf("真实 fixture 不是图片 path=%q mime=%q", path, mime)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(raw))
	if sum != wantSHA {
		t.Fatalf("真实 fixture SHA 不符 path=%q got=%s want=%s（原图必须只读且与 §1.2 一致）", path, sum, wantSHA)
	}
	t.Logf("FIXTURE_VERIFIED: path=%s bytes=%d sha256=%s", path, len(raw), sum)
	return raw
}

// workFeedbackProbeStubSolve 满足装配所需的 SolveExecutor；作品点评链路绝不触发 solve，
// 一旦被调用即失败取证。
type workFeedbackProbeStubSolve struct{}

func (workFeedbackProbeStubSolve) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return nil, fmt.Errorf("work feedback probe: solve executor must not be invoked")
}

// workFeedbackProbeProfiles 内存档案：给点评链路提供年级（生产由 router agent store 承担）。
type workFeedbackProbeProfiles struct{ p k12.ChildProfile }

func (s *workFeedbackProbeProfiles) GetProfile(context.Context, string) (k12.ChildProfile, error) {
	return s.p, nil
}
func (s *workFeedbackProbeProfiles) SaveProfile(_ context.Context, _ string, p k12.ChildProfile) error {
	s.p = p
	return nil
}

func TestK12WorkFeedback_RealModel(t *testing.T) {
	if os.Getenv("HEXCLAW_K12_WORKFB_PROBE") != "1" {
		t.Skip("set HEXCLAW_K12_WORKFB_PROBE=1 to run the real-model work feedback probe")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load local HexClaw config: %v", err)
	}
	applyK12PhotoProbeProviderOverride(t, cfg)
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("build real provider router: %v", err)
	}

	ctx := context.Background()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "k12-workfb-probe.db"))
	if err != nil {
		t.Fatalf("create isolated store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init isolated store: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT OR IGNORE INTO agents(name) VALUES('child-tutor')`); err != nil {
		t.Fatalf("seed probe agent: %v", err)
	}

	// 写作纯文本闭包：与 cmd/hexclaw/main.go 的 workFeedbackGenFn 同构（系统提示、红线、
	// egress 归类逐字一致），美术误入即诚实报错。
	writingGen := func(genCtx context.Context, subject, prompt, grade string) (string, error) {
		provider := router.Default()
		if provider == nil {
			return "", fmt.Errorf("k12 作品点评: 没有可用的默认 LLM Provider")
		}
		if subject == "美术" {
			return "", fmt.Errorf("美术观察式点评必须走视觉通道（原图多模态），不允许纯文本生成")
		}
		task := prompt
		if grade != "" {
			task += "\n（点评口径贴合" + grade + "孩子的水平，用家长和孩子都能懂的话。）"
		}
		cctx := egress.WithRequest(genCtx, egress.PurposeGeneralChat, "k12-work-feedback", egress.ClassGeneral)
		cctx, ccancel := context.WithTimeout(cctx, 120*time.Second)
		defer ccancel()
		temp := 0.3
		resp, err := provider.Complete(cctx, hexagon.CompletionRequest{
			Messages: []hexagon.Message{
				{Role: hexagon.RoleSystem, Content: "你是小学写作辅导老师，给孩子作文做形成性点评。红线：只点评不打分——禁止输出任何分数、等第、评级、排名；" +
					"不代写——禁止给范文、禁止改写或重写全文。点评框架与输出格式按用户消息里的技能指引执行；语气鼓励、具体、可执行。"},
				{Role: hexagon.RoleUser, Content: task},
			},
			MaxTokens:   1536,
			Temperature: &temp,
		})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
	// 美术视觉闭包：与 main.go 的 workFeedbackVisionFn 同构。路由镜像 RouteForVision
	// （配置的默认 provider + 其配置模型，不走 cost-aware）。
	// lastVisionSHA 记录最近一次真正送进视觉请求的图片字节 SHA-256——用于断言真实
	// FX-ART-001 / FX-WRITING-001 原图确实到达模型（§13.2 的 E1 证据），而非探针在别处替换。
	var lastVisionSHA string
	visionFn := func(visionCtx context.Context, imageBytes []byte, prompt string) (string, error) {
		provider := router.Default()
		if provider == nil {
			return "", fmt.Errorf("没有可用的默认 LLM Provider")
		}
		lastVisionSHA = fmt.Sprintf("%x", sha256.Sum256(imageBytes))
		visionModel := router.ProviderModel(router.DefaultName())
		mime := http.DetectContentType(imageBytes)
		if !strings.HasPrefix(mime, "image/") {
			mime = "image/png"
		}
		t.Logf("vision call: provider=%q model=%q bytes=%d mime=%s", provider.Name(), visionModel, len(imageBytes), mime)
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imageBytes)
		cctx := egress.WithRequest(visionCtx, egress.PurposeVisionChat, "k12-work-feedback", egress.ClassSensitiveMedia)
		cctx, ccancel := context.WithTimeout(cctx, 180*time.Second)
		defer ccancel()
		resp, err := provider.Complete(cctx, hexagon.CompletionRequest{
			Model: visionModel,
			Messages: []hexagon.Message{{
				Role: hexagon.RoleUser,
				MultiContent: []llm.ContentPart{
					llm.NewTextPart(prompt),
					llm.NewImageURLPart(dataURL, "high"),
				},
			}},
		})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}

	// 盘上 marketplace 加载链真机取证：与生产同路径——seed 到真实 skills 目录（幂等）、
	// Init 扫描注册、loader 现读盘。写作点评应**优先加载盘上 seed 的 writing-feedback**
	// （证据：DISK_SKILL_LOADED 日志 + 落库 feedback_skill 以 /disk 结尾）。
	mp := marketplace.NewMarketplace(cfg.Skills.Dir)
	if _, serr := mp.SeedFromFS(k12.BundledSkillsFS(), "skills"); serr != nil {
		t.Fatalf("seed skills to disk: %v", serr)
	}
	if err := mp.Init(); err != nil {
		t.Fatalf("init marketplace: %v", err)
	}
	var diskLoaded []string
	diskLoader := func(name string) (string, error) {
		mdSkill, ok := mp.Get(name)
		if !ok {
			return "", fmt.Errorf("skill %q 未安装", name)
		}
		data, rerr := os.ReadFile(mdSkill.FilePath)
		if rerr != nil {
			return "", rerr
		}
		diskLoaded = append(diskLoaded, name)
		t.Logf("DISK_SKILL_LOADED: %s (%d bytes) from %s", name, len(data), mdSkill.FilePath)
		return string(data), nil
	}

	// 建档给年级：writing-feedback skill 的学段标尺要求「识别不到年级时先问家长，不猜」，
	// 生产链路点评前档案必在（建档守门）；不建档的探针会得到「请问几年级」的合规反问
	// 而非点评正文。这里注入内存档案对齐生产前置条件。
	profiles := &workFeedbackProbeProfiles{p: k12.ChildProfile{ChildName: "小雨", GradeTerm: "三年级上"}}
	runtime, err := assembly.Wire(
		store.DB(),
		workFeedbackProbeStubSolve{},
		assembly.WithWorkFeedbackGenerator(writingGen),
		assembly.WithWorkFeedbackVision(visionFn),
		assembly.WithWorkFeedbackSkillLoader(diskLoader),
		assembly.WithProfiles(profiles),
	)
	if err != nil {
		t.Fatalf("wire K12 runtime: %v", err)
	}

	t.Run("写作点评_真机", func(t *testing.T) {
		// 写作反馈消费文字原文；真实 FX-WRITING-001 是手写作文照片。给了真实文件即：
		// ① 断言原图 SHA==§1.2；② 用同一批视觉模型逐字誊录（断言原图 SHA 进入誊录请求=E1）；
		// ③ 把誊录文字作原文喂点评；④ 断言点评引用了作文真实内容（爸爸/程序员/河蟹…），不套模板。
		var realWriting bool
		var essayKeywords []string
		essay := "今天放学的时候下了一场小雨。雨点打在伞上，发出嗒嗒嗒的声音，像是在敲小鼓。" +
			"路边的小草被雨水洗得发亮，空气里有泥土的味道。我和同桌共撑一把伞，我们把伞往对方那边推来推去，" +
			"到家的时候两个人的肩膀都湿了一半，可是我们都笑了。"
		title, task := "《放学路上的小雨》", "写一段放学路上的见闻，写出真实感受"
		if writingFile := strings.TrimSpace(os.Getenv(fxWritingImageEnv)); writingFile != "" {
			raw := readWorkFeedbackFixture(t, writingFile, fxWritingSHA256)
			transcribed, terr := visionFn(ctx, raw,
				"这是一张小学生手写作文的照片。请逐字誊录图中作文正文（含标题），只输出作文文字本身，不要添加任何点评、说明或标注。")
			if terr != nil {
				t.Fatalf("真实作文誊录失败: %v", terr)
			}
			if lastVisionSHA != fxWritingSHA256 {
				t.Fatalf("誊录请求进入的图片 SHA=%s，应为 FX-WRITING-001 %s（真实原图未到达模型）", lastVisionSHA, fxWritingSHA256)
			}
			if strings.TrimSpace(transcribed) == "" {
				t.Fatal("真实作文誊录为空")
			}
			essay = transcribed
			title, task = "《我的好爸爸》", "写一个你熟悉的人，写出他的特点和你们之间的事"
			// 黄金锚点（§1.2）：作文涉及父亲、程序员、河蟹 AI、Skill、用提问教数学。
			// 点评必须落在真实原文上——命中任一关键词即证明未套用「校园春景」类无关模板。
			essayKeywords = []string{"爸爸", "父亲", "程序员", "河蟹", "Skill", "提问", "数学"}
			realWriting = true
			t.Logf("REAL_WRITING_TRANSCRIBED: chars=%d\n----\n%s\n----", len([]rune(essay)), essay)
		}
		id, _, err := runtime.Deps.CreateCreativeWork(ctx, "child-tutor", "workfb-probe", k12.CreativeWorkFields{
			WorkType: k12.WorkTypeWriting, Title: title, Task: task,
			Versions: []k12.CreativeWorkVersion{{ContentMarkdown: essay}},
		})
		if err != nil {
			t.Fatalf("创建写作作品: %v", err)
		}
		started := time.Now()
		v, err := runtime.Deps.GenerateWorkFeedback(ctx, "child-tutor", id)
		if err != nil {
			if strings.Contains(err.Error(), "INV-011") {
				// 拦截器按契约工作：模型返回了打分/等第/代写口径，被拒绝入库。如实记录。
				t.Logf("INV011_INTERCEPTED（写作）: %v", err)
				return
			}
			t.Fatalf("写作点评真机生成失败: %v", err)
		}
		last := v.Fields.Versions[len(v.Fields.Versions)-1]
		if v.Record.Status != k12.WorkStatusFeedbackReady {
			t.Fatalf("状态应为 feedback_ready，got %s", v.Record.Status)
		}
		if last.FeedbackSource != k12.FeedbackSourceAI {
			t.Fatalf("来源应为 ai，got %q", last.FeedbackSource)
		}
		if strings.TrimSpace(last.Feedback) == "" {
			t.Fatal("点评不得为空")
		}
		// 加载链取证：盘上 seed 的 writing-feedback 被优先加载，且来源戳随点评落库。
		if !containsString(diskLoaded, "writing-feedback") {
			t.Fatalf("写作点评应优先加载盘上 writing-feedback，实际盘上加载: %v", diskLoaded)
		}
		if !strings.HasPrefix(last.FeedbackSkill, "writing-feedback@") || !strings.HasSuffix(last.FeedbackSkill, "/disk") {
			t.Fatalf("落库 feedback_skill 应为 writing-feedback@…/disk，got %q", last.FeedbackSkill)
		}
		// 红线：只点评不打分（LIVE-WRITING-001）——任何路径都不得出现分数/等第/排名口径。
		for _, banned := range []string{"打分", "评分", "等第", "甲等", "排名", "名次", "满分"} {
			if strings.Contains(last.Feedback, banned) {
				t.Fatalf("写作点评不得含 %q：%s", banned, last.Feedback)
			}
		}
		if realWriting {
			// LIVE-WRITING-001 黄金：点评必须引用真实原文，不得套用「校园春景」类无关模板。
			hit := 0
			for _, kw := range essayKeywords {
				if strings.Contains(last.Feedback, kw) {
					hit++
				}
			}
			if hit == 0 {
				t.Fatalf("真实写作点评未命中任何原文关键词 %v（疑似套用无关模板，未锚定真实原文）：\n%s",
					essayKeywords, last.Feedback)
			}
			if strings.Contains(last.Feedback, "校园春景") {
				t.Fatalf("真实写作点评出现无关模板词「校园春景」：%s", last.Feedback)
			}
			t.Logf("REAL_WRITING_FEEDBACK_ANCHORED: 命中原文关键词 %d/%d", hit, len(essayKeywords))
		}
		t.Logf("WRITING_FEEDBACK_OK: real=%v elapsed=%s chars=%d feedback_skill=%s\n----\n%s\n----",
			realWriting, time.Since(started).Round(time.Millisecond), len([]rune(last.Feedback)), last.FeedbackSkill, last.Feedback)
	})

	t.Run("美术点评_真机_视觉通道", func(t *testing.T) {
		// 给了真实 FX-ART-001 即走真素材：原图（只读，SHA==§1.2）作 SourceAssetID，
		// 生产 loadWorkArtImage 读盘 → 多模态视觉调用；断言真实原图 SHA 进入请求=E1。
		var realArt bool
		var artFeatures []string
		artPath := filepath.Join(t.TempDir(), "child-drawing.png")
		title, task, intent := "《我家门前》", "画一画自己家门前的景色", "想画出晴天里我家小房子和大树"
		if artFile := strings.TrimSpace(os.Getenv(fxArtImageEnv)); artFile != "" {
			readWorkFeedbackFixture(t, artFile, fxArtSHA256) // 校验只读 + SHA==§1.2
			artPath = artFile
			title, task, intent = "《我的画》", "画一幅自己喜欢的画", ""
			// 黄金可见证据（§1.2）：中央棕发粉蝴蝶结女孩、紫爱心上衣、蓝裙粉鞋、右下橙猫、
			// 左上彩虹白云、周围爱心星星、底部绿地面。观察须逐项属实——命中任一即证明看的是真图。
			artFeatures = []string{"女孩", "蝴蝶结", "爱心", "裙", "猫", "彩虹", "星星", "云", "草", "地面"}
			realArt = true
		} else if err := os.WriteFile(artPath, drawChildStyleArtPNG(t), 0o600); err != nil {
			t.Fatalf("write probe drawing: %v", err)
		}
		id, _, err := runtime.Deps.CreateCreativeWork(ctx, "child-tutor", "workfb-probe", k12.CreativeWorkFields{
			WorkType: k12.WorkTypeArt, Title: title, Task: task,
			Intent:   intent,
			Versions: []k12.CreativeWorkVersion{{SourceAssetID: artPath}},
		})
		if err != nil {
			t.Fatalf("创建美术作品: %v", err)
		}
		started := time.Now()
		v, err := runtime.Deps.GenerateWorkFeedback(ctx, "child-tutor", id)
		if err != nil {
			if strings.Contains(err.Error(), "INV-011") {
				t.Logf("INV011_INTERCEPTED（美术）: %v", err)
				return
			}
			t.Fatalf("美术点评真机生成失败（含 proxy 图片格式证据时如实上报）: %v", err)
		}
		last := v.Fields.Versions[len(v.Fields.Versions)-1]
		if v.Record.Status != k12.WorkStatusFeedbackReady {
			t.Fatalf("状态应为 feedback_ready，got %s", v.Record.Status)
		}
		if last.FeedbackSource != k12.FeedbackSourceAI {
			t.Fatalf("来源应为 ai，got %q", last.FeedbackSource)
		}
		// 入库成功即 INV-011 已通过；再显式断言不含打分/等第口径（双保险取证）。
		for _, banned := range []string{"打分", "评分", "等第", "甲等", "排名", "名次"} {
			if strings.Contains(last.Feedback, banned) {
				t.Fatalf("美术点评不得含 %q：%s", banned, last.Feedback)
			}
		}
		if !strings.HasSuffix(last.FeedbackSkill, "/disk") {
			t.Fatalf("美术点评 feedback_skill 应为盘上来源（…/disk），got %q", last.FeedbackSkill)
		}
		if realArt {
			// LIVE-ART-001 黄金：真实文件 SHA 到达请求 + 可见证据准确。
			if lastVisionSHA != fxArtSHA256 {
				t.Fatalf("视觉请求进入的图片 SHA=%s，应为 FX-ART-001 %s（真实原图未到达模型）", lastVisionSHA, fxArtSHA256)
			}
			hit := 0
			for _, feat := range artFeatures {
				if strings.Contains(last.Feedback, feat) {
					hit++
				}
			}
			if hit == 0 {
				t.Fatalf("真实美术点评未命中任何可见证据 %v（疑似虚构/未看真图）：\n%s", artFeatures, last.Feedback)
			}
			t.Logf("REAL_ART_FEEDBACK_ANCHORED: FX-ART SHA 进入请求✓ 命中可见证据 %d/%d", hit, len(artFeatures))
		}
		t.Logf("ART_FEEDBACK_OK: real=%v elapsed=%s chars=%d feedback_skill=%s\n----\n%s\n----",
			realArt, time.Since(started).Round(time.Millisecond), len([]rune(last.Feedback)), last.FeedbackSkill, last.Feedback)
	})
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// drawChildStyleArtPNG 程序生成一张儿童画风格的图：蓝天、草地、左上角带光芒的太阳、
// 红顶小房子、棕干绿冠的大树。几何朴拙、色块平涂，贴近低年级蜡笔画的可见证据
// （构图/色彩/线条都可被观察描述）。
func drawChildStyleArtPNG(t *testing.T) []byte {
	t.Helper()
	const w, h = 640, 480
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	sky := color.RGBA{R: 150, G: 205, B: 245, A: 255}
	grass := color.RGBA{R: 105, G: 185, B: 90, A: 255}
	sun := color.RGBA{R: 250, G: 210, B: 60, A: 255}
	wall := color.RGBA{R: 235, G: 180, B: 120, A: 255}
	roof := color.RGBA{R: 205, G: 70, B: 60, A: 255}
	door := color.RGBA{R: 120, G: 75, B: 40, A: 255}
	trunk := color.RGBA{R: 130, G: 85, B: 45, A: 255}
	crown := color.RGBA{R: 60, G: 140, B: 65, A: 255}

	fillRect := func(x0, y0, x1, y1 int, c color.RGBA) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.SetRGBA(x, y, c)
			}
		}
	}
	fillCircle := func(cx, cy, r int, c color.RGBA) {
		for y := cy - r; y <= cy+r; y++ {
			for x := cx - r; x <= cx+r; x++ {
				if x >= 0 && y >= 0 && x < w && y < h && (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
					img.SetRGBA(x, y, c)
				}
			}
		}
	}

	fillRect(0, 0, w, h, sky)
	fillRect(0, 340, w, h, grass)
	// 太阳 + 八道光芒。
	fillCircle(80, 80, 42, sun)
	for i := 0; i < 8; i++ {
		dx, dy := [8]int{1, 1, 0, -1, -1, -1, 0, 1}[i], [8]int{0, 1, 1, 1, 0, -1, -1, -1}[i]
		for step := 50; step < 75; step++ {
			x, y := 80+dx*step, 80+dy*step
			if x >= 0 && y >= 0 && x < w && y < h {
				fillCircle(x, y, 3, sun)
			}
		}
	}
	// 小房子：墙 + 三角红顶 + 门。
	fillRect(220, 250, 380, 370, wall)
	for row := 0; row < 70; row++ {
		half := 90 * (70 - row) / 70
		fillRect(300-half, 180+row, 300+half, 181+row, roof)
	}
	fillRect(285, 305, 320, 370, door)
	// 大树：树干 + 三球树冠。
	fillRect(480, 260, 505, 380, trunk)
	fillCircle(492, 225, 55, crown)
	fillCircle(455, 255, 40, crown)
	fillCircle(530, 255, 40, crown)

	tmp := filepath.Join(t.TempDir(), "encode-buf.png")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatalf("create png buf: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close png: %v", err)
	}
	raw, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read png: %v", err)
	}
	return raw
}
