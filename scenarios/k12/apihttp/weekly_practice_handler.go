package apihttp

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func (h *handler) getCurriculumCatalog(w http.ResponseWriter, r *http.Request) {
	catalog, err := h.rt.Deps.GetWeeklyCurriculumCatalog(r.Context(),
		usecase.WeeklyCurriculumCatalogRequest{
			AgentName: r.URL.Query().Get("agent"), Subject: r.URL.Query().Get("subject"),
			TextbookEdition: r.URL.Query().Get("textbook_edition"),
			Volume: r.URL.Query().Get("volume"),
		})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, catalog)
}

func (h *handler) getCurriculumProgress(w http.ResponseWriter, r *http.Request) {
	progress, err := h.rt.Deps.GetCurriculumProgress(
		r.Context(), r.URL.Query().Get("agent"), r.URL.Query().Get("subject"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"progress": progress})
}

func (h *handler) getWeeklyPracticeSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.rt.Deps.GetWeeklyPracticeSettings(r.Context(), r.URL.Query().Get("agent"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

type profileBundleRequest struct {
	AgentName                  string `json:"agent"`
	IdempotencyKey             string `json:"idempotency_key"`
	ExpectedProfileRevision    int    `json:"expected_profile_revision"`
	ExpectedProgressRevision   int    `json:"expected_progress_revision"`
	ExpectedSettingsRevision   int    `json:"expected_settings_revision"`
	Profile struct {
		ChildName        string               `json:"child_name"`
		GradeTerm        string               `json:"grade_term"`
		SubjectTextbooks k12.SubjectTextbooks `json:"subject_textbooks"`
	} `json:"profile"`
	CurriculumProgress struct {
		Subject           string `json:"subject"`
		TextbookBindingID string `json:"textbook_binding_id"`
		Volume            string `json:"volume"`
		UnitID            string `json:"unit_id"`
		LessonID          string `json:"lesson_id,omitempty"`
		PageFrom          *int   `json:"page_from,omitempty"`
		PageTo            *int   `json:"page_to,omitempty"`
		EvidenceSource    string `json:"evidence_source"`
	} `json:"curriculum_progress"`
	WeeklyPracticeSettings struct {
		Timezone                     string `json:"timezone"`
		TextbookConsolidationEnabled bool   `json:"textbook_consolidation_enabled"`
		ArithmeticWarmupEnabled      bool   `json:"arithmetic_warmup_enabled"`
		ArithmeticMinutes            int    `json:"arithmetic_minutes"`
	} `json:"weekly_practice_settings"`
}

func (h *handler) updateProfileBundle(w http.ResponseWriter, r *http.Request) {
	var req profileBundleRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	result, err := h.rt.Deps.UpdateProfileBundle(r.Context(), usecase.UpdateProfileBundleRequest{
		AgentName: req.AgentName, IdempotencyKey: req.IdempotencyKey,
		ExpectedProfileRevision: req.ExpectedProfileRevision,
		ExpectedProgressRevision: req.ExpectedProgressRevision,
		ExpectedSettingsRevision: req.ExpectedSettingsRevision,
		Profile: k12.ChildProfile{
			ChildName: req.Profile.ChildName, GradeTerm: req.Profile.GradeTerm,
			SubjectTextbooks: req.Profile.SubjectTextbooks,
		},
		CurriculumProgress: usecase.CurriculumProgressInput{
			Subject: req.CurriculumProgress.Subject,
			TextbookBindingID: req.CurriculumProgress.TextbookBindingID,
			Volume: req.CurriculumProgress.Volume, UnitID: req.CurriculumProgress.UnitID,
			LessonID: req.CurriculumProgress.LessonID, PageFrom: req.CurriculumProgress.PageFrom,
			PageTo: req.CurriculumProgress.PageTo,
			EvidenceSource: req.CurriculumProgress.EvidenceSource,
		},
		WeeklyPracticeSettings: usecase.WeeklyPracticeSettingsInput{
			Timezone: req.WeeklyPracticeSettings.Timezone,
			TextbookConsolidationEnabled: req.WeeklyPracticeSettings.TextbookConsolidationEnabled,
			ArithmeticWarmupEnabled: req.WeeklyPracticeSettings.ArithmeticWarmupEnabled,
			ArithmeticMinutes: req.WeeklyPracticeSettings.ArithmeticMinutes,
		},
	})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type weeklyPlanCommandRequest struct {
	AgentName      string `json:"agent"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (h *handler) ensureWeeklyPracticePlan(w http.ResponseWriter, r *http.Request) {
	var req weeklyPlanCommandRequest
	if !decode(w, r, &req) {
		return
	}
	plan, replay, err := h.rt.Deps.EnsureWeeklyPracticePlan(r.Context(),
		usecase.EnsureWeeklyPracticePlanRequest{
			AgentName: req.AgentName, IdempotencyKey: req.IdempotencyKey,
		})
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"plan": plan, "replayed": replay})
}

func (h *handler) getCurrentWeeklyPracticePlan(w http.ResponseWriter, r *http.Request) {
	plan, err := h.rt.Deps.GetCurrentWeeklyPracticePlan(r.Context(), r.URL.Query().Get("agent"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": plan})
}

func (h *handler) listWeeklyPracticeHistory(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeErr(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	items, next, err := h.rt.Deps.ListWeeklyPracticeHistory(
		r.Context(), r.URL.Query().Get("agent"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (h *handler) getWeeklyPracticeSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot, err := h.rt.Deps.GetWeeklyPracticeSnapshot(
		r.Context(), r.URL.Query().Get("agent"), r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusNotFound), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

type weeklyExpectedRevisionRequest struct {
	AgentName       string `json:"agent"`
	ExpectedRevision int   `json:"expected_revision"`
	IdempotencyKey  string `json:"idempotency_key"`
}

func (h *handler) prepareWeeklyPracticeOutput(w http.ResponseWriter, r *http.Request) {
	var req weeklyExpectedRevisionRequest
	if !decode(w, r, &req) {
		return
	}
	result, err := h.rt.Deps.PrepareWeeklyPracticeOutput(
		r.Context(), req.AgentName, r.PathValue("id"), req.ExpectedRevision, req.IdempotencyKey)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"snapshot": result.Snapshot, "artifact": printableArtifactDTO(result.Artifact),
	})
}

type weeklySendRequest struct {
	AgentName      string `json:"agent"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (h *handler) sendWeeklyPracticeSnapshot(w http.ResponseWriter, r *http.Request) {
	var req weeklySendRequest
	if !decode(w, r, &req) {
		return
	}
	batch, err := h.rt.Deps.SendWeeklyPracticeSnapshot(
		r.Context(), req.AgentName, r.PathValue("id"), req.IdempotencyKey)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

type weeklyAttemptRequest struct {
	AgentName      string `json:"agent"`
	ItemID         string `json:"item_id"`
	StudentAnswer  string `json:"student_answer"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (h *handler) submitWeeklyPracticeAttempt(w http.ResponseWriter, r *http.Request) {
	var req weeklyAttemptRequest
	if !decode(w, r, &req) {
		return
	}
	attempt, replay, err := h.rt.Deps.SubmitWeeklyPracticeAttempt(
		r.Context(), req.AgentName, r.PathValue("id"), req.ItemID,
		req.StudentAnswer, req.IdempotencyKey)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusInternalServerError), err.Error())
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"attempt": attempt, "replayed": replay})
}

func (h *handler) saveWeeklyPracticeToPracticeSet(w http.ResponseWriter, r *http.Request) {
	var req weeklyExpectedRevisionRequest
	if !decode(w, r, &req) {
		return
	}
	receipt, replay, err := h.rt.Deps.SaveWeeklyPracticeToPracticeSet(
		r.Context(), req.AgentName, r.PathValue("id"), req.ExpectedRevision, req.IdempotencyKey)
	if err != nil {
		writeErr(w, httpStatusForK12Error(err, http.StatusConflict), err.Error())
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"receipt": receipt, "replayed": replay})
}
