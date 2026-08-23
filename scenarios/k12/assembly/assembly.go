// Package assembly 是 K12 场景包的 composition root——把六缝装配 + records 存储 +
// engine adapter + 用例依赖组装成一个可用的 K12 运行时。
//
// 放独立包（而非 package k12）是为避免依赖环：k12 ← usecase ← engineadapter，
// 装配需同时 import 三者，故上提到本包（谁都不 import 它）。
// cmd/main.go 只需：build solve skill → assembly.Wire(db, solveSkill, opts...) → 拿到 K12。
package assembly

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/scenarioinstall"
)

// K12 是装配产物：注册表（六缝）+ Manifest v2 声明与安装收据（§6.2/§6.3）+
// 类型化存储（§6.9）+ Outbox 投递器 + 组好的用例依赖。
type K12 struct {
	Registry *scenario.Registry
	// Manifest ScenarioManifest v2 声明（挂载点等由 composition root 从此取，不再硬编码）。
	Manifest *scenario.Manifest
	// Receipt 安装收据（实际创建资源；卸载按它精确清理，台账见 scenario_installations）。
	Receipt *scenario.Receipt
	Records *k12storage.Store
	// Outbox 单进程投递器（§6.15）：composition root 调 Outbox.Start(ctx) 启动
	// （nudge 即时 + 轮询兜底 + 启动补投 pending）。学情信号消费者已注册。
	Outbox *k12storage.Dispatcher
	// CatalogWorker consumes the restart-durable textbook catalog queue using
	// only persisted Knowledge checkpoints and exact source spans.
	CatalogWorker *usecase.TextbookCatalogWorker
	Deps          usecase.Deps
}

// Option 装配可选项（注入 Insights/Grounding 等 adapter）。
type Option func(*usecase.Deps)

// WithInsights 注入学情信号写入 adapter（memory 反思管线）。
func WithInsights(ins usecase.Insights) Option {
	return func(d *usecase.Deps) { d.Insights = ins }
}

// WithGrounding injects textbook retrieval for tutoring tips.
func WithGrounding(g usecase.Grounding) Option {
	return func(d *usecase.Deps) { d.Grounding = g }
}

// WithRecognizer 注入识题 adapter（云端 vision）。
func WithRecognizer(rec usecase.Recognizer) Option {
	return func(d *usecase.Deps) { d.Recognizer = rec }
}

// WithCreativeWorkOCR injects the writing-photo OCR adapter. Production wires
// it from the same VisionFunc used by homework recognition.
func WithCreativeWorkOCR(rec usecase.CreativeWorkOCRRecognizer) Option {
	return func(d *usecase.Deps) { d.CreativeWorkOCR = rec }
}

// WithAnswerAnchorer 注入识题后的批量答案坐标证据 adapter。
// （AnswerGeometry 低延迟子集端口已随 /recognize/anchors 一次切换删除，§6.14。）
func WithAnswerAnchorer(anchorer usecase.AnswerAnchorer) Option {
	return func(d *usecase.Deps) {
		d.AnswerAnchorer = anchorer
	}
}

// WithProfiles 注入孩子档案读写 adapter（router agent store）。
func WithProfiles(p usecase.ProfileStore) Option {
	return func(d *usecase.Deps) { d.Profiles = p }
}

// ArchiveRestorerFactory receives the typed store assembled by Wire, allowing
// the composition root to build a cross-store adapter around that exact store.
type ArchiveRestorerFactory func(*k12storage.Store) usecase.ArchiveRestorer

// WithArchiveRestorer injects the crash-durable records/profile restore seam.
func WithArchiveRestorer(build ArchiveRestorerFactory) Option {
	return func(d *usecase.Deps) {
		if build != nil {
			d.ArchiveRestorer = build(d.Records)
			if migrator, ok := d.ArchiveRestorer.(usecase.ArchiveMigrationRestorer); ok {
				d.ArchiveMigrator = migrator
			}
		}
	}
}

// WithRenderer 注入文档渲染 adapter（PDF/Word）。
func WithRenderer(r usecase.Renderer) Option {
	return func(d *usecase.Deps) { d.Renderer = r }
}

// WithPhotoAnnotator 注入服务器端批改图像素合成 adapter（供 IM 原图批改回传）。
func WithPhotoAnnotator(a usecase.PhotoAnnotator) Option {
	return func(d *usecase.Deps) { d.PhotoAnnotator = a }
}

// WithDeliveryTransport wires the durable DD-024 send-to-phone seam. The
// transport resolves bindings and renders channel payloads; Deps owns the
// receipt-first state machine around it.
func WithDeliveryTransport(delivery usecase.DeliveryTransport) Option {
	return func(d *usecase.Deps) { d.Delivery = delivery }
}

// WithPracticeVariantGenerator injects the bounded text-only generator used by
// the durable single-practice state machine. Route selection is intentionally
// separate and is frozen by the usecase before this adapter may run.
func WithPracticeVariantGenerator(generator usecase.PracticeVariantGenerator) Option {
	return func(d *usecase.Deps) { d.PracticeVariant = generator }
}

// WithCauseSummaryGenerator 注入「记一条错题」的专用轻量错因摘要闭包。
func WithCauseSummaryGenerator(fn engineadapter.CauseSummaryGenerateFunc) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetCauseSummaryGen(fn)
		}
	}
}

// WithTutoringTipsReviewGenerator injects the bounded explanation generator
// used when textbook evidence is unavailable.
func WithTutoringTipsReviewGenerator(fn engineadapter.TutoringTipsReviewGenerateFunc) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetTutoringTipsReviewGen(fn)
		}
	}
}

// WithParentTeachingGuideGenerator injects the dedicated structured generator
// for each problem on a blank worksheet.
func WithParentTeachingGuideGenerator(fn engineadapter.ParentTeachingGuideGenerateFunc) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetParentTeachingGuideGen(fn)
		}
	}
}

// WithParentTeachingSkillLoader 让逐题家长讲法消费建档锁定的教学 Skill 正文。
func WithParentTeachingSkillLoader(fn engineadapter.SkillContentLoader) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetParentTeachingSkillLoader(fn)
		}
	}
}

// WithWorkFeedbackGenerator 注入作品证据化点评生成闭包（PRD §3.10 / INV-011）：
// 写作好句摘出+一处具体建议、美术观察描述式，单次 reasoning，不走验算链。
func WithWorkFeedbackGenerator(fn engineadapter.WorkFeedbackGenerateFunc) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetWorkFeedbackGen(fn)
		}
	}
}

// WithAccumulationMetadataDeriver wires the trusted structured classifier used
// by the content-only current accumulation command. Production must provide an
// implementation explicitly; the use case has no guessed fallback.
func WithAccumulationMetadataDeriver(
	deriver usecase.AccumulationMetadataDeriver,
) Option {
	return func(d *usecase.Deps) {
		d.AccumulationMetadata = deriver
	}
}

// WithWorkFeedbackVision 注入美术作品观察式点评的视觉闭包（PRD §3.10）：复用识题链的
// VisionFunc 原语（原图 bytes + 提示词 → 视觉模型文本），美术点评据原图可见证据生成；
// 未注入时美术点评在 adapter 层诚实报错（需要视觉通道）。
func WithWorkFeedbackVision(fn engineadapter.VisionFunc) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetWorkFeedbackVision(fn)
		}
	}
}

// WithWorkFeedbackSkillLoader 注入盘上 marketplace skill 内容加载闭包：作品点评方法论
// 基座「盘上→内嵌→硬编码」链的第一级（hub 发新版 skill、用户端 seed/安装更新后，
// 点评不重新编译即用新正文）。未注入时从内嵌发版快照起链。
func WithWorkFeedbackSkillLoader(fn engineadapter.SkillContentLoader) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetWorkFeedbackSkillLoader(fn)
		}
	}
}

// Wire 装配 K12 运行时（自建单场景 Registry + 收据台账落同库；测试与单场景部署入口）。
//
// solveSkill 由 composition root 传入（engine.NewSolveSkill(agentExecFn, reg) 的产物，
// 已注入真 agentExecFn）。它同时充当 Solver 与 Grader 两个 port（SolveAdapter）。
func Wire(db *sql.DB, solveSkill engineadapter.SolveExecutor, opts ...Option) (*K12, error) {
	if db == nil {
		return nil, fmt.Errorf("assembly: db 不可为 nil")
	}
	reg := scenario.NewRegistry()
	reg.Recorder = scenarioinstall.New(db)
	return WireInto(context.Background(), reg, db, solveSkill, opts...)
}

// WireInto 把 K12 按 ScenarioManifest v2 安装进**平台级** Registry（§6.3 原子安装 +
// InstallationReceipt），再组装运行时。多场景 composition root（cmd/main.go）持有 reg，
// 可继续 Install 其他场景包；平台内核始终不 import 场景包（AP-1）。
func WireInto(ctx context.Context, reg *scenario.Registry, db *sql.DB, solveSkill engineadapter.SolveExecutor, opts ...Option) (*K12, error) {
	if reg == nil {
		return nil, fmt.Errorf("assembly: registry 不可为 nil")
	}
	if db == nil {
		return nil, fmt.Errorf("assembly: db 不可为 nil")
	}
	if solveSkill == nil {
		return nil, fmt.Errorf("assembly: solveSkill 不可为 nil")
	}

	constraint := curriculum.New() // 真课标（人教版数学），替代内存 stub
	man := k12.Manifest(constraint)
	receipt, err := reg.Install(ctx, man)
	if err != nil {
		return nil, fmt.Errorf("assembly: 安装 K12 场景包: %w", err)
	}

	store := k12storage.NewStore(db, reg.Records)
	solveAdapter := engineadapter.NewSolveAdapter(solveSkill)

	deps := usecase.Deps{
		Solver:              solveAdapter,
		Grader:              solveAdapter,
		VerifiedGrader:      solveAdapter,
		WeeklyAssessment:    usecase.NewVerifiedSolutionWeeklyAssessor(solveAdapter),
		TutoringTipsReview:  solveAdapter,
		ParentTeachingGuide: solveAdapter,
		Records:             store,
		TextbookOwnerID:     "desktop-user",
		PageAssets:          assetstore.PageStore{},
		Constraint:          constraint,
	}
	for _, o := range opts {
		o(&deps)
	}
	// Outbox 投递器 + 学情信号消费者（§6.9：投影失败不撤销域写，重试只补投影）。
	// 消费者持 Deps.Insights（opts 应用后再建，确保拿到注入的 adapter）。
	outbox := k12storage.NewDispatcher(store, usecase.InsightsConsumer{Insights: deps.Insights})
	catalogWorker := usecase.NewTextbookCatalogWorker(
		store,
		usecase.TextbookCatalogCheckpointExtractor{},
		usecase.TextbookCatalogWorkerConfig{
			WorkerID: "k12-catalog-worker", Lease: 30 * time.Second,
			HeartbeatInterval: 10 * time.Second, ExtractTimeout: 2 * time.Minute,
			MaxAttempts: 4, RetryBase: 2 * time.Second, RetryMax: time.Minute,
			RecoveryBatch: 32,
		},
	)
	return &K12{
		Registry: reg, Manifest: man, Receipt: receipt, Records: store,
		Outbox: outbox, CatalogWorker: catalogWorker, Deps: deps,
	}, nil
}
