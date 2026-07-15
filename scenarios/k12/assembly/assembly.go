// Package assembly 是 K12 场景包的 composition root——把六缝装配 + records 存储 +
// engine adapter + 用例依赖组装成一个可用的 K12 运行时。
//
// 放独立包（而非 package k12）是为避免依赖环：k12 ← usecase ← engineadapter，
// 装配需同时 import 三者，故上提到本包（谁都不 import 它）。
// cmd/main.go 只需：build solve skill → assembly.Wire(db, solveSkill, opts...) → 拿到 K12。
package assembly

import (
	"database/sql"
	"fmt"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// K12 是装配产物：注册表（六缝）+ records 存储 + 组好的用例依赖。
type K12 struct {
	Registry *scenario.Registry
	Records  *records.Store
	Deps     usecase.Deps
}

// Option 装配可选项（注入 Insights/Grounding 等 adapter）。
type Option func(*usecase.Deps)

// WithInsights 注入学情信号写入 adapter（memory 反思管线）。
func WithInsights(ins usecase.Insights) Option {
	return func(d *usecase.Deps) { d.Insights = ins }
}

// WithGrounding 注入备课卡①段教材检索 adapter（knowledge/RAG）。
func WithGrounding(g usecase.Grounding) Option {
	return func(d *usecase.Deps) { d.Grounding = g }
}

// WithRecognizer 注入识题 adapter（云端 vision）。
func WithRecognizer(rec usecase.Recognizer) Option {
	return func(d *usecase.Deps) { d.Recognizer = rec }
}

// WithProfiles 注入孩子档案读写 adapter（router agent store）。
func WithProfiles(p usecase.ProfileStore) Option {
	return func(d *usecase.Deps) { d.Profiles = p }
}

// ArchiveRestorerFactory receives the records store assembled by Wire, allowing
// the composition root to build a cross-store adapter around that exact store.
type ArchiveRestorerFactory func(*records.Store) usecase.ArchiveRestorer

// WithArchiveRestorer injects the crash-durable records/profile restore seam.
func WithArchiveRestorer(build ArchiveRestorerFactory) Option {
	return func(d *usecase.Deps) {
		if build != nil {
			d.ArchiveRestorer = build(d.Records)
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

// WithRetryGenerator 注入「再练一道」轻量出题闭包（BUG-20260712 治本）：让复习再练走单次
// reasoning 出题、不复用全对抗验算链。回填进 SolveAdapter（此时 Deps.Solver 已建好）。
func WithRetryGenerator(fn engineadapter.RetryGenerateFunc) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetRetryGen(fn)
		}
	}
}

// WithCauseSummaryGenerator 注入「记一条错题」的专用轻量错因摘要闭包。
func WithCauseSummaryGenerator(fn engineadapter.CauseSummaryGenerateFunc) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetCauseSummaryGen(fn)
		}
	}
}

// WithPrepReviewGenerator 注入教材未命中时的专用知识点回顾闭包。
func WithPrepReviewGenerator(fn engineadapter.PrepReviewGenerateFunc) Option {
	return func(d *usecase.Deps) {
		if sa, ok := d.Solver.(*engineadapter.SolveAdapter); ok && fn != nil {
			sa.SetPrepReviewGen(fn)
		}
	}
}

// Wire 装配 K12 运行时。
//
// solveSkill 由 composition root 传入（engine.NewSolveSkill(agentExecFn, reg) 的产物，
// 已注入真 agentExecFn）。它同时充当 Solver 与 Grader 两个 port（SolveAdapter）。
func Wire(db *sql.DB, solveSkill engineadapter.SolveExecutor, opts ...Option) (*K12, error) {
	if db == nil {
		return nil, fmt.Errorf("assembly: db 不可为 nil")
	}
	if solveSkill == nil {
		return nil, fmt.Errorf("assembly: solveSkill 不可为 nil")
	}

	reg := scenario.NewRegistry()
	constraint := curriculum.New() // 真课标（人教版数学），替代内存 stub
	if err := reg.Assemble(k12.Pack(constraint)); err != nil {
		return nil, fmt.Errorf("assembly: 装配 K12 场景包: %w", err)
	}

	store := records.NewStore(db, reg.Records)
	solveAdapter := engineadapter.NewSolveAdapter(solveSkill)

	deps := usecase.Deps{
		Solver:         solveAdapter,
		Grader:         solveAdapter,
		VerifiedGrader: solveAdapter,
		PrepReview:     solveAdapter,
		Records:        store,
		Constraint:     constraint,
	}
	for _, o := range opts {
		o(&deps)
	}
	return &K12{Registry: reg, Records: store, Deps: deps}, nil
}
