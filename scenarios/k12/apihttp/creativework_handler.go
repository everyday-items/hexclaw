package apihttp

import (
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 作品 DTO（前端契约，PRD §3.10 / §5.5）。
type workVersionDTO struct {
	VersionID          string            `json:"version_id"`
	AssetID            string            `json:"source_asset_id,omitempty"`
	Content            string            `json:"content_markdown,omitempty"`
	OCRJobID           string            `json:"ocr_job_id,omitempty"`
	OCRRaw             string            `json:"ocr_raw,omitempty"`
	OCRVersion         int               `json:"ocr_version,omitempty"`
	OCRConfirmedDigest string            `json:"ocr_confirmed_digest,omitempty"`
	ContentConfirmedAt int64             `json:"content_confirmed_at,omitempty"`
	Feedback           string            `json:"feedback,omitempty"`
	StructuredFeedback *k12.WorkFeedback `json:"structured_feedback,omitempty"`
	// FeedbackSource 点评来源（ai / parent）；老数据空值前向兼容。
	FeedbackSource string `json:"feedback_source,omitempty"`
	// FeedbackSkill AI 点评所用方法论基座来源戳（如 "writing-feedback@1.0.0/disk"，
	// 追溯用）；家长手写/老数据空值前向兼容。
	FeedbackSkill string `json:"feedback_skill,omitempty"`
	// PracticeCard 观察小练习卡文本（§3.10 美术侧，服务端由点评正文确定性提炼——单一事实源，
	// 前端只渲染不再各自解析）；写作/无点评为空。
	PracticeCard string `json:"practice_card,omitempty"`
	// PracticeCardDoneAt 练习卡完成打卡时间（unix 秒；0/缺失 = 未打卡）。
	PracticeCardDoneAt int64 `json:"practice_card_done_at,omitempty"`
}

type creativeWorkDTO struct {
	RecordID    string           `json:"record_id"`
	WorkType    string           `json:"work_type"`
	Title       string           `json:"title"`
	Task        string           `json:"task"`
	Intent      string           `json:"intent,omitempty"`
	Status      string           `json:"status"`
	StatusLabel string           `json:"status_label"`
	Versions    []workVersionDTO `json:"versions"`
}

func toCreativeWorkDTO(v usecase.CreativeWorkView) creativeWorkDTO {
	vers := make([]workVersionDTO, 0, len(v.Fields.Versions))
	for _, ver := range v.Fields.Versions {
		dto := workVersionDTO{
			VersionID: ver.VersionID, AssetID: ver.SourceAssetID,
			Content: ver.ContentMarkdown, Feedback: ver.Feedback, StructuredFeedback: ver.StructuredFeedback,
			OCRJobID: ver.OCRJobID, OCRRaw: ver.OCRRaw, OCRVersion: ver.OCRVersion,
			OCRConfirmedDigest: ver.OCRConfirmedDigest, ContentConfirmedAt: ver.ContentConfirmedAt,
			FeedbackSource: ver.FeedbackSource, FeedbackSkill: ver.FeedbackSkill,
			PracticeCardDoneAt: ver.PracticeCardDoneAt,
		}
		// 观察练习卡（§3.10 美术）：由点评正文服务端提炼，随版本下发。
		if v.Fields.WorkType == k12.WorkTypeArt {
			dto.PracticeCard = k12.ObservationPracticeCardFromStructured(ver.StructuredFeedback, ver.Feedback)
		}
		vers = append(vers, dto)
	}
	return creativeWorkDTO{
		RecordID: v.Record.RecordID, WorkType: v.Fields.WorkType, Title: v.Fields.Title,
		Task: v.Fields.Task, Intent: v.Fields.Intent,
		Status: v.Record.Status, StatusLabel: k12.CreativeWorkLabel(v.Record.Status),
		Versions: vers,
	}
}

type createWorkReq struct {
	Agent         string `json:"agent"`
	SourceSession string `json:"source_session"`
	WorkType      string `json:"work_type"`
	Title         string `json:"title"`
	Task          string `json:"task"`
	Intent        string `json:"intent"`
	Content       string `json:"content_markdown"`
	AssetID       string `json:"source_asset_id"`
	OCRJobID      string `json:"ocr_job_id"`
	OCRVersion    int    `json:"ocr_version"`
	OCRDigest     string `json:"ocr_confirmed_digest"`
}

func (h *handler) createCreativeWork(w http.ResponseWriter, r *http.Request) {
	var req createWorkReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	id, created, err := h.rt.Deps.CreateCreativeWork(r.Context(), req.Agent, req.SourceSession, k12.CreativeWorkFields{
		WorkType: req.WorkType, Title: req.Title, Task: req.Task, Intent: req.Intent,
		Versions: []k12.CreativeWorkVersion{{
			ContentMarkdown: req.Content, SourceAssetID: req.AssetID,
			OCRJobID: req.OCRJobID, OCRVersion: req.OCRVersion, OCRConfirmedDigest: req.OCRDigest,
		}},
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"record_id": id, "created": created})
}

func (h *handler) listCreativeWorks(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	items, err := h.rt.Deps.ListCreativeWorks(r.Context(), agent, r.URL.Query().Get("type"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]creativeWorkDTO, 0, len(items))
	for _, it := range items {
		out = append(out, toCreativeWorkDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getCreativeWork(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, err := h.rt.Deps.GetCreativeWork(r.Context(), agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toCreativeWorkDTO(v))
}

type feedbackReq struct {
	Agent    string `json:"agent"`
	Feedback string `json:"feedback"`
}

func (h *handler) attachWorkFeedback(w http.ResponseWriter, r *http.Request) {
	var req feedbackReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" || req.Feedback == "" {
		writeErr(w, http.StatusBadRequest, "agent / feedback 必填")
		return
	}
	v, err := h.rt.Deps.AttachFeedback(r.Context(), req.Agent, r.PathValue("id"), req.Feedback)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toCreativeWorkDTO(v))
}

// generateWorkFeedback POST /creative-works/{id}/generate-feedback —— 调 Skill Executor
// 为 draft/revised 作品生成证据化点评（写作：好句摘出+一处具体建议；美术：观察描述式），
// 来源标记 ai（PRD §3.10 / INV-011：只点评不打分不代写，违规输出拒绝入库 → 502）。
func (h *handler) generateWorkFeedback(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, err := h.rt.Deps.GenerateWorkFeedback(r.Context(), req.Agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toCreativeWorkDTO(v))
}

type revisionReq struct {
	Agent      string `json:"agent"`
	Content    string `json:"content_markdown"`
	AssetID    string `json:"source_asset_id"`
	OCRJobID   string `json:"ocr_job_id"`
	OCRVersion int    `json:"ocr_version"`
	OCRDigest  string `json:"ocr_confirmed_digest"`
}

func (h *handler) submitWorkRevision(w http.ResponseWriter, r *http.Request) {
	var req revisionReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	var v usecase.CreativeWorkView
	var err error
	if req.OCRJobID != "" || req.OCRVersion != 0 || req.OCRDigest != "" {
		v, err = h.rt.Deps.SubmitRevisionWithOCR(r.Context(), req.Agent, r.PathValue("id"), k12.CreativeWorkVersion{
			ContentMarkdown: req.Content, SourceAssetID: req.AssetID,
			OCRJobID: req.OCRJobID, OCRVersion: req.OCRVersion, OCRConfirmedDigest: req.OCRDigest,
		})
	} else {
		v, err = h.rt.Deps.SubmitRevision(r.Context(), req.Agent, r.PathValue("id"), req.Content, req.AssetID)
	}
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toCreativeWorkDTO(v))
}

// sendFeedbackReq 点评/练习卡发送体：kind = ""|"feedback"（点评要点）| "practice_card"（观察练习卡）。
type sendFeedbackReq struct {
	Agent string `json:"agent"`
	Kind  string `json:"kind"`
}

// sendWorkFeedback POST /creative-works/{id}/send-feedback —— 把点评要点/观察练习卡作为
// 辅导延伸消息发到绑定该实例的私聊（§3.10「点评可发送到手机」/ §3.12 发送到手机）。
// 发送前先落 durable Receipt；平台 HTTP 接受只返回 sending，不能虚标 delivered。
// 文案家长向（§4.11）：只说后果不说机制，无「验证器/机制」词。
func (h *handler) sendWorkFeedback(w http.ResponseWriter, r *http.Request) {
	var req sendFeedbackReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, err := h.rt.Deps.GetCreativeWork(r.Context(), req.Agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	// 取最新一条带点评的版本（修改稿新版本可能尚无点评）。
	feedback := ""
	var structured *k12.WorkFeedback
	for i := len(v.Fields.Versions) - 1; i >= 0; i-- {
		if fb := strings.TrimSpace(v.Fields.Versions[i].Feedback); fb != "" {
			feedback = fb
			structured = v.Fields.Versions[i].StructuredFeedback
			break
		}
	}
	if feedback == "" {
		writeErr(w, http.StatusConflict, "这件作品还没有点评，先写一条或生成点评再发送")
		return
	}
	var content string
	switch req.Kind {
	case "", "feedback":
		content = "《" + v.Fields.Title + "》点评要点\n" + feedback
	case "practice_card":
		if v.Fields.WorkType != k12.WorkTypeArt {
			writeErr(w, http.StatusBadRequest, "观察练习卡只属于美术作品")
			return
		}
		card := k12.ObservationPracticeCardFromStructured(structured, feedback)
		if card == "" {
			writeErr(w, http.StatusConflict, "这件作品还没有观察练习卡")
			return
		}
		content = "《" + v.Fields.Title + "》观察小练习\n" + card
	default:
		writeErr(w, http.StatusBadRequest, "kind 只支持 feedback / practice_card")
		return
	}
	if h.rt.Deps.Delivery != nil {
		kind := "creative_work_feedback"
		if req.Kind == "practice_card" {
			kind = "creative_work_practice_card"
		}
		receipt, _, err := h.rt.Deps.PrepareAndSendText(r.Context(), req.Agent, kind, v.Record.RecordID, content)
		if err != nil {
			writeDeliveryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	writeErr(w, http.StatusNotImplemented, "发送到手机还没有开通，请先在连接设置里完成私聊绑定")
}

// markPracticeCardDone POST /creative-works/{id}/practice-card/done —— 观察练习卡完成打卡
// （§3.10：练习必须有产物；完成记录归档在版本记录，幂等保留首次时间）。
func (h *handler) markPracticeCardDone(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	v, err := h.rt.Deps.MarkPracticeCardDone(r.Context(), req.Agent, r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toCreativeWorkDTO(v))
}

func (h *handler) archiveCreativeWork(w http.ResponseWriter, r *http.Request) {
	var req agentOnlyReq
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	if err := h.rt.Deps.ArchiveCreativeWork(r.Context(), req.Agent, r.PathValue("id")); err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
