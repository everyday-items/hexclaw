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
	visionFn := func(visionCtx context.Context, imageBytes []byte, prompt string) (string, error) {
		provider := router.Default()
		if provider == nil {
			return "", fmt.Errorf("没有可用的默认 LLM Provider")
		}
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
		essay := "今天放学的时候下了一场小雨。雨点打在伞上，发出嗒嗒嗒的声音，像是在敲小鼓。" +
			"路边的小草被雨水洗得发亮，空气里有泥土的味道。我和同桌共撑一把伞，我们把伞往对方那边推来推去，" +
			"到家的时候两个人的肩膀都湿了一半，可是我们都笑了。"
		id, _, err := runtime.Deps.CreateCreativeWork(ctx, "child-tutor", "workfb-probe", k12.CreativeWorkFields{
			WorkType: k12.WorkTypeWriting, Title: "《放学路上的小雨》", Task: "写一段放学路上的见闻，写出真实感受",
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
		t.Logf("WRITING_FEEDBACK_OK: elapsed=%s chars=%d feedback_skill=%s\n----\n%s\n----",
			time.Since(started).Round(time.Millisecond), len([]rune(last.Feedback)), last.FeedbackSkill, last.Feedback)
	})

	t.Run("美术点评_真机_视觉通道", func(t *testing.T) {
		artPath := filepath.Join(t.TempDir(), "child-drawing.png")
		if err := os.WriteFile(artPath, drawChildStyleArtPNG(t), 0o600); err != nil {
			t.Fatalf("write probe drawing: %v", err)
		}
		id, _, err := runtime.Deps.CreateCreativeWork(ctx, "child-tutor", "workfb-probe", k12.CreativeWorkFields{
			WorkType: k12.WorkTypeArt, Title: "《我家门前》", Task: "画一画自己家门前的景色",
			Intent:   "想画出晴天里我家小房子和大树",
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
		t.Logf("ART_FEEDBACK_OK: elapsed=%s chars=%d feedback_skill=%s\n----\n%s\n----",
			time.Since(started).Round(time.Millisecond), len([]rune(last.Feedback)), last.FeedbackSkill, last.Feedback)
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
