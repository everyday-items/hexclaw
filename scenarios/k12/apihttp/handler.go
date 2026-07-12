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
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// CronRegistrar 是自动化沉淀「调度」缝：K12 产出声明式 CronSpec，由 composition root
// 包平台 cron.Scheduler 实现本接口（AP-1：K12 不 import cron）。nil 时 provision 返回 501。
type CronRegistrar interface {
	// Register 注册一个默认任务（幂等键 = agent+kind，重复注册应覆盖或跳过由实现决定）。
	Register(ctx context.Context, kind string, spec usecase.CronSpec, platform, chatID, userID string) (jobID string, err error)
}

// IMBinder 是 IM 入站路由「绑定」缝：把某 IM 群（platform+chat_id）绑到某辅导实例，
// 之后该群的作业消息由平台路由（agent_rules）投给这个 Agent（PRD §3.1.7 "各绑各的群"）。
// composition root 包平台 router.Dispatcher + store 实现（AP-1：K12 不 import router）。
type IMBinder interface {
	Bind(ctx context.Context, platform, instanceID, chatID, agentName string) error
}

// Runtime 是本 handler 依赖的 K12 运行时（assembly.K12 满足它；用接口避免 import 环）。
type Runtime struct {
	Views   *scenario.ViewExtensionRegistry
	Records *records.Store
	Deps    usecase.Deps
	// Cron 可选：注入后 POST /cron/provision 可为实例注册默认自动化任务。
	Cron CronRegistrar
	// Binder 可选：注入后 POST /bind-im 可把 IM 群绑到辅导实例（入站路由）。
	Binder IMBinder
	// BaseURL 本机 API 基址（如 http://127.0.0.1:8787），生成投递脚本 http_get 目标用。
	// 空时 provision 用请求体里的 base_url。
	BaseURL string
}

// NewHandler 返回 K12 的 HTTP 子路由（Go 1.22+ method+path 路由）。
func NewHandler(rt Runtime) http.Handler {
	mux := http.NewServeMux()
	h := &handler{rt: rt}
	mux.HandleFunc("GET /view-descriptor", h.viewDescriptor)
	mux.HandleFunc("POST /recognize", h.recognize)
	mux.HandleFunc("POST /grade", h.grade)
	mux.HandleFunc("POST /record-mistake", h.recordMistake)
	mux.HandleFunc("POST /solve", h.solve)
	mux.HandleFunc("GET /mistakes", h.mistakes)
	mux.HandleFunc("DELETE /mistakes/{record_id}", h.deleteMistake)
	mux.HandleFunc("GET /review-queue", h.reviewQueue)
	mux.HandleFunc("GET /insight-report", h.insightReport)
	mux.HandleFunc("POST /mark-mastered", h.markMastered)
	mux.HandleFunc("POST /review/retry", h.reviewRetry)
	mux.HandleFunc("POST /prep-card", h.prepCard)
	mux.HandleFunc("POST /grounding", h.addGrounding)
	mux.HandleFunc("POST /accumulation", h.addAccumulation)
	mux.HandleFunc("GET /accumulation", h.listAccumulation)
	mux.HandleFunc("GET /backup", h.backup)
	mux.HandleFunc("POST /restore", h.restore)
	mux.HandleFunc("GET /export", h.export)
	mux.HandleFunc("GET /mistake-sheet", h.mistakeSheet)
	mux.HandleFunc("GET /profile", h.getProfile)
	mux.HandleFunc("PUT /profile", h.updateProfile)
	mux.HandleFunc("POST /cold-start", h.coldStart)
	mux.HandleFunc("GET /study-time", h.studyTime)
	mux.HandleFunc("POST /tutor-turn", h.tutorTurn)
	// 自动化沉淀投递端点（PRD §3.6）：返回**纯文本**投递内容，空 body = 本期无内容（静默跳过）。
	// 供平台 cron 的 Starlark 脚本 http_get 抓取后 emit → Deliverer 投递到 IM 群/桌面。
	mux.HandleFunc("GET /cron/mistake-sheet", h.cronMistakeSheet)
	mux.HandleFunc("GET /cron/daily-reminder", h.cronDailyReminder)
	mux.HandleFunc("GET /cron/monthly-report", h.cronMonthlyReport)
	mux.HandleFunc("GET /cron/semester-check", h.cronSemesterCheck)
	mux.HandleFunc("GET /cron/year-archive", h.cronYearArchive)
	mux.HandleFunc("POST /cron/provision", h.cronProvision)
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
	Solution      string `json:"solution"`
	Verdict       string `json:"verdict"`
	EvidenceType  string `json:"evidence_type"`
	Badge         string `json:"badge"`
	Correct       bool   `json:"correct"`
	WrongStep     string `json:"wrong_step,omitempty"`
	ErrorCause    string `json:"error_cause,omitempty"`
	OutOfScope    bool   `json:"out_of_scope"`
	OutOfScopeKP  string `json:"out_of_scope_kp,omitempty"`
	RecordCreated bool   `json:"record_created"`
	RecordID      string `json:"record_id,omitempty"`
	// SolveOnly=true 表示本次 student_answer 为空，内部转「解题」分叉（非批改）：
	// 只返回 solution，correct/record 无意义。前端应按解题口径呈现，不显示对/错。
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

type recognizeReq struct {
	ImageBase64 string `json:"image_base64"`
}

// bboxDTO 学生作答区域归一化边界框（0~1），随识题一次返回供前端原图叠加批改标记。
type bboxDTO struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type recognizedQuestionDTO struct {
	Question        string   `json:"question"`
	KnowledgePoints []string `json:"knowledge_points"`
	// StudentAnswer 识题回收的孩子手写作答（未作答=空串）。前端据此区分空白题(走 /solve 求解)
	// 与已答题(走 /grade 批改)，家长可在回显门修改。
	StudentAnswer string `json:"student_answer"`
	// Subject 识题自动判定的题目学科（数学/语文/英语/物理/化学，判不出=空）。
	Subject string `json:"subject,omitempty"`
	// BBox 学生作答区域归一化边界框（0~1）；null=未定位（前端降级为纯文字批改，不叠加，绝不错位）。
	// 用 omitempty + 指针：缺失时字段为 null，前端据 null 走降级路径。
	BBox *bboxDTO `json:"bbox,omitempty"`
}

// recognize POST /recognize —— 作业图片（base64）→ 结构化题目清单。
func (h *handler) recognize(w http.ResponseWriter, r *http.Request) {
	var req recognizeReq
	if !decodeLimit(w, r, &req, 8<<20) {
		return
	}
	img, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stripDataURI(req.ImageBase64)))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid base64 image")
		return
	}
	qs, err := h.rt.Deps.RecognizeHomework(r.Context(), img)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]recognizedQuestionDTO, 0, len(qs))
	for _, q := range qs {
		dto := recognizedQuestionDTO{Question: q.Question, KnowledgePoints: q.KnowledgePoints, StudentAnswer: q.StudentAnswer, Subject: q.Subject}
		if q.BBox != nil { // 仅识题回收到合法框时下发；nil → 字段 null，前端降级纯文字批改。
			dto.BBox = &bboxDTO{X: q.BBox.X, Y: q.BBox.Y, W: q.BBox.W, H: q.BBox.H}
		}
		out = append(out, dto)
	}
	// 顶层整卷学科：逐题判定后取多数（家长不必手选，前端据此预填学科下拉，仍可手动覆盖）。
	writeJSON(w, http.StatusOK, map[string]any{"questions": out, "subject": dominantSubject(qs)})
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
// 取生效年级，供 solve/验算链携带年级边界，避免超纲解法。grade 不信客户端裸传——tutor-turn /
// grade / prep-card 三个入口同一纪律（此前仅 tutor-turn/复习端点做了，/grade 与 /prep-card 裸传，
// hex-test 审计发现的一致性缺口）。agent 空或档案缺失时回退原值（不阻断）。
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
	writeJSON(w, http.StatusOK, gradeResp{
		Solution: res.Solution, Verdict: string(res.Evidence.Verdict),
		EvidenceType: string(res.Evidence.EvidenceType), Badge: res.Evidence.Badge(),
		Correct: res.Outcome.Correct, WrongStep: res.Outcome.WrongStep, ErrorCause: res.Outcome.ErrorCause,
		OutOfScope: res.OutOfScope, OutOfScopeKP: res.OutOfScopeKP,
		RecordCreated: res.RecordCreated, RecordID: res.RecordID,
		SolveOnly: res.SolveOnly,
	})
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
	writeJSON(w, http.StatusOK, rep)
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

type reviewRetryReq struct {
	Agent    string `json:"agent"`
	RecordID string `json:"record_id"`
	Grade    string `json:"grade"`
}

type reviewRetryResp struct {
	Solution string `json:"solution"`
	Verdict  string `json:"verdict"`
	Badge    string `json:"badge"`
}

// reviewRetry POST /review/retry —— 「再练一道」：按错题出同知识点相似题（过 solve 验算链）。
func (h *handler) reviewRetry(w http.ResponseWriter, r *http.Request) {
	var req reviewRetryReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.RecordID == "" {
		writeErr(w, http.StatusBadRequest, "agent / record_id 必填")
		return
	}
	res, err := h.rt.Deps.GenerateRetryByRecord(r.Context(), req.Agent, req.RecordID, req.Grade)
	if err != nil {
		// BUG-2：错题不存在 404 / 下游解题失败 502（对齐 recognize）/ 其余请求校验 400。
		writeErr(w, httpStatusForK12Error(err, http.StatusBadRequest), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reviewRetryResp{
		Solution: res.Solution, Verdict: string(res.Evidence.Verdict), Badge: res.Evidence.Badge(),
	})
}

type prepCardReq struct {
	Agent           string   `json:"agent"`
	Grade           string   `json:"grade"`
	KnowledgePoints []string `json:"knowledge_points"`
}

type groundingReq struct {
	Agent   string `json:"agent"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// addGrounding POST /grounding —— 家长教材按 agent scope 入库，与备课卡读侧同键。
func (h *handler) addGrounding(w http.ResponseWriter, r *http.Request) {
	var req groundingReq
	if !decode(w, r, &req) {
		return
	}
	if err := h.rt.Deps.AddGrounding(r.Context(), req.Agent, req.Title, req.Content); err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type prepSectionDTO struct {
	Title       string `json:"title"`
	Content     string `json:"content"`
	SourceLabel string `json:"source_label"`
}

// prepCard POST /prep-card —— 生成一页备课卡（只读）。
func (h *handler) prepCard(w http.ResponseWriter, r *http.Request) {
	var req prepCardReq
	if !decode(w, r, &req) {
		return
	}
	card, err := h.rt.Deps.BuildPrepCard(r.Context(), req.Agent, h.resolveGrade(r.Context(), req.Agent, req.Grade), req.KnowledgePoints)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	secs := make([]prepSectionDTO, 0, len(card.Sections))
	for _, s := range card.Sections {
		secs = append(secs, prepSectionDTO{Title: s.Title, Content: s.Content, SourceLabel: s.SourceLabel})
	}
	writeJSON(w, http.StatusOK, map[string]any{"knowledge_points": card.KnowledgePoints, "sections": secs})
}

type accumReq struct {
	Agent         string `json:"agent"`
	SourceSession string `json:"source_session"`
	Subject       string `json:"subject"`
	EntryType     string `json:"entry_type"`
	Content       string `json:"content"`
	Source        string `json:"source"`
}

type accumDTO struct {
	RecordID  string `json:"record_id"`
	Subject   string `json:"subject"`
	EntryType string `json:"entry_type"`
	Content   string `json:"content"`
	Status    string `json:"status"`
}

// addAccumulation POST /accumulation —— 语文/英语积累本写入。
func (h *handler) addAccumulation(w http.ResponseWriter, r *http.Request) {
	var req accumReq
	if !decode(w, r, &req) {
		return
	}
	id, created, err := h.rt.Deps.AddAccumulation(r.Context(), req.Agent, req.SourceSession, k12.AccumFields{
		Subject: req.Subject, EntryType: req.EntryType, Content: req.Content, Source: req.Source,
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"record_id": id, "created": created})
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
		out = append(out, accumDTO{
			RecordID: it.Record.RecordID, Subject: it.Fields.Subject, EntryType: it.Fields.EntryType,
			Content: it.Fields.Content, Status: it.Record.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
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

// restore POST /restore —— 从 .hexbak 恢复（先校验 checksum）。
func (h *handler) restore(w http.ResponseWriter, r *http.Request) {
	var bak usecase.Hexbak
	if !decodeLimit(w, r, &bak, 16<<20) {
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
	w.Header().Set("Content-Disposition", "attachment; filename=\"mistakes."+format+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

type profileDTO struct {
	ChildName       string `json:"child_name"`
	GradeTerm       string `json:"grade_term"`
	TextbookEdition string `json:"textbook_edition"`
}

// getProfile GET /profile?agent=X —— 读孩子档案。
func (h *handler) getProfile(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	p, err := h.rt.Deps.GetProfile(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, profileDTO{ChildName: p.ChildName, GradeTerm: p.GradeTerm, TextbookEdition: p.TextbookEdition})
}

type coldStartReq struct {
	Agent           string   `json:"agent"`
	ChildName       string   `json:"child_name"`
	KnowledgePoints []string `json:"knowledge_points"`
	FallbackGrade   string   `json:"fallback_grade"` // 推断不出时用（可空 → 降级不注入约束）
	Textbook        string   `json:"textbook_edition"`
}

type coldStartResp struct {
	ChildName       string `json:"child_name"`
	GradeTerm       string `json:"grade_term"`
	TextbookEdition string `json:"textbook_edition"`
	Inferred        bool   `json:"inferred"` // 年级是否由知识点倒查推断
	Created         bool   `json:"created"`  // 是否新建档案（false=已有档案）
}

// coldStart POST /cold-start —— 未建档实例首拍作业：按知识点倒查推断年级 → 建档（PRD §3.1.4-4）。
// 已有档案则不覆盖直接返回；前端应先向家长确认推断年级再调此端点落库。
func (h *handler) coldStart(w http.ResponseWriter, r *http.Request) {
	var req coldStartReq
	if !decode(w, r, &req) {
		return
	}
	res, err := h.rt.Deps.ColdStartProvision(r.Context(), req.Agent, req.ChildName, req.KnowledgePoints, req.FallbackGrade, req.Textbook)
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
	writeJSON(w, http.StatusOK, profileDTO{ChildName: p.ChildName, GradeTerm: p.GradeTerm, TextbookEdition: p.TextbookEdition})
}

// studyTime GET /study-time?agent=X —— 学习时长（日粒度近似）。
func (h *handler) studyTime(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	st, err := h.rt.Deps.StudyTime(r.Context(), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
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
	Agent      string `json:"agent"`       // 目标辅导实例名
	Platform   string `json:"platform"`    // IM 平台（dingtalk/feishu/…）
	InstanceID string `json:"instance_id"` // 平台实例标识（可空）
	ChatID     string `json:"chat_id"`     // 家庭群/会话 ID
}

// bindIM POST /bind-im —— 把 IM 群绑到辅导实例（PRD §3.1.7 "各绑各的群" / §3.2.3 通道路由）。
// 绑定后该群的入站作业消息由平台 agent_rules 路由到这个 Agent。需注入 IMBinder。
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
	if err := h.rt.Binder.Bind(r.Context(), req.Platform, req.InstanceID, req.ChatID, req.Agent); err != nil {
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
	userID := req.UserID
	if userID == "" {
		userID = "k12"
	}
	specs := usecase.DefaultCronSpecs(base, req.Agent, req.Deliver)
	kinds := []usecase.K12CronKind{
		usecase.KindWeeklySheet, usecase.KindDailyReminder, usecase.KindMonthlyReport,
		usecase.KindYearEndArchive, usecase.KindSemesterSpring, usecase.KindSemesterFall,
	}
	out := make([]provisionedJob, 0, len(specs))
	for i, s := range specs {
		jobID, err := h.rt.Cron.Register(r.Context(), string(kinds[i]), s, req.Platform, req.ChatID, userID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "注册 "+s.Name+" 失败: "+err.Error())
			return
		}
		out = append(out, provisionedJob{Kind: string(kinds[i]), Name: s.Name, Schedule: s.Schedule, JobID: jobID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"provisioned": out})
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
	return mistakeDTO{
		RecordID: r.RecordID, Question: f.Question, KnowledgePoint: f.KnowledgePoint,
		ErrorCause: f.ErrorCause, Status: r.Status, Version: r.Version, DueAt: r.DueAt,
		Subject: f.Subject,
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimit(w, r, v, 1<<20)
}

func decodeLimit(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
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
		errors.Is(err, records.ErrIllegalTransition):
		return http.StatusConflict
	case errors.Is(err, records.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, records.ErrInvalidStatus), errors.Is(err, records.ErrInvalidFields),
		errors.Is(err, records.ErrInvalidRecord), errors.Is(err, records.ErrUnknownCollection),
		errors.Is(err, records.ErrScopeNotFound), errors.Is(err, usecase.ErrChecksumMismatch):
		return http.StatusBadRequest
	case errors.Is(err, usecase.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, usecase.ErrSolveFailed), errors.Is(err, usecase.ErrRenderUnavailable):
		return http.StatusBadGateway
	default:
		return fallback
	}
}
