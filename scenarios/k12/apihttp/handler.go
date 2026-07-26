// Package apihttp 提供 K12 场景的 HTTP 子路由（前端联调契约）。
//
// AP-1：K12 的 HTTP 端点由场景包自己提供一个 http.Handler，由 composition root 挂载到
// 路径前缀（如 /api/k12/），平台 api/ 层不认识 K12。前端对着本包的 DTO 编码即可联调。
package apihttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// CronRegistrar 是自动化沉淀「调度」缝：K12 产出声明式 CronSpec，由 composition root
// 包平台 cron.Scheduler 实现本接口（AP-1：K12 不 import cron）。nil 时 provision 返回 501。
type CronRegistrar interface {
	// Register 注册一个默认任务（幂等键 = agent+kind，重复注册应覆盖或跳过由实现决定）。
	Register(ctx context.Context, kind string, spec usecase.CronSpec, platform, chatID, userID string) (jobID string, err error)
	// ProvisionDefaults 在同一 durable 事务中覆盖四个默认任务并回收历史超集。
	// 任一注册/归并/回收失败时，durable rows 与 active map 必须都保持调用前快照。
	ProvisionDefaults(ctx context.Context, specs []usecase.CronSpec, platform, chatID, userID string) (jobIDs []string, removed []ReclaimedCronJob, err error)
	// ReclaimStale 回收本 agent 名下不在 keepJobIDs 集合内的历史 K12 job（§6.14 一次
	// 切换终局批）：以稳定幂等键前缀 "<agent>/" 识别 K12 归属（跨 user_id），绝不按
	// 展示名匹配——用户自建任务（无稳定键）一个都不许动。返回回收清单供取证。
	ReclaimStale(ctx context.Context, agentName string, keepJobIDs []string) (removed []ReclaimedCronJob, err error)
	// EnsureMissing 只补 exact SourceKey 缺项；已有任务（含暂停、改时区、改投递、改脚本）
	// 必须原样保留。用于存量档案保存后的作用域修复，不做全局启动扫描。
	EnsureMissing(ctx context.Context, kind string, spec usecase.CronSpec, platform, chatID, userID string) (jobID string, created bool, err error)
}

// ReclaimedCronJob 是 provision stale 回收的取证条目。
type ReclaimedCronJob struct {
	JobID     string `json:"job_id"`
	Name      string `json:"name"`
	SourceKey string `json:"source_key"`
}

// ErrBindConflict 限绑业务冲突哨兵（§3.12「一个私聊同时只能接收一个孩子的助手」）：
// composition root 的 IMBinder 实现用它包装限绑拒绝，HTTP 层映射 409（区别于真内部错误 500）。
var ErrBindConflict = errors.New("绑定冲突")

// IMBinder 是 IM 入站路由「绑定」缝：把某 IM 私聊会话（platform+chat_id）绑到某辅导实例，
// 之后该私聊的作业消息由平台路由（agent_rules）投给这个 Agent（架构 §4.11 一对一私聊）。
// composition root 包平台 router.Dispatcher + store 实现（AP-1：K12 不 import router）。
type IMBinder interface {
	Bind(ctx context.Context, platform, instanceID, chatID, agentName string) error
}

// Runtime 是本 handler 依赖的 K12 运行时（assembly.K12 满足它；用接口避免 import 环）。
type Runtime struct {
	Views   *scenario.ViewExtensionRegistry
	Records *k12storage.Store
	Deps    usecase.Deps
	// ModelSnapshotResolver is the shared control-plane guard for generic
	// GradingJob creation. It validates explicit provider/model selections and
	// resolves automatic selections before any durable record is written.
	ModelSnapshotResolver usecase.GradingModelSnapshotResolver
	// Cron 可选：注入后 POST /cron/provision 可为实例注册默认自动化任务。
	Cron CronRegistrar
	// Binder 可选：注入后 POST /bind-im 可把 IM 群绑到辅导实例（入站路由）。
	Binder IMBinder
	// BaseURL 本机 API 基址（如 http://127.0.0.1:8787），生成投递脚本 http_get 目标用。
	// 空时 provision 用请求体里的 base_url。
	BaseURL string
	// Grading 可选：作业子链内部的 GradingJob 编排器（§6.7/§6.15）。
	// 客户端只能通过 ImageTasks facade 驱动图片任务；GradingJob 不再拥有公开路由。
	Grading *usecase.GradingOrchestrator
	// ImageTasks is the sole public image command/query facade. Internal
	// GradingJob and OCR resources are never addressed by clients.
	ImageTasks *usecase.ImageTaskCoordinator
	// WorkFeedback owns direct-text CreativeWork feedback workers. Image-backed
	// works remain under ImageTasks; both coordinators share durable generation
	// checkpoints but recover disjoint roots.
	WorkFeedback *usecase.CreativeWorkFeedbackCoordinator
	// PracticeGeneration owns durable one-click mistake -> practice workers.
	// SQLite is the queue/source of truth; HTTP only accepts commands and
	// schedules the already-persisted job.
	PracticeGeneration *usecase.SinglePracticeGenerationCoordinator
	// PracticeReturnRegrade owns durable PracticeReturnAsset -> GradingJob
	// workers. The HTTP handler persists the return first and only then nudges
	// this coordinator.
	PracticeReturnRegrade *usecase.PracticeReturnRegradeCoordinator
	// PrincipalMode is "local_loopback" for the single-user Desktop runtime or
	// "remote" when an authentication middleware owns identity.
	PrincipalMode string
	// OwnerScope is server composition state in local_loopback mode. It is
	// never read from an HTTP body, query, or header.
	OwnerScope string
	// AuthenticatedOwnerScope resolves only an already-authenticated middleware
	// context in remote mode. Taking context.Context (not *http.Request)
	// deliberately prevents this endpoint from treating request headers as
	// identity.
	AuthenticatedOwnerScope func(context.Context) (string, error)
}

// NewHandler 返回 K12 的 HTTP 子路由（Go 1.22+ method+path 路由）。
func NewHandler(rt Runtime) http.Handler {
	if strings.TrimSpace(rt.PrincipalMode) == "" {
		rt.PrincipalMode = "local_loopback"
	}
	if rt.PrincipalMode == "local_loopback" &&
		strings.TrimSpace(rt.OwnerScope) == "" {
		rt.OwnerScope = "desktop-user"
	}
	mux := http.NewServeMux()
	h := &handler{rt: rt}
	mux.HandleFunc("GET /view-descriptor", h.viewDescriptor)
	// POST /recognize、POST /recognize/anchors 与全部 /grading-jobs* 公开路由已删除：
	// 图片任务统一走 /image-tasks exact-set，识题、锚点和 GradingJob 只属于内部作业子链。
	// /grade（单题补批）与 /solve（空白题求解）为甄别保留项：仍被 Job 外合法路径消费。
	mux.HandleFunc("POST /grade", h.grade)
	mux.HandleFunc("POST /record-mistake", h.recordMistake)
	mux.HandleFunc("POST /solve", h.solve)
	mux.HandleFunc("GET /mistakes", h.mistakes)
	mux.HandleFunc("DELETE /mistakes/{record_id}", h.deleteMistake)
	mux.HandleFunc("POST /mistakes/{record_id}/archive", h.archiveMistake)
	mux.HandleFunc("POST /mistakes/{record_id}/restore", h.restoreMistake)
	mux.HandleFunc("POST /mistakes/{record_id}/practice-generation", h.startPracticeGeneration)
	mux.HandleFunc("GET /mistakes/{record_id}/practice-generation", h.getPracticeGeneration)
	mux.HandleFunc("POST /mistakes/{record_id}/practice-generation/retry", h.retryPracticeGeneration)
	mux.HandleFunc("GET /review-queue", h.reviewQueue)
	mux.HandleFunc("GET /insight-report", h.insightReport)
	mux.HandleFunc("POST /mark-mastered", h.markMastered)
	mux.HandleFunc("POST /tutoring-tips", h.tutoringTips)
	// DD-024: tutoring tips and creative-work feedback share durable delivery
	// receipts; provider acceptance is not proof of delivery.
	mux.HandleFunc("POST /tutoring-tips/send", h.sendTutoringTips)
	mux.HandleFunc("GET /delivery-receipts/{id}", h.getDeliveryReceipt)
	mux.HandleFunc("POST /delivery-receipts/{id}/retry", h.retryDeliveryReceipt)
	mux.HandleFunc("POST /delivery-receipts/{id}/query", h.queryDeliveryReceipt)
	mux.HandleFunc("GET /delivery-batches/{id}", h.getDeliveryBatch)
	mux.HandleFunc("POST /delivery-batches/{id}/retry", h.retryDeliveryBatch)
	mux.HandleFunc("POST /delivery-batches/{id}/query", h.queryDeliveryBatch)
	mux.HandleFunc("POST /grounding", h.addGrounding)
	mux.HandleFunc("POST /accumulation", h.addAccumulation)
	mux.HandleFunc("GET /accumulation", h.listAccumulation)
	mux.HandleFunc("GET /accumulation/{id}", h.getAccumulation)
	mux.HandleFunc("POST /accumulation/{id}/send", h.sendAccumulation)
	// 积累检验出口（§3.9）：生成默写题加入练习集待打印（item added_via=accumulation）。
	mux.HandleFunc("POST /accumulation/{id}/dictation-to-basket", h.accumDictationToBasket)
	mux.HandleFunc("DELETE /accumulation/{id}", h.deleteAccumulation)
	// 练习集（PRD §3.8）：草稿→确认（发布门）→发送→回传→复批→关闭；draft/confirmed 可取消。
	// POST /practice-sets（整卷直建）已随切换日死刑名单删除（执行计划 §3.4 端点冻结）：
	// 装篮命令（basket/items → finalize）是唯一创建路径。
	mux.HandleFunc("GET /practice-sets", h.listPracticeSets)
	mux.HandleFunc("GET /practice-sets/{id}", h.getPracticeSet)
	mux.HandleFunc("GET /practice-sets/{id}/paper", h.getPracticePaper)
	mux.HandleFunc("POST /practice-sets/{id}/verify", h.verifyPracticeItem)
	// 2026-07-18 购物车裁决：命令端点。confirm/assign 已删除——打印/发送即确认（finalize 一步固化）。
	mux.HandleFunc("POST /practice-sets/custom-paper", h.generateCustomPaper)
	mux.HandleFunc("POST /practice-sets/basket/items", h.addToBasket)
	mux.HandleFunc("POST /practice-sets/{id}/items/remove", h.removeFromBasket)
	mux.HandleFunc("POST /practice-sets/{id}/finalize", h.finalizePracticeSet)
	// DD-023 native two-phase printing. The legacy /finalize endpoint remains for
	// older clients; new Desktop builds use only PrintJob prepare/events/retry.
	mux.HandleFunc("POST /practice-sets/{id}/print-jobs", h.preparePracticePrintJob)
	mux.HandleFunc("POST /print-jobs", h.prepareGenericPrintJob)
	mux.HandleFunc("POST /print-artifacts", h.preparePrintableArtifact)
	mux.HandleFunc("GET /print-artifacts/{id}/content", h.getPrintableArtifactContent)
	mux.HandleFunc("GET /print-jobs/{id}", h.getPracticePrintJob)
	mux.HandleFunc("GET /print-jobs/{id}/paper", h.getPracticePrintJobPaper)
	mux.HandleFunc("POST /print-jobs/{id}/events", h.recordPracticePrintEvent)
	mux.HandleFunc("POST /print-jobs/{id}/commit", h.commitPracticePrintReceipt)
	mux.HandleFunc("POST /print-jobs/{id}/retry", h.retryPracticePrintJob)
	mux.HandleFunc("POST /practice-sets/{id}/submit", h.submitPracticeSet)
	mux.HandleFunc("POST /practice-sets/{id}/grade", h.gradePracticeSet)
	mux.HandleFunc("POST /practice-sets/{id}/close", h.closePracticeSet)
	mux.HandleFunc("POST /practice-sets/{id}/cancel", h.cancelPracticeSet)
	// Sole public image exact-set. GradingJob/OCR remain internal aggregates.
	mux.HandleFunc("POST /image-tasks", h.createImageTask)
	mux.HandleFunc("GET /image-tasks/{id}", h.getImageTask)
	mux.HandleFunc("POST /image-tasks/{id}/confirm", h.confirmImageTask)
	mux.HandleFunc("POST /image-tasks/{id}/retry", h.retryImageTask)
	mux.HandleFunc("POST /image-tasks/{id}/cancel", h.cancelImageTask)
	mux.HandleFunc("GET /image-tasks/{id}/result", h.getImageTaskResult)
	mux.HandleFunc("POST /image-tasks/{dispatch_id}/problems/{problem_id}/source-actions", h.problemSourceAction)
	// 作品（PRD §3.10）：draft→点评→修改稿→再点评；只点评不打分不代写（INV-011）。
	mux.HandleFunc("POST /creative-works", h.createCreativeWork)
	mux.HandleFunc("GET /creative-works", h.listCreativeWorks)
	mux.HandleFunc("GET /creative-works/{id}", h.getCreativeWork)
	mux.HandleFunc("POST /creative-works/{id}/generate-feedback", h.generateWorkFeedback)
	mux.HandleFunc("POST /creative-works/{id}/send", h.sendCreativeWork)
	mux.HandleFunc("DELETE /creative-works/{id}", h.deleteCreativeWork)
	// 作品照片最小资产服务（§3.10 / §5.5 source_asset_id；魔数/上限/归属契约见 asset_handler.go）。
	mux.HandleFunc("POST /assets", h.uploadAsset)
	mux.HandleFunc("GET /assets/{file}", h.getAsset)
	mux.HandleFunc("GET /backup", h.backup)
	mux.HandleFunc("POST /restore", h.restore)
	mux.HandleFunc("POST /restore-as", h.restoreAs)
	mux.HandleFunc("POST /restore-as/{migration_id}/rollback", h.rollbackRestoreAs)
	mux.HandleFunc("GET /export", h.export)
	mux.HandleFunc("GET /mistake-sheet", h.mistakeSheet)
	mux.HandleFunc("GET /profile", h.getProfile)
	mux.HandleFunc("PUT /profile", h.updateProfile)
	mux.HandleFunc("GET /curriculum-catalog", h.getCurriculumCatalog)
	mux.HandleFunc("GET /curriculum-progress", h.getCurriculumProgress)
	mux.HandleFunc("GET /weekly-practice/settings", h.getWeeklyPracticeSettings)
	mux.HandleFunc("PUT /profile-bundle", h.updateProfileBundle)
	mux.HandleFunc("POST /weekly-practice/plans", h.ensureWeeklyPracticePlan)
	mux.HandleFunc("GET /weekly-practice/plans/current", h.getCurrentWeeklyPracticePlan)
	mux.HandleFunc("GET /weekly-practice/plans/history", h.listWeeklyPracticeHistory)
	mux.HandleFunc("GET /weekly-practice/snapshots/{id}", h.getWeeklyPracticeSnapshot)
	mux.HandleFunc("POST /weekly-practice/plans/{id}/prepare-output", h.prepareWeeklyPracticeOutput)
	mux.HandleFunc("POST /weekly-practice/snapshots/{id}/send", h.sendWeeklyPracticeSnapshot)
	mux.HandleFunc("POST /weekly-practice/snapshots/{id}/attempts", h.submitWeeklyPracticeAttempt)
	mux.HandleFunc("POST /weekly-practice/plans/{id}/save-to-practice-set", h.saveWeeklyPracticeToPracticeSet)
	mux.HandleFunc("POST /cold-start", h.coldStart)
	// GET /study-time 已删除（架构设计 v0.5.0《明确不做》#6：不做学习时长与无证据投入指标）。
	mux.HandleFunc("POST /tutor-turn", h.tutorTurn)
	// 自动化沉淀投递端点（PRD §3.6）：返回**纯文本**投递内容，空 body = 本期无内容（静默跳过）。
	// 供平台 cron 的 Starlark 脚本 http_get 抓取后 emit → Deliverer 投递到 IM 群/桌面。
	mux.HandleFunc("GET /cron/mistake-sheet", h.cronMistakeSheet)
	// §3.13 每周复习第一步（执行计划 §3.0 治本③）：到期错题自动装篮（幂等）；
	// weekly-sheet 的 Starlark 脚本先 http_post 此端点再抓错题卷。
	mux.HandleFunc("POST /cron/fill-basket", h.cronFillBasket)
	mux.HandleFunc("GET /cron/daily-reminder", h.cronDailyReminder)
	mux.HandleFunc("GET /cron/return-reminder", h.cronReturnReminder)
	mux.HandleFunc("GET /cron/monthly-report", h.cronMonthlyReport)
	mux.HandleFunc("GET /cron/semester-check", h.cronSemesterCheck)
	mux.HandleFunc("GET /cron/year-archive", h.cronYearArchive)
	mux.HandleFunc("POST /cron/provision", h.cronProvision)
	mux.HandleFunc("POST /cron/reconcile-defaults", h.cronReconcileDefaults)
	mux.HandleFunc("POST /bind-im", h.bindIM)
	return mux
}

type handler struct{ rt Runtime }

// --- DTO：前端稳定契约 ---

type gradeReq struct {
	Agent           string   `json:"agent"`
	Subject         string   `json:"subject"`
	Grade           string   `json:"grade"`
	SourceSession   string   `json:"source_session"`
	Problem         string   `json:"problem"`
	StudentAnswer   string   `json:"student_answer"`
	KnowledgePoints []string `json:"knowledge_points"`
}

type gradeResp struct {
	Solution string `json:"solution"`
	// Verdict 判定五值（§4.5 布尔 correct 删除，§6.14 授权破坏性契约变更）：
	// 批改路径 = 批改判定（agree=答对 / disagree=答错）；解题分叉与超纲 = 验算/超纲结论。
	// 徽章强弱仍由 badge/evidence_type（验算证据）承载，与批改判定解耦。
	Verdict       string `json:"verdict"`
	EvidenceType  string `json:"evidence_type"`
	Badge         string `json:"badge"`
	WrongStep     string `json:"wrong_step,omitempty"`
	ErrorCause    string `json:"error_cause,omitempty"`
	OutOfScope    bool   `json:"out_of_scope"`
	OutOfScopeKP  string `json:"out_of_scope_kp,omitempty"`
	RecordCreated bool   `json:"record_created"`
	RecordID      string `json:"record_id,omitempty"`
	// CurriculumUnmapped 词表外知识点（fail-visible，PRD §5.2.4 / bug 2026-07-18）：
	// 超纲硬拦截对这些 KP 不生效，前端须显性提示「不在课标映射内」。
	CurriculumUnmapped []string `json:"curriculum_unmapped,omitempty"`
	// SolveOnly=true 表示本次 student_answer 为空，内部转「解题」分叉（非批改）：
	// 只返回 solution，无批改判定与入库。前端应按解题口径呈现，不显示对/错。
	SolveOnly bool `json:"solve_only"`
}

type mistakeDTO struct {
	RecordID       string `json:"record_id"`
	Question       string `json:"question"`
	KnowledgePoint string `json:"knowledge_point"`
	ErrorCause     string `json:"error_cause"`
	Status         string `json:"status"`
	Version        int    `json:"version"`
	DueAt          *int64 `json:"due_at,omitempty"`
	// 跨科复习队列用：subject=学科（数学/语文/英语），review_kind=再练方式（verify=验算链变式 / verbatim=原词重现字符比对）。
	Subject    string `json:"subject,omitempty"`
	ReviewKind string `json:"review_kind,omitempty"`
	// SpotCheckState 抽查复验状态（§3.6：none/scheduled/passed/failed）。前端只消费 failed
	// →「家长确认（复验未过）」事实标注；scheduled 不呈现（不打抽查标签，规则 1）。
	SpotCheckState    string `json:"spot_check_state,omitempty"`
	ParentConfirmedAt int64  `json:"parent_confirmed_at,omitempty"`
	ArchivedReason    string `json:"archived_reason,omitempty"`
	ArchivedAt        int64  `json:"archived_at,omitempty"`
	RestoredAt        int64  `json:"archive_restored_at,omitempty"`
	// Restorable 是服务端按当前状态与合法快照计算的能力投影，不进入持久化或备份。
	Restorable bool `json:"restorable"`
}

type viewDescriptorDTO struct {
	HeaderTabs          []string `json:"header_tabs"`
	MessageBadges       []string `json:"message_badges"`
	ComposerPlaceholder string   `json:"composer_placeholder"`
	ComposerChips       []string `json:"composer_chips"`
	RecordCollections   []string `json:"record_collections"`
	SidePanels          []string `json:"side_panels"`
	Actions             []string `json:"actions"`
	I18nKeys            []string `json:"i18n_keys"`
	SchemaVersion       int      `json:"schema_version"`
}

// --- handlers ---

// viewDescriptor GET /view-descriptor?slot=tutor —— 供前端 chat shell 渲染视图槽。
func (h *handler) viewDescriptor(w http.ResponseWriter, r *http.Request) {
	slot := r.URL.Query().Get("slot")
	if slot == "" {
		slot = "tutor"
	}
	ve, ok := h.rt.Views.Get(slot)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown view slot")
		return
	}
	writeJSON(w, http.StatusOK, viewDescriptorDTO{
		HeaderTabs: ve.HeaderTabs, MessageBadges: ve.MessageBadges,
		ComposerPlaceholder: ve.ComposerPlaceholder, ComposerChips: ve.ComposerChips,
		RecordCollections: ve.RecordCollections,
		SidePanels:        ve.SidePanels, Actions: ve.Actions, I18nKeys: ve.I18nKeys, SchemaVersion: ve.SchemaVersion,
	})
}

// bboxDTO 是独立答案锚定阶段核验出的学生作答区域归一化边界框（0~1）。
type bboxDTO struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type recognizedQuestionDTO struct {
	ProblemID        string              `json:"problem_id"`
	ProblemKind      usecase.ProblemKind `json:"problem_kind"`
	ParentProblemID  string              `json:"parent_problem_id,omitempty"`
	SubproblemNo     string              `json:"subproblem_no,omitempty"`
	SourceNumberPath []string            `json:"source_number_path"`
	DisplayLabel     string              `json:"display_label"`
	PageAssetID      string              `json:"page_asset_id"`
	AttemptID        string              `json:"attempt_id,omitempty"`
	// Question 是当前 surface 的安全展示投影；raw/canonical 双事实同时返回供确认 UI
	// 对照。canonical_valid=false 时 Question 必须回退为可复制 raw，绝不返回空白。
	Question          string   `json:"question"`
	RawTranscription  string   `json:"raw_transcription"`
	CanonicalMarkdown string   `json:"canonical_markdown"`
	CanonicalValid    bool     `json:"canonical_valid"`
	CanonicalVersion  int      `json:"canonical_version"`
	KnowledgePoints   []string `json:"knowledge_points"`
	// StudentAnswer 识题回收的孩子手写作答（未作答=空串）。前端据此区分空白题(走 /solve 求解)
	// 与已答题(走 /grade 批改)，家长可在回显门修改。
	StudentAnswer           string `json:"student_answer"`
	AnswerRawTranscription  string `json:"answer_raw_transcription,omitempty"`
	AnswerCanonicalMarkdown string `json:"answer_canonical_markdown,omitempty"`
	AnswerCanonicalValid    bool   `json:"answer_canonical_valid"`
	// AnswerState 是作答事实的单一真相源。blank / present / unclear 与答案文本、图片坐标解耦。
	AnswerState usecase.AnswerState `json:"answer_state"`
	// Subject 识题自动判定的题目学科（数学/语文/英语/物理/化学，判不出=空）。
	Subject               string                  `json:"subject,omitempty"`
	RecognitionConfidence *float64                `json:"recognition_confidence,omitempty"`
	ConfirmationRequired  bool                    `json:"confirmation_required"`
	ConfirmationReasons   []usecase.OCRRiskReason `json:"confirmation_reasons,omitempty"`
	ConfirmedVersion      int                     `json:"confirmed_version"`
	InputDigest           string                  `json:"input_digest,omitempty"`
	// BBox 只在独立答案锚定阶段之后出现（GradingJob 停点产物）；核心识题永远不携带坐标。
	BBox *bboxDTO `json:"bbox,omitempty"`
}

func recognizedQuestionToDTO(question usecase.RecognizedQuestion, includeBBox bool) recognizedQuestionDTO {
	question = usecase.NormalizeRecognizedQuestion(question)
	dto := recognizedQuestionDTO{
		ProblemID: question.ProblemID, ProblemKind: question.ProblemKind,
		ParentProblemID: question.ParentProblemID, SubproblemNo: question.SubproblemNo,
		SourceNumberPath: append([]string{}, question.SourceNumberPath...),
		DisplayLabel:     question.DisplayLabel,
		PageAssetID:      question.PageAssetID, AttemptID: question.AttemptID,
		Question:         usecase.RecognizedQuestionDisplayText(question),
		RawTranscription: question.RawTranscription, CanonicalMarkdown: question.CanonicalMarkdown,
		CanonicalValid:          usecase.CanonicalMarkdownValid(question.CanonicalMarkdown),
		CanonicalVersion:        question.CanonicalVersion,
		KnowledgePoints:         append([]string{}, question.KnowledgePoints...),
		AnswerState:             question.AnswerState,
		StudentAnswer:           question.StudentAnswer,
		AnswerRawTranscription:  question.AnswerRawTranscription,
		AnswerCanonicalMarkdown: question.AnswerCanonicalMarkdown,
		AnswerCanonicalValid:    question.AnswerState != usecase.AnswerStatePresent || usecase.CanonicalMarkdownValid(question.AnswerCanonicalMarkdown),
		Subject:                 question.Subject,
		RecognitionConfidence:   question.RecognitionConfidence,
		ConfirmationRequired:    question.ConfirmationRequired,
		ConfirmationReasons:     question.ConfirmationReasons,
		ConfirmedVersion:        question.ConfirmedVersion,
		InputDigest:             question.InputDigest,
	}
	if includeBBox && question.BBox != nil {
		dto.BBox = &bboxDTO{
			X: question.BBox.X,
			Y: question.BBox.Y,
			W: question.BBox.W,
			H: question.BBox.H,
		}
	}
	return dto
}

// dominantSubject 取识题逐题学科里出现最多的那一个作为整卷学科（平票取先出现者，全空则空）。
func dominantSubject(qs []usecase.RecognizedQuestion) string {
	count := map[string]int{}
	var order []string
	for _, q := range qs {
		if q.Subject == "" {
			continue
		}
		if _, seen := count[q.Subject]; !seen {
			order = append(order, q.Subject)
		}
		count[q.Subject]++
	}
	best := ""
	for _, s := range order {
		if best == "" || count[s] > count[best] {
			best = s
		}
	}
	return best
}

// resolveGrade 年级确定性注入（AP-4 / PRD §3.3.3+§5.2.4）：未显式传年级时据 agent 从孩子档案
// 取生效年级，供仍接受年级字段的 solve/grade/tutor-turn 携带学段边界，避免超纲解法。
// 辅导要点不接受客户端年级，而是从 owner-scoped 持久档案派生。agent 空或档案缺失时回退原值。
func (h *handler) resolveGrade(ctx context.Context, agent, grade string) string {
	if grade != "" || agent == "" {
		return grade
	}
	if p, err := h.rt.Deps.GetProfile(ctx, agent); err == nil {
		return p.GradeTerm
	}
	return grade
}

// stripDataURI 去掉 data:image/...;base64, 前缀（前端可能带）。
func stripDataURI(s string) string {
	if i := strings.Index(s, ";base64,"); i >= 0 {
		return s[i+len(";base64,"):]
	}
	return s
}

func decodeRequiredImage(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(stripDataURI(encoded))
	if encoded == "" {
		return nil, errors.New("image_base64 is required")
	}
	image, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(image) == 0 {
		return nil, errors.New("invalid base64 image")
	}
	return image, nil
}

// grade POST /grade —— 批改一道题的完整闭环。
func (h *handler) grade(w http.ResponseWriter, r *http.Request) {
	var req gradeReq
	if !decode(w, r, &req) {
		return
	}
	res, err := h.rt.Deps.GradeHomeworkProblem(r.Context(), usecase.GradeRequest{
		AgentName: req.Agent, Subject: req.Subject, Grade: h.resolveGrade(r.Context(), req.Agent, req.Grade), SourceSession: req.SourceSession,
		Problem: req.Problem, StudentAnswer: req.StudentAnswer, KnowledgePoints: req.KnowledgePoints,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gradeRespFromResult(res))
}

// gradeRespFromResult 统一 GradeResult → wire DTO（判定五值口径，§4.5）：
// 批改发生时 verdict = 批改判定（Outcome.Verdict）；解题分叉/超纲无批改判定，
// 沿用验算/超纲 verdict（Evidence.Verdict），不伪造二元结论。
func gradeRespFromResult(res usecase.GradeResult) gradeResp {
	verdict := res.Outcome.Verdict
	if verdict == "" {
		verdict = res.Evidence.Verdict
	}
	return gradeResp{
		Solution: res.Solution, Verdict: string(verdict),
		EvidenceType: string(res.Evidence.EvidenceType), Badge: res.Evidence.Badge(),
		WrongStep: res.Outcome.WrongStep, ErrorCause: res.Outcome.ErrorCause,
		OutOfScope: res.OutOfScope, OutOfScopeKP: res.OutOfScopeKP,
		RecordCreated: res.RecordCreated, RecordID: res.RecordID,
		CurriculumUnmapped: res.CurriculumUnmapped,
		SolveOnly:          res.SolveOnly,
	}
}

type recordMistakeReq struct {
	Agent           string   `json:"agent"`
	Subject         string   `json:"subject"`
	Grade           string   `json:"grade"`
	SourceSession   string   `json:"source_session"`
	Problem         string   `json:"problem"`
	StudentAnswer   string   `json:"student_answer"`
	ErrorCause      string   `json:"error_cause"`
	KnowledgePoints []string `json:"knowledge_points"`
}

type recordMistakeResp struct {
	RecordCreated bool   `json:"record_created"`
	RecordID      string `json:"record_id,omitempty"`
	ErrorCause    string `json:"error_cause,omitempty"`
}

// recordMistake POST /record-mistake —— 家长「记一条错题」的**轻量记录路径**（BUG-20260712 治本）。
// 直接把已知错题入错题本（错因留空时单次轻量归纳），**绝不跑 solve+verify 对抗验算链**（秒级完成）。
func (h *handler) recordMistake(w http.ResponseWriter, r *http.Request) {
	var req recordMistakeReq
	if !decode(w, r, &req) {
		return
	}
	res, err := h.rt.Deps.RecordMistake(r.Context(), usecase.RecordMistakeRequest{
		AgentName: req.Agent, Subject: req.Subject, Grade: h.resolveGrade(r.Context(), req.Agent, req.Grade),
		SourceSession: req.SourceSession, Problem: req.Problem, StudentAnswer: req.StudentAnswer,
		ErrorCause: req.ErrorCause, KnowledgePoints: req.KnowledgePoints,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recordMistakeResp{
		RecordCreated: res.RecordCreated, RecordID: res.RecordID, ErrorCause: res.ErrorCause,
	})
}

type solveReq struct {
	Agent           string   `json:"agent"`
	Subject         string   `json:"subject"`
	Grade           string   `json:"grade"`
	Problem         string   `json:"problem"`
	KnowledgePoints []string `json:"knowledge_points"`
}

type solveResp struct {
	Solution     string `json:"solution"`
	Verdict      string `json:"verdict"`
	EvidenceType string `json:"evidence_type"`
	Badge        string `json:"badge"`
	OutOfScope   bool   `json:"out_of_scope"`
	OutOfScopeKP string `json:"out_of_scope_kp,omitempty"`
	// CurriculumUnmapped 词表外知识点（fail-visible，同 gradeResp）。
	CurriculumUnmapped []string `json:"curriculum_unmapped,omitempty"`
}

// solve POST /solve —— 空白/未作答题求解：给解法+答案+讲解，**不批改、不入错题本**。
// 与 /grade 的分工是单一真相源的显式落地：空白题走此端点，不要求填 student_answer。
func (h *handler) solve(w http.ResponseWriter, r *http.Request) {
	var req solveReq
	if !decode(w, r, &req) {
		return
	}
	res, err := h.rt.Deps.SolveHomeworkProblem(r.Context(), usecase.GradeRequest{
		AgentName: req.Agent, Subject: req.Subject,
		Grade:   h.resolveGrade(r.Context(), req.Agent, req.Grade),
		Problem: req.Problem, KnowledgePoints: req.KnowledgePoints,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, solveResp{
		Solution: res.Solution, Verdict: string(res.Evidence.Verdict),
		EvidenceType: string(res.Evidence.EvidenceType), Badge: res.Evidence.Badge(),
		OutOfScope: res.OutOfScope, OutOfScopeKP: res.OutOfScopeKP,
		CurriculumUnmapped: res.CurriculumUnmapped,
	})
}

// mistakes GET /mistakes?agent=X&status= —— 错题本列表。
func (h *handler) mistakes(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	recs, err := h.rt.Records.ListByScope(r.Context(), agent, k12.CollectionMistakes, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": toMistakeDTOs(recs)})
}

// deleteMistake DELETE /mistakes/{record_id}?agent=X —— 家长「删除这条错题」（UX-3 数据纠错）。
// 校验 agent 归属后删对应记录；record 不存在 / 越权（非本 agent）→ 404。前端在详情弹层内
// 经二次确认后调用（克制入口，非首屏主动作）。
func (h *handler) deleteMistake(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	recordID := r.PathValue("record_id")
	if agent == "" || recordID == "" {
		writeErr(w, http.StatusBadRequest, "agent / record_id 必填")
		return
	}
	if err := h.rt.Deps.DeleteMistake(r.Context(), agent, recordID); err != nil {
		// 归属校验失败 / 记录不存在 → 404；输入错 → 400；其余存储错 → 500。
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type mistakeArchiveCommandReq struct {
	Agent          string `json:"agent"`
	Version        *int   `json:"version"`
	IdempotencyKey string `json:"idempotency_key"`
}

// archiveMistake POST /mistakes/{record_id}/archive —— 家长「不再复习」。
// 这是可恢复软归档；返回推进后的 version，供 8 秒 Undo 直接做 CAS。
func (h *handler) archiveMistake(w http.ResponseWriter, r *http.Request) {
	var req mistakeArchiveCommandReq
	if !decodeStrict(w, r, &req) {
		return
	}
	recordID := r.PathValue("record_id")
	if req.Agent == "" || recordID == "" || req.IdempotencyKey == "" ||
		req.Version == nil || *req.Version < 0 {
		writeErr(w, http.StatusBadRequest, "agent / record_id / version / idempotency_key 必填，version 不可为负数")
		return
	}
	rec, err := h.rt.Deps.ArchiveMistake(
		r.Context(), req.Agent, recordID, *req.Version, req.IdempotencyKey,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	fields, err := k12.ParseMistakeFields(rec.Fields)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mistakeDTOFrom(rec, fields))
}

// restoreMistake POST /mistakes/{record_id}/restore —— Undo 与已归档长期恢复共用。
func (h *handler) restoreMistake(w http.ResponseWriter, r *http.Request) {
	var req mistakeArchiveCommandReq
	if !decodeStrict(w, r, &req) {
		return
	}
	recordID := r.PathValue("record_id")
	if req.Agent == "" || recordID == "" || req.IdempotencyKey == "" ||
		req.Version == nil || *req.Version < 0 {
		writeErr(w, http.StatusBadRequest, "agent / record_id / version / idempotency_key 必填，version 不可为负数")
		return
	}
	rec, err := h.rt.Deps.RestoreMistake(
		r.Context(), req.Agent, recordID, *req.Version, req.IdempotencyKey,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	fields, err := k12.ParseMistakeFields(rec.Fields)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mistakeDTOFrom(rec, fields))
}

// reviewQueue GET /review-queue?agent=X —— 到期该练队列。
func (h *handler) reviewQueue(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	items, err := h.rt.Deps.ReviewQueue(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]mistakeDTO, 0, len(items))
	for _, it := range items {
		kind := "verify" // 数理化：验算链变式
		if it.IsAccum() {
			kind = "verbatim" // 语英字词：原词重现·字符比对
		}
		out = append(out, mistakeDTO{
			RecordID: it.Record.RecordID, Question: it.Title(), KnowledgePoint: it.Point(),
			ErrorCause: it.Point(), Status: it.Record.Status, Version: it.Record.Version,
			DueAt: it.Record.DueAt, Subject: it.Subject(), ReviewKind: kind,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// insightReport GET /insight-report?agent=X —— 学情报告（趋势/薄弱TOP3/复习完成率/连续挫败/建议）。
func (h *handler) insightReport(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	rep, err := h.rt.Deps.InsightReport(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	md, skip := usecase.RenderReportMarkdown(rep, rep.GradeTerm)
	if skip {
		title := rep.GradeTerm + "学习概览"
		if rep.GradeTerm == "" {
			title = "学习概览"
		}
		md = "## " + title + "\n\n本月还没有错题记录，继续保持。"
	}
	content, manifest := k12RenderProjection(messagecontent.ProducerReport, "zh-CN", md)
	writeJSON(w, http.StatusOK, struct {
		usecase.InsightReport
		MessageContent *messagecontent.MessageContent `json:"message_content,omitempty"`
		RenderManifest *messagecontent.RenderManifest `json:"render_manifest,omitempty"`
	}{InsightReport: rep, MessageContent: content, RenderManifest: manifest})
}

type markMasteredReq struct {
	Agent    string `json:"agent"`
	RecordID string `json:"record_id"`
	Version  int    `json:"version"`
}

// markMastered POST /mark-mastered —— 「他会了」。
func (h *handler) markMastered(w http.ResponseWriter, r *http.Request) {
	var req markMasteredReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.RecordID == "" {
		writeErr(w, http.StatusBadRequest, "agent / record_id 必填")
		return
	}
	if err := h.rt.Deps.MarkMastered(r.Context(), req.Agent, req.RecordID, req.Version); err != nil {
		// BUG-2：按错误类型分流——版本冲突 409 / 记录不存在 404 / 非法状态 400 / 其余存储错 500。
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type singlePracticeGenerationReq struct {
	Agent          string `json:"agent"`
	IdempotencyKey string `json:"idempotency_key"`
	Grade          string `json:"grade,omitempty"`
	Textbook       string `json:"textbook,omitempty"`
	Difficulty     string `json:"difficulty,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	SourceSession  string `json:"source_session,omitempty"`
}

func (req singlePracticeGenerationReq) command() usecase.SinglePracticeGenerationRequest {
	return usecase.SinglePracticeGenerationRequest{
		IdempotencyKey: req.IdempotencyKey,
		Grade:          req.Grade,
		Textbook:       req.Textbook,
		Difficulty:     req.Difficulty,
		Provider:       req.Provider,
		Model:          req.Model,
		SourceSession:  req.SourceSession,
	}
}

func (h *handler) startPracticeGeneration(w http.ResponseWriter, r *http.Request) {
	if h.rt.PracticeGeneration == nil {
		writeErr(w, http.StatusServiceUnavailable, "逐题出题任务暂不可用")
		return
	}
	var req singlePracticeGenerationReq
	if !decode(w, r, &req) {
		return
	}
	sourceID := strings.TrimSpace(r.PathValue("record_id"))
	if strings.TrimSpace(req.Agent) == "" || sourceID == "" {
		writeErr(w, http.StatusBadRequest, "agent / record_id 必填")
		return
	}
	view, err := h.rt.Deps.StartSinglePracticeGeneration(
		r.Context(), req.Agent, sourceID, req.command(),
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	if view.State == usecase.SinglePracticePending {
		h.rt.PracticeGeneration.StartAsync(req.Agent, view.GenerationJobID)
	}
	writeJSON(w, http.StatusAccepted, view)
}

func (h *handler) getPracticeGeneration(w http.ResponseWriter, r *http.Request) {
	agentName := strings.TrimSpace(r.URL.Query().Get("agent"))
	sourceID := strings.TrimSpace(r.PathValue("record_id"))
	if agentName == "" || sourceID == "" {
		writeErr(w, http.StatusBadRequest, "agent / record_id 必填")
		return
	}
	view, err := h.rt.Deps.GetSinglePracticeGeneration(
		r.Context(), agentName, sourceID,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *handler) retryPracticeGeneration(w http.ResponseWriter, r *http.Request) {
	if h.rt.PracticeGeneration == nil {
		writeErr(w, http.StatusServiceUnavailable, "逐题出题任务暂不可用")
		return
	}
	var req struct {
		Agent string `json:"agent"`
	}
	if !decode(w, r, &req) {
		return
	}
	sourceID := strings.TrimSpace(r.PathValue("record_id"))
	if strings.TrimSpace(req.Agent) == "" || sourceID == "" {
		writeErr(w, http.StatusBadRequest, "agent / record_id 必填")
		return
	}
	view, err := h.rt.Deps.RetrySinglePracticeGeneration(
		r.Context(), req.Agent, sourceID,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	h.rt.PracticeGeneration.StartAsync(req.Agent, view.GenerationJobID)
	writeJSON(w, http.StatusAccepted, view)
}

type tutoringTipsReq struct {
	Agent      string `json:"agent"`
	DispatchID string `json:"dispatch_id"`
}

type groundingReq struct {
	Agent string `json:"agent"`
	// Subject 分科教材学科（六学科枚举校验）；空 = 不分科旧语义（前向兼容）。
	Subject string `json:"subject"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// addGrounding POST /grounding —— 家长教材按 agent（× 学科）scope 入库，与辅导要点读侧同键。
func (h *handler) addGrounding(w http.ResponseWriter, r *http.Request) {
	var req groundingReq
	if !decode(w, r, &req) {
		return
	}
	if err := h.rt.Deps.AddGrounding(r.Context(), req.Agent, req.Subject, req.Title, req.Content); err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type tutoringTipsSectionDTO struct {
	Title       string `json:"title"`
	Content     string `json:"content"`
	SourceLabel string `json:"source_label"`
}

// tutoringTips POST /tutoring-tips resolves the owner-scoped ImageTaskDispatch
// to its internal confirmed homework facts. The GradingJob identity is never a
// public request or response field.
func (h *handler) tutoringTips(w http.ResponseWriter, r *http.Request) {
	var req tutoringTipsReq
	if !decodeStrict(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Agent) == "" || strings.TrimSpace(req.DispatchID) == "" {
		writeErr(w, http.StatusBadRequest, "agent / dispatch_id required")
		return
	}
	if h.rt.ImageTasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "image task facade unavailable")
		return
	}
	gradingJobID, err := h.rt.ImageTasks.ResolveTutoringTipsGradingJob(
		r.Context(), req.Agent, req.DispatchID,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	tips, err := h.rt.Deps.BuildTutoringTips(r.Context(), req.Agent, gradingJobID)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	sections := make([]tutoringTipsSectionDTO, 0, len(tips.Sections))
	for _, section := range tips.Sections {
		sections = append(sections, tutoringTipsSectionDTO{
			Title: section.Title, Content: section.Content, SourceLabel: section.SourceLabel,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"knowledge_points": tips.KnowledgePoints, "sections": sections})
}

type accumReq struct {
	Content string `json:"content"`
}

type accumDTO struct {
	RecordID            string                              `json:"record_id"`
	Subject             string                              `json:"subject"`
	EntryType           string                              `json:"entry_type"`
	Content             string                              `json:"content"`
	Source              string                              `json:"source,omitempty"`
	Version             int                                 `json:"version"`
	DictationGeneration *accumulationDictationGenerationDTO `json:"dictation_generation,omitempty"`
	CreatedAt           int64                               `json:"created_at"` // unix 秒；引文列表收藏日期（原型 20260718 定案 acc-date）
}

type accumulationDictationGenerationDTO struct {
	GenerationID   string `json:"generation_id"`
	Status         string `json:"status"`
	PracticeItemID string `json:"practice_item_id,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
	Attempt        int    `json:"attempt,omitempty"`
	UpdatedAt      int64  `json:"updated_at,omitempty"`
}

func accumulationGenerationDTO(
	generation *k12.AccumulationDictationGeneration,
) *accumulationDictationGenerationDTO {
	if generation == nil {
		return nil
	}
	return &accumulationDictationGenerationDTO{
		GenerationID: generation.GenerationID, Status: generation.Status,
		PracticeItemID: generation.PracticeItemID,
		FailureReason:  generation.FailureReason,
		Attempt:        generation.Attempt, UpdatedAt: generation.UpdatedAt,
	}
}

func accumulationItemDTO(item usecase.AccumItem) accumDTO {
	return accumDTO{
		RecordID: item.Record.RecordID, Subject: item.Fields.Subject,
		EntryType: item.Fields.EntryType, Content: item.Fields.Content,
		Source: item.Fields.Source, Version: item.RowVersion,
		DictationGeneration: accumulationGenerationDTO(item.DictationGeneration),
		CreatedAt:           item.Record.CreatedAt,
	}
}

// addAccumulation POST /accumulation —— 语文/英语积累本写入。
func (h *handler) addAccumulation(w http.ResponseWriter, r *http.Request) {
	var req accumReq
	if !decodeStrict(w, r, &req) {
		return
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	commandKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if agent == "" || commandKey == "" {
		writeErr(w, http.StatusBadRequest, "agent query and Idempotency-Key required")
		return
	}
	id, created, err := h.rt.Deps.CreateCurrentAccumulation(
		r.Context(), agent, req.Content, commandKey,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"record_id": id, "created": created})
}

// sendAccumulation POST /accumulation/{id}/send reads the message content from
// the owned server-side record. Unknown request fields are rejected so clients
// cannot substitute arbitrary delivery text.
func (h *handler) sendAccumulation(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decodeStrict(w, r, &req) {
		return
	}
	req.Agent = strings.TrimSpace(req.Agent)
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	batch, _, err := h.rt.Deps.SendAccumulation(r.Context(), req.Agent, r.PathValue("id"))
	if err != nil {
		writeDeliveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

// dictationReq POST /accumulation/{id}/dictation-to-basket 请求体。
// full_dictation：古诗整首默写须家长在生成时显式选择（§3.9 默写出题格式，缺省补空式）。
type dictationReq struct {
	Agent         string `json:"agent"`
	SourceSession string `json:"source_session"`
	FullDictation bool   `json:"full_dictation"`
}

// accumDictationToBasket POST /accumulation/{id}/dictation-to-basket ——「生成默写题，加入练习集」
// （§3.9 出口）：≤20 字全文默写 / 古诗默认补空 / >100 字拒绝（400）；装篮幂等去重，家长可移除。
func (h *handler) accumDictationToBasket(w http.ResponseWriter, r *http.Request) {
	var req dictationReq
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	generation, _, _, err := h.rt.Deps.GenerateCurrentDictationToBasket(
		r.Context(), req.Agent, req.SourceSession, r.PathValue("id"),
		req.FullDictation, "dictation:"+r.PathValue("id"),
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusBadRequest), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dictation_generation": accumulationGenerationDTO(&generation),
	})
}

// listAccumulation GET /accumulation?agent=X&subject=语文 —— 积累本列表。
func (h *handler) listAccumulation(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	items, err := h.rt.Deps.ListAccumulation(r.Context(), agent, r.URL.Query().Get("subject"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]accumDTO, 0, len(items))
	for _, it := range items {
		out = append(out, accumulationItemDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getAccumulation(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	item, err := h.rt.Deps.GetAccumulation(r.Context(), agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, accumulationItemDTO(item))
}

func (h *handler) deleteAccumulation(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	expected, commandKey, ok := parseDeleteCommand(w, r)
	if !ok {
		return
	}
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	receipt, err := h.rt.Records.TombstoneCurrentObject(
		r.Context(), agent, "accumulation", r.PathValue("id"), expected, commandKey,
	)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accumulation_id": receipt.ObjectID,
		"deleted":         receipt.Deleted,
		"version":         receipt.RowVersion,
	})
}

// backup GET /backup?agent=X —— 导出家庭学习档案 .hexbak（含 checksum）。
func (h *handler) backup(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	bak, err := h.rt.Deps.Backup(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, bak)
}

// v3 内嵌内容文件经 base64 有约 4/3 膨胀；保留 128MiB 本地回环请求上限以容纳
// 多张 10MiB 白名单图片，同时继续由 MaxBytesReader fail-closed 防无界内存输入。
const maxHexbakRequestBytes int64 = 128 << 20

// restore POST /restore —— 从 .hexbak 恢复（先校验 checksum）。
func (h *handler) restore(w http.ResponseWriter, r *http.Request) {
	var bak usecase.Hexbak
	if !decodeLimit(w, r, &bak, maxHexbakRequestBytes) {
		return
	}
	// T2.6：恢复前自动快照（PRD §3.12.9），随响应回传供前端保存以便回退。
	n, snapshot, err := h.rt.Deps.RestoreWithSnapshot(r.Context(), &bak)
	if err != nil {
		// BUG-2：归档版本过新 409（需升级应用）/ checksum 不符 400（归档损坏）/ 其余存储错 500。
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": n, "snapshot": snapshot})
}

// restoreAs POST /restore-as —— 经监护人显式确认，把已完成版本与 checksum 校验的 v2/v3 archive
// 迁移到重建后的 Tutor。原 archive、目标快照、owner rewrite journal 与领域写入
// 由 adapter 在同一 SQLite 事务内提交。
func (h *handler) restoreAs(w http.ResponseWriter, r *http.Request) {
	var req usecase.RestoreAsRequest
	if !decodeLimit(w, r, &req, maxHexbakRequestBytes) {
		return
	}
	result, err := h.rt.Deps.RestoreAs(r.Context(), req)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// rollbackRestoreAs POST /restore-as/{migration_id}/rollback —— 从不可变恢复前快照
// 精确回退，journal 追加 rollback 事实；重复请求返回同一 rolled_back 收据。
func (h *handler) rollbackRestoreAs(w http.ResponseWriter, r *http.Request) {
	var req usecase.RestoreAsRollbackRequest
	if !decodeLimit(w, r, &req, 1<<20) {
		return
	}
	req.MigrationID = r.PathValue("migration_id")
	result, err := h.rt.Deps.RollbackRestoreAs(r.Context(), req)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// export GET /export?agent=X&format=pdf|docx|md —— 错题本导出。
// format=md 或 render 服务不可用 → 回退 Markdown JSON；pdf/docx → 渲染并流式返回二进制。
func (h *handler) export(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	md, err := h.rt.Deps.ExportMistakesMarkdown(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" || format == "md" || h.rt.Deps.Renderer == nil {
		writeJSON(w, http.StatusOK, map[string]string{"format": "markdown", "content": md})
		return
	}
	data, contentType, err := h.rt.Deps.Renderer.Render(r.Context(), md, format)
	if err != nil {
		// 渲染不可用/失败 → 优雅降级回 markdown（前端仍能拿到内容）
		writeJSON(w, http.StatusOK, map[string]string{"format": "markdown", "content": md, "render_error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", contentType)
	// §4.13 文件名规范：导出（单孩）= {孩子称呼}_学习档案_{学期}.{ext}；
	// 档案读不到（未接 Profiles / 未建档）回退现名 mistakes.{ext}。
	name := "mistakes." + format
	if h.rt.Deps.Profiles != nil {
		if p, perr := h.rt.Deps.GetProfile(r.Context(), agent); perr == nil && p.ChildName != "" && p.GradeTerm != "" {
			name = p.ChildName + "_学习档案_" + p.GradeTerm + "." + format
		}
	}
	// filename* 携带 RFC 5987 UTF-8 编码，兼容非 ASCII 称呼；filename 保留原文供现代客户端。
	w.Header().Set("Content-Disposition",
		"attachment; filename=\""+name+"\"; filename*=UTF-8''"+url.PathEscape(name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

type profileDTO struct {
	ChildName       string `json:"child_name"`
	GradeTerm       string `json:"grade_term"`
	TextbookEdition string `json:"textbook_edition"`
	Revision        int    `json:"revision"`
}

// getProfile GET /profile?agent=X —— 读孩子档案。
func (h *handler) getProfile(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	p, err := h.rt.Deps.GetProfileWithRevision(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, profileDTO{ChildName: p.ChildName, GradeTerm: p.GradeTerm, TextbookEdition: p.TextbookEdition, Revision: p.Revision})
}

type coldStartReq struct {
	Agent           string   `json:"agent"`
	ChildName       string   `json:"child_name"`
	KnowledgePoints []string `json:"knowledge_points"`
	FallbackGrade   string   `json:"fallback_grade"` // 推断不出时用（可空 → 降级不注入约束）
	Textbook        string   `json:"textbook_edition"`
	// Confirm 家长确认标记（§3.1 主流程 4：推断只返回建议，不写档案；家长确认后才创建）。
	// 缺省 false = 只读推断；显式 true 才落库。
	Confirm bool `json:"confirm"`
}

type coldStartResp struct {
	ChildName       string `json:"child_name"`
	GradeTerm       string `json:"grade_term"`
	TextbookEdition string `json:"textbook_edition"`
	Inferred        bool   `json:"inferred"` // 年级是否由知识点倒查推断
	Created         bool   `json:"created"`  // 是否新建档案（false=已有档案）
}

// coldStart POST /cold-start —— 未建档实例首拍作业：按知识点倒查推断年级（PRD §3.1.4-4）。
// 不带 confirm：只读推断，返回建议档案，**绝不写库**（§3.1 主流程 4）；
// confirm=true：家长确认后落库（已有档案不覆盖）。教材未提供留空待补充，不默认人教版。
func (h *handler) coldStart(w http.ResponseWriter, r *http.Request) {
	var req coldStartReq
	if !decode(w, r, &req) {
		return
	}
	provision := h.rt.Deps.InferProfile
	if req.Confirm {
		provision = h.rt.Deps.ColdStartProvision
	}
	res, err := provision(r.Context(), req.Agent, req.ChildName, req.KnowledgePoints, req.FallbackGrade, req.Textbook)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, coldStartResp{
		ChildName: res.Profile.ChildName, GradeTerm: res.Profile.GradeTerm,
		TextbookEdition: res.Profile.TextbookEdition, Inferred: res.Inferred, Created: res.Created,
	})
}

type updateProfileReq struct {
	Agent           string `json:"agent"`
	ChildName       string `json:"child_name"`
	GradeTerm       string `json:"grade_term"`
	TextbookEdition string `json:"textbook_edition"`
}

// updateProfile PUT /profile —— 建档 / 改档（升学改年级，校验 18 档）。
func (h *handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	var req updateProfileReq
	if !decode(w, r, &req) {
		return
	}
	p, err := h.rt.Deps.UpdateProfile(r.Context(), req.Agent, k12.ChildProfile{
		ChildName: req.ChildName, GradeTerm: req.GradeTerm, TextbookEdition: req.TextbookEdition,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	view, viewErr := h.rt.Deps.GetProfileWithRevision(r.Context(), req.Agent)
	if viewErr != nil {
		writeErr(w, httpStatusForK12Error(viewErr, http.StatusInternalServerError), viewErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, profileDTO{ChildName: p.ChildName, GradeTerm: p.GradeTerm, TextbookEdition: p.TextbookEdition, Revision: view.Revision})
}

// mistakeSheet GET /mistake-sheet?agent=X —— 生成本周错题卷（到期该练，只出题）。
func (h *handler) mistakeSheet(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	md, err := h.rt.Deps.MistakeSheetMarkdown(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"format": "markdown", "content": md})
}

type tutorTurnReq struct {
	// BUG-3：agent 必填、grade 可选（前端契约）——PRD §3.3.3 + AP-4「年级来自孩子档案确定性
	// 注入」：省略 grade 时后端据 agent 从档案取生效年级，供阶段三 solve 携带年级边界。
	Agent         string `json:"agent"`
	PriorStage    int    `json:"prior_stage"`    // 上一轮阶段（0 起于阶段一）
	ParentMessage string `json:"parent_message"` // 家长本轮消息（升级/情绪/作答信号）
	StudentAnswer string `json:"student_answer"` // 家长转述的孩子作答（可空）
	Problem       string `json:"problem"`        // 题目（阶段三取验算解用）
	Grade         string `json:"grade"`          // 生效年级
}

type tutorTurnResp struct {
	Stage      int    `json:"stage"`
	Comfort    bool   `json:"comfort"`
	EmotionCue string `json:"emotion_cue,omitempty"`
	Escalated  bool   `json:"escalated"`
	PromptHint string `json:"prompt_hint"`
	Solution   string `json:"solution,omitempty"` // 阶段三验算解
	Badge      string `json:"badge,omitempty"`    // 阶段三验算徽章
}

// tutorTurn POST /tutor-turn —— 渐进提示三阶段 + 情绪守门编排（PRD §3.3.4）。
// 返回分阶段指令（供上游注入 LLM 生成话术）；阶段三附带验算过的完整解。
func (h *handler) tutorTurn(w http.ResponseWriter, r *http.Request) {
	var req tutorTurnReq
	if !decode(w, r, &req) {
		return
	}
	grade := h.resolveGrade(r.Context(), req.Agent, req.Grade)
	res, err := h.rt.Deps.TutorTurn(r.Context(), usecase.TutorTurnRequest{
		AgentName:     req.Agent,
		PriorStage:    usecase.TutorStage(req.PriorStage),
		ParentMessage: req.ParentMessage,
		StudentAnswer: req.StudentAnswer,
	}, req.Problem, grade)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	d := res.Directive
	// 徽章仅阶段三给（有验算解时）——阶段一二无解、Evidence 为零值，Badge() 会返回
	// "unverifiable" 非空串，前端契约标注 badge 仅阶段三，无条件下发会让方向提示阶段误显徽章。
	badge := ""
	if res.Solution != "" {
		badge = res.Evidence.Badge()
	}
	writeJSON(w, http.StatusOK, tutorTurnResp{
		Stage: int(d.Stage), Comfort: d.Comfort, EmotionCue: d.EmotionCue,
		Escalated: d.Escalated, PromptHint: d.PromptHint,
		Solution: res.Solution, Badge: badge,
	})
}

type bindIMReq struct {
	Agent      string `json:"agent"`             // 目标辅导实例名
	Platform   string `json:"platform"`          // IM 平台（dingtalk/feishu/…）
	InstanceID string `json:"instance_id"`       // 平台实例标识（可空）
	ChatID     string `json:"chat_id"`           // 私聊会话 ID（direct；架构 §4.11 不接受群）
	ConvType   string `json:"conversation_type"` // 会话类型（可空=direct）；群类型显式拒绝
}

// isDirectConversation 判定绑定目标是否为 direct 私聊会话（架构 §4.11：仅一对一私聊，
// 不接受群 conversation 类型；ChannelBinding.conversation_scope 当前只能是 direct）。
// 缺省（历史前端不带该字段）视为 direct（向后兼容）；显式群类型（钉钉 "2" / 通用
// "group"）与任何非 direct 值一律 fail-closed 拒绝，绝不悄悄按私聊处理。
func isDirectConversation(convType string) bool {
	switch strings.ToLower(strings.TrimSpace(convType)) {
	case "", "1", "direct":
		return true
	default:
		return false
	}
}

// bindIM POST /bind-im —— 把某 IM 私聊会话（platform+chat_id）绑到辅导实例（架构 §4.11
// 一对一私聊 / §6.10 忽略群消息）。绑定后该私聊的入站作业消息由平台 agent_rules 路由到
// 这个 Agent。仅接受 direct 会话，群 conversation 类型拒绝。需注入 IMBinder。
func (h *handler) bindIM(w http.ResponseWriter, r *http.Request) {
	if h.rt.Binder == nil {
		writeErr(w, http.StatusNotImplemented, "im binder 未注入")
		return
	}
	var req bindIMReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.Platform == "" || req.ChatID == "" {
		writeErr(w, http.StatusBadRequest, "agent / platform / chat_id 必填")
		return
	}
	if !isDirectConversation(req.ConvType) {
		// 架构 §4.11/§6.10：只支持家长与机器人的一对一私聊，群聊不绑不路由。
		writeErr(w, http.StatusBadRequest, "仅支持一对一私聊绑定，不接受群聊会话")
		return
	}
	if err := h.rt.Binder.Bind(r.Context(), req.Platform, req.InstanceID, req.ChatID, req.Agent); err != nil {
		// 限绑冲突（§3.12）是业务裁决不是服务器故障：409 + 家长向文案原样透传。
		if errors.Is(err, ErrBindConflict) {
			writeErr(w, http.StatusConflict, strings.TrimPrefix(err.Error(), ErrBindConflict.Error()+": "))
			return
		}
		writeErr(w, http.StatusInternalServerError, "绑定失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bound": true, "agent": req.Agent, "platform": req.Platform, "chat_id": req.ChatID,
	})
}

// --- 自动化沉淀投递端点（纯文本；空 body = 静默跳过）---

// cronMistakeSheet GET /cron/mistake-sheet?agent=X —— 周五错题卷投递内容。
// 无到期错题 → 空 body（跳过，不发空卷）。
func (h *handler) cronMistakeSheet(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	items, err := h.rt.Deps.ReviewQueue(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(items) == 0 { // 本周无到期错题 → 静默跳过
		writeText(w, "")
		return
	}
	md, err := h.rt.Deps.MistakeSheetMarkdown(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeText(w, md)
}

// cronFillBasket POST /cron/fill-basket?agent=X —— §3.13 每周复习自动装篮（§3.8 装篮入口2）。
// 调 FillBasketFromDue：到期复习项逐题原题重现装篮（added_via=weekly），幂等去重——
// cron 重触发不重复装，重复调用安全。响应 {added, skipped}。
func (h *handler) cronFillBasket(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	added, skipped, err := h.rt.Deps.FillBasketFromDue(r.Context(), agent, "cron-weekly")
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "skipped": skipped})
}

// cronDailyReminder GET /cron/daily-reminder?agent=X —— 每日复习提醒。
// 无待复习 → 空 body（跳过）。
func (h *handler) cronDailyReminder(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	text, skip, err := h.rt.Deps.DailyReminder(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if skip {
		writeText(w, "")
		return
	}
	writeText(w, text)
}

// cronReturnReminder GET /cron/return-reminder?agent=X —— §3.13 回传提醒（T+1 20:00 扫描节拍）。
// 昨日固化且仍未回传的卷 → 提醒文案（含 paper_no 与题数，每卷最多一次）；无 → 空 body（跳过）。
func (h *handler) cronReturnReminder(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	text, skip, err := h.rt.Deps.ReturnReminder(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if skip {
		writeText(w, "")
		return
	}
	writeText(w, text)
}

// cronMonthlyReport GET /cron/monthly-report?agent=X —— 月度学情报告（Markdown）。
// 无错题记录 → 空 body（跳过）。
func (h *handler) cronMonthlyReport(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	md, skip, err := h.rt.Deps.MonthlyReportMarkdown(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if skip {
		writeText(w, "")
		return
	}
	writeText(w, md)
}

// cronSemesterCheck GET /cron/semester-check?agent=X —— 学期确认提醒（3/1、9/1）。
// 档案无年级 / 已到最末档 → 空 body（无从推进，跳过）。
func (h *handler) cronSemesterCheck(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	p, err := h.rt.Deps.GetProfile(r.Context(), agent)
	if err != nil {
		// 未建档实例：学期确认无意义，跳过（不算错误）。
		writeText(w, "")
		return
	}
	text, _, skip := usecase.SemesterCheckText(p)
	if skip {
		writeText(w, "")
		return
	}
	writeText(w, text)
}

type provisionReq struct {
	Agent    string   `json:"agent"`
	Platform string   `json:"platform"` // IM 平台（dingtalk/feishu/…）；空 → 桌面 chat
	ChatID   string   `json:"chat_id"`  // 投递目标群/会话
	BaseURL  string   `json:"base_url"` // 本机 API 基址（Runtime.BaseURL 未配时用）
	Deliver  []string `json:"deliver"`  // 投递目标；空 → 平台默认 chat
	UserID   string   `json:"user_id"`  // cron 配额归属；空 → "k12"
}

type provisionedJob struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	JobID    string `json:"job_id"`
	Created  bool   `json:"created,omitempty"`
}

// cronReconcileDefaults is the profile-lifecycle repair path. It is deliberately
// separate from legacy /cron/provision: provision may overwrite/reclaim during
// an explicit cutover, while this endpoint only fills missing frozen defaults.
// A partial failure is retryable; already-created keys are preserved and the
// next call only attempts the remaining keys.
func (h *handler) cronReconcileDefaults(w http.ResponseWriter, r *http.Request) {
	if h.rt.Cron == nil {
		writeErr(w, http.StatusNotImplemented, "cron registrar 未注入")
		return
	}
	var req provisionReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	base := h.rt.BaseURL
	if base == "" {
		base = req.BaseURL
	}
	if base == "" {
		writeErr(w, http.StatusBadRequest, "base_url required（Runtime.BaseURL 未配）")
		return
	}
	const desktopPrincipal = "desktop-user"
	claimedUserID := strings.TrimSpace(req.UserID)
	if claimedUserID != "" && claimedUserID != desktopPrincipal {
		writeErr(w, http.StatusBadRequest, "user_id must match desktop principal")
		return
	}
	userID := desktopPrincipal
	specs := usecase.DefaultCronSpecs(base, req.Agent, req.Deliver)
	out := make([]provisionedJob, 0, len(specs))
	for _, spec := range specs {
		jobID, created, err := h.rt.Cron.EnsureMissing(
			r.Context(), string(spec.Kind), spec, req.Platform, req.ChatID, userID,
		)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "补齐 "+spec.Name+" 失败: "+err.Error())
			return
		}
		out = append(out, provisionedJob{
			Kind: string(spec.Kind), Name: spec.Name, Schedule: spec.Schedule, JobID: jobID, Created: created,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"provisioned": out})
}

// cronProvision POST /cron/provision —— 为某辅导实例注册默认自动化任务（PRD §3.6.2
// "随模板创建默认注册"）。建档流程 / 前端调用一次即可。需 composition root 注入 CronRegistrar。
func (h *handler) cronProvision(w http.ResponseWriter, r *http.Request) {
	if h.rt.Cron == nil {
		writeErr(w, http.StatusNotImplemented, "cron registrar 未注入")
		return
	}
	var req provisionReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	base := h.rt.BaseURL
	if base == "" {
		base = req.BaseURL
	}
	if base == "" {
		writeErr(w, http.StatusBadRequest, "base_url required（Runtime.BaseURL 未配）")
		return
	}
	const desktopPrincipal = "desktop-user"
	claimedUserID := strings.TrimSpace(req.UserID)
	if claimedUserID != "" && claimedUserID != desktopPrincipal {
		writeErr(w, http.StatusBadRequest, "user_id must match desktop principal")
		return
	}
	userID := desktopPrincipal
	// 任务集与 kind 都以描述符（DefaultCronSpecs，对齐 §3.13）为单一事实源——
	// 原平行 kinds 数组随 monthly-report/year-archive 描述符撤下而删除，防 kind/spec 错位。
	specs := usecase.DefaultCronSpecs(base, req.Agent, req.Deliver)
	jobIDs, reclaimed, err := h.rt.Cron.ProvisionDefaults(r.Context(), specs, req.Platform, req.ChatID, userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "回收历史任务失败（本次注册已生效，可重试 provision）: "+err.Error())
		return
	}
	if len(jobIDs) != len(specs) {
		writeErr(w, http.StatusInternalServerError, "cron registrar 返回的默认任务数不完整")
		return
	}
	out := make([]provisionedJob, 0, len(specs))
	for i, s := range specs {
		out = append(out, provisionedJob{Kind: string(s.Kind), Name: s.Name, Schedule: s.Schedule, JobID: jobIDs[i]})
	}
	if reclaimed == nil {
		reclaimed = []ReclaimedCronJob{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"provisioned": out, "reclaimed": reclaimed})
}

// cronYearArchive GET /cron/year-archive?agent=X —— 学年 6 月底归档建议。
// 无记录 → 空 body（跳过）。
func (h *handler) cronYearArchive(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	text, skip, err := h.rt.Deps.YearArchiveSuggestion(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if skip {
		writeText(w, "")
		return
	}
	writeText(w, text)
}

// --- helpers ---

// writeText 返回纯文本投递体（UTF-8）。空串 = 本期无内容，Starlark 侧据此跳过。
func writeText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s))
}

func toMistakeDTOs(recs []*records.AgentRecord) []mistakeDTO {
	out := make([]mistakeDTO, 0, len(recs))
	for _, r := range recs {
		f, _ := k12.ParseMistakeFields(r.Fields)
		out = append(out, mistakeDTOFrom(r, f))
	}
	return out
}

func mistakeDTOFrom(r *records.AgentRecord, f k12.MistakeFields) mistakeDTO {
	dto := mistakeDTO{
		RecordID: r.RecordID, Question: f.Question, KnowledgePoint: f.KnowledgePoint,
		ErrorCause: f.ErrorCause, Status: r.Status, Version: r.Version, DueAt: r.DueAt,
		Subject: f.Subject, SpotCheckState: f.SpotCheckState,
		ParentConfirmedAt: f.ParentConfirmedAt,
		Restorable:        k12.MistakeRestorable(r.Status, f),
	}
	if r.Status == k12.StatusArchived {
		dto.ArchivedReason = f.ArchivedReason
		dto.ArchivedAt = f.ArchivedAt
	}
	if f.LastArchive != nil {
		dto.RestoredAt = f.LastArchive.RestoredAt
	}
	return dto
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimit(w, r, v, 1<<20)
}

func parseDeleteCommand(
	w http.ResponseWriter,
	r *http.Request,
) (expectedVersion int, commandKey string, ok bool) {
	rawVersion := strings.TrimSpace(r.Header.Get("If-Match"))
	commandKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if strings.HasPrefix(rawVersion, "W/") {
		rawVersion = strings.TrimSpace(strings.TrimPrefix(rawVersion, "W/"))
	}
	rawVersion = strings.Trim(rawVersion, `"`)
	expectedVersion, err := strconv.Atoi(rawVersion)
	if err != nil || expectedVersion <= 0 || commandKey == "" {
		writeErr(w, http.StatusBadRequest, "valid If-Match and Idempotency-Key required")
		return 0, "", false
	}
	return expectedVersion, commandKey, true
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimitMode(w, r, v, 1<<20, true)
}

func decodeLimit(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	return decodeLimitMode(w, r, v, maxBytes, false)
}

func decodeLimitMode(w http.ResponseWriter, r *http.Request, v any, maxBytes int64, strict bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeErr(w, http.StatusBadRequest, "request body must contain exactly one JSON value")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// httpStatusForK12Error 把 usecase/records 的语义错误映射到 HTTP 状态码（BUG-2）。
//
// 分流原则：客户端请求错(400) / 记录不存在(404) / 乐观并发或版本不兼容冲突(409) /
// 下游服务执行失败(502)。未识别的错误落到 fallback（由各 handler 按其主错型给定：
// 存储型 handler 用 500、校验型 handler 用 400）。
func httpStatusForK12Error(err error, fallback int) int {
	switch {
	case errors.Is(err, records.ErrVersionConflict), errors.Is(err, usecase.ErrVersionUnsupported),
		errors.Is(err, records.ErrIllegalTransition),
		errors.Is(err, k12storage.ErrImageTaskVersionConflict),
		errors.Is(err, k12storage.ErrImageTaskConflict),
		errors.Is(err, k12storage.ErrImageTaskInvalidState),
		errors.Is(err, k12storage.ErrCurrentCommandConflict),
		errors.Is(err, k12storage.ErrPracticeGenerationOutputConflict),
		errors.Is(err, usecase.ErrModelInvocationRequiresReconciliation):
		return http.StatusConflict
	case errors.Is(err, records.ErrNotFound), errors.Is(err, k12storage.ErrImageTaskNotFound):
		return http.StatusNotFound
	case errors.Is(err, records.ErrInvalidStatus), errors.Is(err, records.ErrInvalidFields),
		errors.Is(err, records.ErrInvalidRecord), errors.Is(err, records.ErrUnknownCollection),
		errors.Is(err, records.ErrScopeNotFound), errors.Is(err, usecase.ErrChecksumMismatch),
		errors.Is(err, usecase.ErrHexbakAssetManifest),
		errors.Is(err, usecase.ErrGuardianConfirmationRequired),
		errors.Is(err, usecase.ErrArchiveScopeMismatch), errors.Is(err, usecase.ErrRestoreAsArchiveVersion):
		return http.StatusBadRequest
	case errors.Is(err, usecase.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, usecase.ErrSolveFailed), errors.Is(err, usecase.ErrRenderUnavailable):
		return http.StatusBadGateway
	case errors.Is(err, usecase.ErrCurriculumCatalogUnavailable):
		return http.StatusBadGateway
	default:
		return fallback
	}
}
