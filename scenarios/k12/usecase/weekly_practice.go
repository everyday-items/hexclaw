package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var ErrCurriculumCatalogUnavailable = errors.New("curriculum catalog unavailable")

type WeeklyCurriculumCatalogRequest struct {
	AgentName       string
	Subject         string
	TextbookEdition string
	Volume          string
}

type WeeklyCurriculumCatalogSource interface {
	LookupWeeklyCurriculum(context.Context, WeeklyCurriculumCatalogRequest) (k12.CurriculumCatalog, error)
}

type WeeklyPracticeCandidateRequest struct {
	AgentName       string
	PlanSection     string
	MaxItems        int
	ArithmeticMinutes int
	Progress        k12.CurriculumProgress
}

type WeeklyPracticeCandidate struct {
	SourceKind       string
	GenerationMethod string
	SourceRef        string
	PromptMarkdown   string
	ExpectedAnswer   string
	EvidenceRefs     []string
	EstimatedSeconds int
}

type WeeklyPracticeCandidateSource interface {
	GenerateWeeklyPracticeCandidates(context.Context, WeeklyPracticeCandidateRequest) ([]WeeklyPracticeCandidate, error)
}

type WeeklyPracticeAnswerRequest struct {
	AgentName    string
	SnapshotID  string
	Item        k12.WeeklyPracticeItem
	StudentAnswer string
}

type WeeklyPracticeAnswerAssessment struct {
	AssessmentID        string
	Result              string
	VerificationEvidence string
	Subject             string
	KnowledgePoint      string
}

type WeeklyPracticeAnswerAssessor interface {
	AssessWeeklyPracticeAnswer(context.Context, WeeklyPracticeAnswerRequest) (WeeklyPracticeAnswerAssessment, error)
}

type CurriculumProgressInput struct {
	Subject           string
	TextbookBindingID string
	Volume            string
	UnitID            string
	LessonID          string
	PageFrom          *int
	PageTo            *int
	EvidenceSource    string
}

type WeeklyPracticeSettingsInput struct {
	Timezone                     string
	TextbookConsolidationEnabled bool
	ArithmeticWarmupEnabled      bool
	ArithmeticMinutes            int
}

type UpdateProfileBundleRequest struct {
	AgentName                 string
	IdempotencyKey            string
	ExpectedProfileRevision   int
	ExpectedProgressRevision  int
	ExpectedSettingsRevision  int
	Profile                   k12.ChildProfile
	CurriculumProgress        CurriculumProgressInput
	WeeklyPracticeSettings    WeeklyPracticeSettingsInput
}

type profilePublisher interface {
	PublishProfile(string, k12.ChildProfile) error
}

func (d Deps) GetProfileWithRevision(ctx context.Context, agentName string) (k12.WeeklyProfile, error) {
	agentName = strings.TrimSpace(agentName)
	if d.Profiles != nil {
		p, err := d.Profiles.GetProfile(ctx, agentName)
		if err != nil {
			return k12.WeeklyProfile{}, err
		}
		revision := 0
		if d.Records != nil {
			if state, stateErr := d.Records.GetProfileState(ctx, agentName); stateErr == nil {
				revision = state.Revision
			}
		}
		return k12.WeeklyProfile{
			ChildName: p.ChildName, GradeTerm: p.GradeTerm,
			SubjectTextbooks: p.SubjectTextbooks,
			TextbookEdition: p.TextbookEdition, Revision: revision,
		}, nil
	}
	if d.Records != nil {
		return d.Records.GetProfileState(ctx, agentName)
	}
	return k12.WeeklyProfile{}, fmt.Errorf("%w: profile store unavailable", ErrInvalidInput)
}

func (d Deps) GetWeeklyCurriculumCatalog(ctx context.Context, req WeeklyCurriculumCatalogRequest) (k12.CurriculumCatalog, error) {
	req.AgentName = strings.TrimSpace(req.AgentName)
	req.Subject = strings.TrimSpace(req.Subject)
	req.TextbookEdition = strings.TrimSpace(req.TextbookEdition)
	req.Volume = strings.TrimSpace(req.Volume)
	if req.AgentName == "" || req.Subject != "math" || req.TextbookEdition == "" || req.Volume == "" {
		return k12.CurriculumCatalog{}, fmt.Errorf("%w: agent/subject=math/textbook_edition/volume required", ErrInvalidInput)
	}
	if d.Records == nil {
		return k12.CurriculumCatalog{}, fmt.Errorf("%w: records unavailable", ErrCurriculumCatalogUnavailable)
	}
	if _, err := d.Records.GetProfileState(ctx, req.AgentName); err != nil {
		return k12.CurriculumCatalog{}, err
	}
	if d.WeeklyCurriculum == nil {
		return k12.CurriculumCatalog{}, ErrCurriculumCatalogUnavailable
	}
	catalog, err := d.WeeklyCurriculum.LookupWeeklyCurriculum(ctx, req)
	if err != nil {
		if errors.Is(err, records.ErrNotFound) {
			return k12.CurriculumCatalog{}, records.ErrNotFound
		}
		return k12.CurriculumCatalog{}, fmt.Errorf("%w: %v", ErrCurriculumCatalogUnavailable, err)
	}
	catalog.AgentName = req.AgentName
	if catalog.Subject != req.Subject || catalog.TextbookEdition != req.TextbookEdition ||
		catalog.Volume != req.Volume || strings.TrimSpace(catalog.TextbookBindingID) == "" ||
		strings.TrimSpace(catalog.TextbookVersion) == "" || strings.TrimSpace(catalog.Title) == "" ||
		catalog.PageMin <= 0 || catalog.PageMax < catalog.PageMin || len(catalog.Units) == 0 {
		return k12.CurriculumCatalog{}, fmt.Errorf("%w: invalid authoritative catalog", ErrCurriculumCatalogUnavailable)
	}
	return catalog, nil
}

func (d Deps) GetCurriculumProgress(ctx context.Context, agentName, subject string) (*k12.CurriculumProgress, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(subject) != "math" {
		return nil, fmt.Errorf("%w: agent and subject=math required", ErrInvalidInput)
	}
	progress, err := d.Records.GetCurriculumProgress(ctx, agentName, subject)
	if errors.Is(err, records.ErrNotFound) {
		if _, ownerErr := d.Records.GetProfileState(ctx, agentName); ownerErr != nil {
			return nil, ownerErr
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &progress, nil
}

func (d Deps) GetWeeklyPracticeSettings(ctx context.Context, agentName string) (k12.WeeklyPracticeSettings, error) {
	if strings.TrimSpace(agentName) == "" {
		return k12.WeeklyPracticeSettings{}, fmt.Errorf("%w: agent required", ErrInvalidInput)
	}
	return d.Records.GetWeeklyPracticeSettings(ctx, agentName)
}

func (d Deps) UpdateProfileBundle(ctx context.Context, req UpdateProfileBundleRequest) (k12.ProfileBundleResult, error) {
	req.AgentName = strings.TrimSpace(req.AgentName)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.AgentName == "" || req.IdempotencyKey == "" ||
		req.ExpectedProfileRevision < 0 || req.ExpectedProgressRevision < 0 ||
		req.ExpectedSettingsRevision < 0 {
		return k12.ProfileBundleResult{}, fmt.Errorf("%w: invalid profile bundle command", ErrInvalidInput)
	}
	req.Profile.ChildName = strings.TrimSpace(req.Profile.ChildName)
	req.Profile.GradeTerm = strings.TrimSpace(req.Profile.GradeTerm)
	textbooks, complete := k12.NormalizeSubjectTextbooks(req.Profile.SubjectTextbooks)
	if req.Profile.ChildName == "" || !complete {
		return k12.ProfileBundleResult{},
			fmt.Errorf("%w: complete six-subject profile required", ErrInvalidInput)
	}
	req.Profile.SubjectTextbooks = textbooks
	req.Profile.TextbookEdition = textbooks.Math
	if err := k12.ValidateProfileGradeTerm(req.Profile.GradeTerm); err != nil {
		return k12.ProfileBundleResult{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	settings, err := normalizeWeeklySettings(req.AgentName, req.WeeklyPracticeSettings)
	if err != nil {
		return k12.ProfileBundleResult{}, err
	}
	catalog, err := d.GetWeeklyCurriculumCatalog(ctx, WeeklyCurriculumCatalogRequest{
		AgentName: req.AgentName, Subject: strings.TrimSpace(req.CurriculumProgress.Subject),
		TextbookEdition: req.Profile.TextbookEdition, Volume: req.CurriculumProgress.Volume,
	})
	if err != nil {
		return k12.ProfileBundleResult{}, err
	}
	progress, err := resolveCurriculumProgress(req.AgentName, req.CurriculumProgress, catalog, d.now())
	if err != nil {
		return k12.ProfileBundleResult{}, err
	}
	digest := digestValue(struct {
		AgentName string
		ExpectedProfileRevision int
		ExpectedProgressRevision int
		ExpectedSettingsRevision int
		Profile k12.ChildProfile
		Progress CurriculumProgressInput
		Settings WeeklyPracticeSettingsInput
	}{
		req.AgentName, req.ExpectedProfileRevision, req.ExpectedProgressRevision,
		req.ExpectedSettingsRevision, req.Profile, req.CurriculumProgress,
		req.WeeklyPracticeSettings,
	})
	result, _, err := d.Records.UpdateProfileBundle(ctx, k12storage.ProfileBundleMutation{
		AgentName: req.AgentName, IdempotencyKey: req.IdempotencyKey,
		RequestDigest: digest, ExpectedProfileRevision: req.ExpectedProfileRevision,
		ExpectedProgressRevision: req.ExpectedProgressRevision,
		ExpectedSettingsRevision: req.ExpectedSettingsRevision,
		Profile: req.Profile, Progress: progress, Settings: settings, At: d.now(),
	})
	if err != nil {
		return k12.ProfileBundleResult{}, err
	}
	if publisher, ok := d.Profiles.(profilePublisher); ok {
		if err := publisher.PublishProfile(req.AgentName, req.Profile); err != nil {
			return k12.ProfileBundleResult{}, err
		}
	}
	return result, nil
}

func normalizeWeeklySettings(agent string, in WeeklyPracticeSettingsInput) (k12.WeeklyPracticeSettings, error) {
	in.Timezone = strings.TrimSpace(in.Timezone)
	if in.Timezone == "" || in.ArithmeticMinutes < 1 || in.ArithmeticMinutes > 5 {
		return k12.WeeklyPracticeSettings{}, fmt.Errorf("%w: timezone and arithmetic_minutes 1..5 required", ErrInvalidInput)
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return k12.WeeklyPracticeSettings{}, fmt.Errorf("%w: invalid timezone", ErrInvalidInput)
	}
	return k12.WeeklyPracticeSettings{
		AgentName: agent, Timezone: in.Timezone, DueReviewEnabled: true,
		TextbookConsolidationEnabled: in.TextbookConsolidationEnabled,
		ArithmeticWarmupEnabled: in.ArithmeticWarmupEnabled,
		ArithmeticMinutes: in.ArithmeticMinutes,
	}, nil
}

func resolveCurriculumProgress(agent string, in CurriculumProgressInput,
	catalog k12.CurriculumCatalog, at int64) (k12.CurriculumProgress, error) {
	if strings.TrimSpace(in.EvidenceSource) != "parent_confirmed" ||
		strings.TrimSpace(in.TextbookBindingID) != catalog.TextbookBindingID ||
		strings.TrimSpace(in.Volume) != catalog.Volume || strings.TrimSpace(in.UnitID) == "" {
		return k12.CurriculumProgress{}, fmt.Errorf("%w: unverified curriculum progress", ErrInvalidInput)
	}
	var unit *k12.CurriculumCatalogUnit
	for i := range catalog.Units {
		if catalog.Units[i].UnitID == in.UnitID {
			unit = &catalog.Units[i]
			break
		}
	}
	if unit == nil {
		return k12.CurriculumProgress{}, fmt.Errorf("%w: unit is not in catalog", ErrInvalidInput)
	}
	segmentFrom, segmentTo := unit.PageFrom, unit.PageTo
	lessonTitle := ""
	if strings.TrimSpace(in.LessonID) != "" {
		found := false
		for _, lesson := range unit.Lessons {
			if lesson.LessonID == in.LessonID {
				lessonTitle, segmentFrom, segmentTo, found = lesson.Title, lesson.PageFrom, lesson.PageTo, true
				break
			}
		}
		if !found {
			return k12.CurriculumProgress{}, fmt.Errorf("%w: lesson is not in unit", ErrInvalidInput)
		}
	}
	p := k12.CurriculumProgress{
		ProgressID: "curr-" + shortDigest(agent+"\x00"+in.Subject),
		AgentName: agent, Subject: in.Subject, TextbookBindingID: catalog.TextbookBindingID,
		TextbookEdition: catalog.TextbookEdition, TextbookVersion: catalog.TextbookVersion,
		Title: catalog.Title, Volume: catalog.Volume, UnitID: unit.UnitID,
		UnitTitle: unit.Title, LessonID: in.LessonID, LessonTitle: lessonTitle,
		PageVerificationStatus: "not_requested",
		SegmentRefs: []string{"unit:" + unit.UnitID}, EvidenceSource: "parent_confirmed",
		ConfirmedAt: at, CreatedAt: at, UpdatedAt: at,
	}
	if p.LessonID != "" {
		p.SegmentRefs = append(p.SegmentRefs, "lesson:"+p.LessonID)
	}
	if in.PageFrom == nil && in.PageTo == nil {
		return p, nil
	}
	from, to := in.PageFrom, in.PageTo
	if from == nil {
		n := *to
		from = &n
	}
	if to == nil {
		n := *from
		to = &n
	}
	if *from > *to || *to < catalog.PageMin || *from > catalog.PageMax {
		return k12.CurriculumProgress{}, fmt.Errorf("%w: page range is outside catalog", ErrInvalidInput)
	}
	p.RequestedPageFrom, p.RequestedPageTo = from, to
	verifiedFrom, verifiedTo := max(*from, segmentFrom), min(*to, segmentTo)
	if verifiedFrom > verifiedTo {
		p.PageVerificationStatus = "rejected"
		return p, nil
	}
	p.VerifiedPageFrom, p.VerifiedPageTo = &verifiedFrom, &verifiedTo
	if verifiedFrom == *from && verifiedTo == *to {
		p.PageVerificationStatus = "verified"
	} else {
		p.PageVerificationStatus = "partially_verified"
	}
	return p, nil
}

type EnsureWeeklyPracticePlanRequest struct {
	AgentName      string
	IdempotencyKey string
}

func (d Deps) EnsureWeeklyPracticePlan(ctx context.Context,
	req EnsureWeeklyPracticePlanRequest) (k12.WeeklyPracticePlan, bool, error) {
	req.AgentName, req.IdempotencyKey = strings.TrimSpace(req.AgentName), strings.TrimSpace(req.IdempotencyKey)
	if req.AgentName == "" || req.IdempotencyKey == "" {
		return k12.WeeklyPracticePlan{}, false, fmt.Errorf("%w: agent/idempotency_key required", ErrInvalidInput)
	}
	settings, err := d.GetWeeklyPracticeSettings(ctx, req.AgentName)
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	window, err := weeklyWindow(d.now(), settings.Timezone)
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	if err := d.Records.ReconcileWeeklyPracticeBoundary(ctx, req.AgentName,
		window.Year, window.Week, settings.Timezone, d.now()); err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	progress, err := d.GetCurriculumProgress(ctx, req.AgentName, "math")
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	dueTrack, dueKeys, err := d.weeklyDueTrack(ctx, req.AgentName)
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	sourceDigest := digestValue(struct {
		Agent string
		Year int
		Week int
		Timezone string
		SettingsRevision int
		ProgressRevision int
		Due k12.WeeklyPracticeTrack
	}{
		req.AgentName, window.Year, window.Week, settings.Timezone, settings.Revision,
		progressRevision(progress), dueTrack,
	})
	if stored, found, replayErr := d.Records.ReplayWeeklyPracticePlan(ctx,
		req.AgentName, req.IdempotencyKey, sourceDigest, window.Year, window.Week,
		settings.Timezone, d.now()); replayErr != nil {
		return k12.WeeklyPracticePlan{}, false, replayErr
	} else if found {
		return stored, true, nil
	}
	tracks := []k12.WeeklyPracticeTrack{dueTrack}
	answerKeys := dueKeys
	elapsed := len(dueTrack.Items) * 60
	syncTrack, syncKeys, syncSeconds := d.weeklySupplementTrack(
		ctx, req.AgentName, k12.WeeklySectionTextbookConsolidation,
		settings.TextbookConsolidationEnabled, progress, 4, 0, max(0, 600-elapsed))
	tracks = append(tracks, syncTrack)
	elapsed += syncSeconds
	for key, value := range syncKeys {
		answerKeys[key] = value
	}
	arithmeticTrack, arithmeticKeys, _ := d.weeklySupplementTrack(
		ctx, req.AgentName, k12.WeeklySectionArithmeticWarmup,
		settings.ArithmeticWarmupEnabled, progress, 100, settings.ArithmeticMinutes,
		min(settings.ArithmeticMinutes*60, max(0, 600-elapsed)))
	tracks = append(tracks, arithmeticTrack)
	for key, value := range arithmeticKeys {
		answerKeys[key] = value
	}
	at := d.now()
	progressRev := optionalProgressRevision(progress)
	plan := k12.WeeklyPracticePlan{
		PlanID: "wplan-" + shortDigest(fmt.Sprintf("%s\x00%d\x00%d\x00%s",
			req.AgentName, window.Year, window.Week, settings.Timezone)),
		AgentName: req.AgentName, Revision: 1, ISOWeekYear: window.Year,
		ISOWeekNumber: window.Week, Timezone: settings.Timezone,
		WeekStart: window.Start, WeekEnd: window.End,
		LocalStartDate: window.LocalStart, LocalEndDate: window.LocalEnd,
		Status: k12.WeeklyPlanDraft, SettingsRevision: settings.Revision,
		CurriculumProgressRevision: progressRev, Tracks: tracks,
		CreatedAt: at, UpdatedAt: at, SourceDigest: sourceDigest, AnswerKeys: answerKeys,
	}
	return d.Records.UpsertWeeklyPracticePlan(ctx, plan, req.IdempotencyKey, sourceDigest)
}

type isoWeeklyWindow struct {
	Year, Week int
	Start, End int64
	LocalStart, LocalEnd string
}

func weeklyWindow(now int64, timezone string) (isoWeeklyWindow, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return isoWeeklyWindow{}, fmt.Errorf("%w: invalid timezone", ErrInvalidInput)
	}
	local := time.Unix(now, 0).In(loc)
	year, week := local.ISOWeek()
	offset := (int(local.Weekday()) + 6) % 7
	start := time.Date(local.Year(), local.Month(), local.Day()-offset, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 7).Add(-time.Second)
	return isoWeeklyWindow{
		Year: year, Week: week, Start: start.Unix(), End: end.Unix(),
		LocalStart: start.Format("2006-01-02"), LocalEnd: end.Format("2006-01-02"),
	}, nil
}

func (d Deps) weeklyDueTrack(ctx context.Context, agent string) (k12.WeeklyPracticeTrack, map[string]string, error) {
	queue, err := d.ReviewQueue(ctx, agent)
	if err != nil {
		return k12.WeeklyPracticeTrack{}, nil, err
	}
	track := k12.WeeklyPracticeTrack{
		PlanSection: k12.WeeklySectionDueReview, Status: k12.WeeklyTrackReady,
		Items: []k12.WeeklyPracticeItem{},
	}
	keys := map[string]string{}
	for _, due := range queue {
		if due.Record == nil || due.Record.Collection != k12.CollectionMistakes || len(track.Items) >= 15 {
			continue
		}
		itemID := "witem-" + shortDigest(k12.WeeklySectionDueReview+"\x00"+due.Record.RecordID)
		item := k12.WeeklyPracticeItem{
			ItemID: itemID, Position: len(track.Items) + 1,
			PlanSection: k12.WeeklySectionDueReview, SourceKind: "mistake",
			GenerationMethod: k12.WeeklyGenerationMethodOriginal,
			SourceRef: due.Record.RecordID,
			Verification: k12.WeeklyPracticeVerification{
				Status: k12.WeeklyVerificationVerified,
				EvidenceRefs: []string{"mistake:" + due.Record.RecordID},
			},
			PromptMarkdown: due.Title(),
		}
		track.Items = append(track.Items, item)
		if strings.TrimSpace(due.Fields.CanonicalAnswer) != "" {
			keys[itemID] = due.Fields.CanonicalAnswer
		}
	}
	return track, keys, nil
}

func (d Deps) weeklySupplementTrack(ctx context.Context, agent, section string,
	enabled bool, progress *k12.CurriculumProgress, maxItems, minutes, budget int,
) (k12.WeeklyPracticeTrack, map[string]string, int) {
	track := k12.WeeklyPracticeTrack{PlanSection: section, Items: []k12.WeeklyPracticeItem{}}
	keys := map[string]string{}
	if !enabled {
		track.Status = k12.WeeklyTrackDisabled
		return track, keys, 0
	}
	if progress == nil || progress.Revision <= 0 || progress.EvidenceSource != "parent_confirmed" ||
		d.WeeklyCandidates == nil {
		track.Status = k12.WeeklyTrackFailed
		return track, keys, 0
	}
	candidates, err := d.WeeklyCandidates.GenerateWeeklyPracticeCandidates(ctx,
		WeeklyPracticeCandidateRequest{
			AgentName: agent, PlanSection: section, MaxItems: maxItems,
			ArithmeticMinutes: minutes, Progress: *progress,
		})
	if err != nil {
		track.Status = k12.WeeklyTrackFailed
		return track, keys, 0
	}
	elapsed := 0
	for _, candidate := range candidates {
		generationMethod := strings.TrimSpace(candidate.GenerationMethod)
		if len(track.Items) >= maxItems || strings.TrimSpace(candidate.PromptMarkdown) == "" ||
			strings.TrimSpace(candidate.SourceKind) == "" ||
			!k12.WeeklySupplementGenerationMethodAllowed(generationMethod) ||
			strings.TrimSpace(candidate.SourceRef) == "" || len(candidate.EvidenceRefs) == 0 {
			continue
		}
		seconds := candidate.EstimatedSeconds
		if seconds <= 0 {
			seconds = 60
		}
		if elapsed+seconds > budget || elapsed+seconds > 900 {
			continue
		}
		itemID := "witem-" + shortDigest(section+"\x00"+candidate.SourceRef+
			"\x00"+candidate.PromptMarkdown)
		verification := k12.WeeklyPracticeVerification{
			Status: k12.WeeklyVerificationVerified,
			EvidenceRefs: append([]string(nil), candidate.EvidenceRefs...),
		}
		if section == k12.WeeklySectionTextbookConsolidation {
			verification.TextbookBindingID = progress.TextbookBindingID
			verification.UnitID, verification.LessonID = progress.UnitID, progress.LessonID
			verification.VerifiedPageFrom, verification.VerifiedPageTo =
				progress.VerifiedPageFrom, progress.VerifiedPageTo
		}
		track.Items = append(track.Items, k12.WeeklyPracticeItem{
			ItemID: itemID, Position: len(track.Items) + 1, PlanSection: section,
			SourceKind: candidate.SourceKind, GenerationMethod: generationMethod,
			SourceRef: candidate.SourceRef, Verification: verification,
			PromptMarkdown: candidate.PromptMarkdown,
		})
		if strings.TrimSpace(candidate.ExpectedAnswer) != "" {
			keys[itemID] = candidate.ExpectedAnswer
		}
		elapsed += seconds
	}
	if len(track.Items) == 0 {
		track.Status = k12.WeeklyTrackFailed
	} else {
		track.Status = k12.WeeklyTrackReady
	}
	return track, keys, elapsed
}

func progressRevision(p *k12.CurriculumProgress) int {
	if p == nil {
		return 0
	}
	return p.Revision
}

func optionalProgressRevision(p *k12.CurriculumProgress) *int {
	if p == nil {
		return nil
	}
	n := p.Revision
	return &n
}

func (d Deps) GetCurrentWeeklyPracticePlan(ctx context.Context, agent string) (*k12.WeeklyPracticePlan, error) {
	settings, err := d.GetWeeklyPracticeSettings(ctx, agent)
	if err != nil {
		return nil, err
	}
	window, err := weeklyWindow(d.now(), settings.Timezone)
	if err != nil {
		return nil, err
	}
	if err := d.Records.ReconcileWeeklyPracticeBoundary(ctx, agent,
		window.Year, window.Week, settings.Timezone, d.now()); err != nil {
		return nil, err
	}
	plan, err := d.Records.GetWeeklyPracticePlanForWeek(ctx, agent, window.Year, window.Week, settings.Timezone)
	if errors.Is(err, records.ErrNotFound) {
		return nil, nil
	}
	return &plan, err
}

func (d Deps) ListWeeklyPracticeHistory(ctx context.Context, agent, cursor string,
	limit int) ([]k12.WeeklyPracticeHistorySummary, *string, error) {
	if strings.TrimSpace(agent) == "" || limit < 1 || limit > 100 {
		return nil, nil, fmt.Errorf("%w: agent and limit 1..100 required", ErrInvalidInput)
	}
	settings, err := d.GetWeeklyPracticeSettings(ctx, agent)
	if err != nil {
		return nil, nil, err
	}
	window, err := weeklyWindow(d.now(), settings.Timezone)
	if err != nil {
		return nil, nil, err
	}
	if err := d.Records.ReconcileWeeklyPracticeBoundary(ctx, agent,
		window.Year, window.Week, settings.Timezone, d.now()); err != nil {
		return nil, nil, err
	}
	return d.Records.ListWeeklyPracticeHistory(ctx, agent, cursor, limit)
}

func (d Deps) GetWeeklyPracticeSnapshot(ctx context.Context, agent, snapshotID string) (k12.WeeklyPracticeSnapshot, error) {
	if strings.TrimSpace(agent) == "" || strings.TrimSpace(snapshotID) == "" {
		return k12.WeeklyPracticeSnapshot{}, fmt.Errorf("%w: agent/snapshot_id required", ErrInvalidInput)
	}
	return d.Records.GetWeeklyPracticeSnapshot(ctx, agent, snapshotID)
}

type WeeklyPrepareOutputResult struct {
	Snapshot k12.WeeklyPracticeSnapshot
	Artifact PrintableArtifactView
	Replayed bool
}

func (d Deps) PrepareWeeklyPracticeOutput(ctx context.Context, agent, planID string,
	expectedRevision int, idempotencyKey string) (WeeklyPrepareOutputResult, error) {
	if strings.TrimSpace(agent) == "" || strings.TrimSpace(planID) == "" ||
		strings.TrimSpace(idempotencyKey) == "" || expectedRevision < 1 {
		return WeeklyPrepareOutputResult{}, fmt.Errorf("%w: invalid prepare-output command", ErrInvalidInput)
	}
	plan, err := d.Records.GetWeeklyPracticePlan(ctx, agent, planID)
	if err != nil {
		return WeeklyPrepareOutputResult{}, err
	}
	if plan.Revision != expectedRevision {
		return WeeklyPrepareOutputResult{}, records.ErrVersionConflict
	}
	if plan.Status != k12.WeeklyPlanDraft && plan.Status != k12.WeeklyPlanFrozen {
		return WeeklyPrepareOutputResult{}, records.ErrIllegalTransition
	}
	at := d.now()
	var snapshot k12.WeeklyPracticeSnapshot
	if plan.Status == k12.WeeklyPlanFrozen {
		snapshot, err = d.Records.GetWeeklyPracticeSnapshotForPlan(
			ctx, agent, plan.PlanID, plan.Revision)
		if err != nil {
			return WeeklyPrepareOutputResult{}, err
		}
	} else {
		snapshot = k12.WeeklyPracticeSnapshot{
			SnapshotID: "wsnap-" + shortDigest(fmt.Sprintf("%s\x00%d", plan.PlanID, plan.Revision)),
			PlanID: plan.PlanID, PlanRevision: plan.Revision, AgentName: plan.AgentName,
			ISOWeekYear: plan.ISOWeekYear, ISOWeekNumber: plan.ISOWeekNumber,
			Timezone: plan.Timezone, WeekStart: plan.WeekStart, WeekEnd: plan.WeekEnd,
			LocalStartDate: plan.LocalStartDate, LocalEndDate: plan.LocalEndDate,
			SettingsRevision: plan.SettingsRevision,
			CurriculumProgressRevision: plan.CurriculumProgressRevision,
			Tracks: plan.Tracks, RenderVersion: "practice-paper-v1", CreatedAt: at,
			AnswerKeys: plan.AnswerKeys,
		}
		snapshot.SnapshotDigest = digestValue(struct {
			PlanID string
			PlanRevision int
			Tracks []k12.WeeklyPracticeTrack
			RenderVersion string
		}{snapshot.PlanID, snapshot.PlanRevision, snapshot.Tracks, snapshot.RenderVersion})
	}
	artifactReq, err := normalizePrintableArtifactRequest(PreparePrintableArtifactRequest{
		AgentName: agent, SourceKind: k12.PrintSourceWeeklyPracticeSnapshot,
		SourceRef: snapshot.SnapshotID, Title: "本周该练",
		CanonicalMarkdown: weeklySnapshotMarkdown(snapshot),
	})
	if err != nil {
		return WeeklyPrepareOutputResult{}, err
	}
	artifact := buildPrintArtifact(artifactReq, at)
	if plan.Status == k12.WeeklyPlanFrozen {
		frozenArtifact, artifactErr := d.Records.GetPrintArtifact(
			ctx, agent, artifact.ArtifactID)
		if artifactErr == nil {
			if !samePrintArtifact(frozenArtifact, artifact) {
				return WeeklyPrepareOutputResult{}, fmt.Errorf(
					"usecase: 打印 Artifact ID 已绑定其他内容")
			}
			frozenRender, renderErr := d.Records.GetPrintArtifactRender(
				ctx, agent, artifact.ArtifactID)
			if renderErr == nil {
				return WeeklyPrepareOutputResult{
					Snapshot: snapshot,
					Artifact: PrintableArtifactView{
						Artifact: frozenArtifact,
						Render: frozenRender,
					},
					Replayed: true,
				}, nil
			}
			if !errors.Is(renderErr, records.ErrNotFound) {
				return WeeklyPrepareOutputResult{}, renderErr
			}
		} else if !errors.Is(artifactErr, records.ErrNotFound) {
			return WeeklyPrepareOutputResult{}, artifactErr
		}
	}
	render, err := d.renderPrintableArtifact(ctx, artifact)
	if err != nil {
		return WeeklyPrepareOutputResult{}, err
	}
	frozenSnapshot, frozenArtifact, frozenRender, replay, err :=
		d.Records.FreezeWeeklyPracticeOutput(ctx, snapshot, artifact, render)
	if err != nil {
		return WeeklyPrepareOutputResult{}, err
	}
	return WeeklyPrepareOutputResult{
		Snapshot: frozenSnapshot,
		Artifact: PrintableArtifactView{
			Artifact: frozenArtifact,
			Render: frozenRender,
		},
		Replayed: replay,
	}, nil
}

func weeklySnapshotMarkdown(snapshot k12.WeeklyPracticeSnapshot) string {
	items := make([]k12.PracticeItem, 0)
	for _, track := range snapshot.Tracks {
		for _, item := range track.Items {
			if item.Verification.Status != k12.WeeklyVerificationVerified {
				continue
			}
			items = append(items, k12.PracticeItem{
				ItemID: item.ItemID, Subject: "数学", AddedVia: k12.PracticeAddedViaWeekly,
				QuestionMarkdown: item.PromptMarkdown,
				ExpectedAnswerMarkdown: snapshot.AnswerKeys[item.ItemID],
				VerificationStatus: "verified",
				VerificationEvidence: strings.Join(item.Verification.EvidenceRefs, ","),
			})
		}
	}
	fields := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly, Title: "本周该练", Items: items,
	}
	loc, _ := time.LoadLocation(snapshot.Timezone)
	return k12.RenderPaperMarkdown(fields, k12.PaperKindQuestion, k12.PaperMeta{
		Date: time.Unix(snapshot.WeekStart, 0).In(loc),
	})
}

func (d Deps) SendWeeklyPracticeSnapshot(ctx context.Context, agent, snapshotID,
	idempotencyKey string) (k12.DeliveryBatch, error) {
	agent, snapshotID, idempotencyKey = strings.TrimSpace(agent), strings.TrimSpace(snapshotID), strings.TrimSpace(idempotencyKey)
	if agent == "" || snapshotID == "" || idempotencyKey == "" {
		return k12.DeliveryBatch{}, fmt.Errorf("%w: agent/snapshot/idempotency_key required", ErrInvalidInput)
	}
	snapshot, err := d.GetWeeklyPracticeSnapshot(ctx, agent, snapshotID)
	if err != nil {
		return k12.DeliveryBatch{}, err
	}
	digest := digestValue(struct{ Agent, Snapshot string }{agent, snapshotID})
	if batchID, found, err := d.Records.GetWeeklySendCommand(ctx, agent, idempotencyKey, digest); err != nil {
		return k12.DeliveryBatch{}, err
	} else if found {
		return d.GetDeliveryBatch(ctx, agent, batchID)
	}
	batch, _, err := d.PrepareAndSendTextBatch(ctx, agent,
		k12.PrintSourceWeeklyPracticeSnapshot, snapshotID, weeklySnapshotMarkdown(snapshot))
	if err != nil {
		return k12.DeliveryBatch{}, err
	}
	if err := d.Records.PutWeeklySendCommand(ctx, agent, idempotencyKey, snapshotID,
		digest, batch.BatchID, d.now()); err != nil {
		if batchID, found, getErr := d.Records.GetWeeklySendCommand(
			ctx, agent, idempotencyKey, digest); getErr == nil && found {
			return d.GetDeliveryBatch(ctx, agent, batchID)
		}
		return k12.DeliveryBatch{}, err
	}
	return batch, nil
}

func (d Deps) SubmitWeeklyPracticeAttempt(ctx context.Context, agent, snapshotID,
	itemID, studentAnswer, idempotencyKey string) (k12.WeeklyPracticeAttempt, bool, error) {
	agent, snapshotID, itemID = strings.TrimSpace(agent), strings.TrimSpace(snapshotID), strings.TrimSpace(itemID)
	studentAnswer, idempotencyKey = strings.TrimSpace(studentAnswer), strings.TrimSpace(idempotencyKey)
	if agent == "" || snapshotID == "" || itemID == "" || studentAnswer == "" || idempotencyKey == "" {
		return k12.WeeklyPracticeAttempt{}, false, fmt.Errorf("%w: complete attempt required", ErrInvalidInput)
	}
	snapshot, err := d.GetWeeklyPracticeSnapshot(ctx, agent, snapshotID)
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	var item *k12.WeeklyPracticeItem
	for i := range snapshot.Tracks {
		for j := range snapshot.Tracks[i].Items {
			if snapshot.Tracks[i].Items[j].ItemID == itemID {
				item = &snapshot.Tracks[i].Items[j]
				break
			}
		}
	}
	if item == nil {
		return k12.WeeklyPracticeAttempt{}, false, records.ErrNotFound
	}
	requestDigest := digestValue(struct{ Snapshot, Item, Answer string }{snapshotID, itemID, studentAnswer})
	assessment := WeeklyPracticeAnswerAssessment{
		AssessmentID: "assessment-pending-" + shortDigest(requestDigest),
		Result: k12.WeeklyAttemptNeedsReview, VerificationEvidence: "assessment_unavailable",
	}
	if d.WeeklyAssessment != nil {
		if verified, assessErr := d.WeeklyAssessment.AssessWeeklyPracticeAnswer(ctx,
			WeeklyPracticeAnswerRequest{
				AgentName: agent, SnapshotID: snapshotID, Item: *item, StudentAnswer: studentAnswer,
			}); assessErr == nil &&
			(verified.Result == k12.WeeklyAttemptCorrect || verified.Result == k12.WeeklyAttemptWrong) &&
			strings.TrimSpace(verified.AssessmentID) != "" &&
			strings.TrimSpace(verified.VerificationEvidence) != "" {
			assessment = verified
		}
	}
	attempt := k12.WeeklyPracticeAttempt{
		AttemptID: "wattempt-" + shortDigest(snapshotID+"\x00"+itemID+"\x00"+idempotencyKey),
		SnapshotID: snapshotID, ItemID: itemID, AssessmentID: assessment.AssessmentID,
		Result: assessment.Result, VerificationEvidence: assessment.VerificationEvidence,
		CreatedAt: d.now(),
	}
	stored, replay, err := d.Records.PutWeeklyPracticeAttempt(ctx, agent, itemID,
		idempotencyKey, requestDigest, attempt)
	if err != nil || replay {
		return stored, replay, err
	}
	switch {
	case item.PlanSection == k12.WeeklySectionDueReview:
		stored.ReviewScheduled = true
		if stored.Result == k12.WeeklyAttemptCorrect {
			rec, getErr := d.Records.Get(ctx, item.SourceRef)
			if getErr != nil || rec.AgentName != agent || rec.Collection != k12.CollectionMistakes {
				return k12.WeeklyPracticeAttempt{}, false, records.ErrNotFound
			}
			if err := d.MarkRetried(ctx, rec.RecordID, rec.Version); err != nil {
				return k12.WeeklyPracticeAttempt{}, false, err
			}
		}
	case stored.Result == k12.WeeklyAttemptWrong:
		recorded, recordErr := d.RecordMistake(ctx, RecordMistakeRequest{
			AgentName: agent, Subject: assessment.Subject,
			SourceSession: "weekly-attempt:" + stored.AttemptID,
			Problem: item.PromptMarkdown, StudentAnswer: studentAnswer,
			KnowledgePoints: []string{assessment.KnowledgePoint},
		})
		if recordErr != nil {
			return k12.WeeklyPracticeAttempt{}, false, recordErr
		}
		stored.MistakeRecordID, stored.ReviewScheduled = recorded.RecordID, true
	}
	if err := d.Records.UpdateWeeklyPracticeAttempt(ctx, agent, stored, d.now()); err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	return stored, false, nil
}

func (d Deps) SaveWeeklyPracticeToPracticeSet(ctx context.Context, agent, planID string,
	expectedRevision int, idempotencyKey string) (k12.WeeklyPracticeSaveReceipt, bool, error) {
	if strings.TrimSpace(agent) == "" || strings.TrimSpace(planID) == "" ||
		strings.TrimSpace(idempotencyKey) == "" || expectedRevision < 1 {
		return k12.WeeklyPracticeSaveReceipt{}, false, fmt.Errorf("%w: invalid save command", ErrInvalidInput)
	}
	plan, err := d.Records.GetWeeklyPracticePlan(ctx, agent, planID)
	if err != nil {
		return k12.WeeklyPracticeSaveReceipt{}, false, err
	}
	if plan.Revision != expectedRevision {
		return k12.WeeklyPracticeSaveReceipt{}, false, records.ErrVersionConflict
	}
	if plan.Status != k12.WeeklyPlanFrozen {
		return k12.WeeklyPracticeSaveReceipt{}, false, records.ErrIllegalTransition
	}
	snapshot, err := d.Records.GetWeeklyPracticeSnapshotForPlan(ctx, agent, planID, expectedRevision)
	if err != nil {
		return k12.WeeklyPracticeSaveReceipt{}, false, records.ErrIllegalTransition
	}
	items := make([]k12.PracticeItem, 0)
	for _, track := range snapshot.Tracks {
		for _, item := range track.Items {
			if item.Verification.Status != k12.WeeklyVerificationVerified {
				continue
			}
			sourceProblemID := ""
			if item.PlanSection == k12.WeeklySectionDueReview {
				sourceProblemID = item.SourceRef
			}
			items = append(items, k12.PracticeItem{
				ItemID: item.ItemID, SourceProblemID: sourceProblemID, Subject: "数学",
				AddedVia: k12.PracticeAddedViaWeekly, QuestionMarkdown: item.PromptMarkdown,
				ExpectedAnswerMarkdown: snapshot.AnswerKeys[item.ItemID],
				VerificationStatus: "verified",
				VerificationEvidence: strings.Join(item.Verification.EvidenceRefs, ","),
			})
		}
	}
	if len(items) == 0 {
		return k12.WeeklyPracticeSaveReceipt{}, false, records.ErrIllegalTransition
	}
	practiceSetID, _, err := d.CreatePracticeSet(ctx, agent,
		fmt.Sprintf("weekly:%s:%d", planID, expectedRevision),
		k12.PracticeSetFields{SourceKind: k12.PracticeSourceWeekly, Title: "本周该练", Items: items})
	if err != nil {
		return k12.WeeklyPracticeSaveReceipt{}, false, err
	}
	receipt := k12.WeeklyPracticeSaveReceipt{
		SaveReceiptID: "wsave-" + shortDigest(fmt.Sprintf("%s\x00%d", planID, expectedRevision)),
		PlanID: planID, PlanRevision: expectedRevision, SnapshotID: snapshot.SnapshotID,
		PracticeSetID: practiceSetID, CreatedAt: d.now(),
	}
	requestDigest := digestValue(struct {
		Agent, Plan string
		Revision int
	}{agent, planID, expectedRevision})
	return d.Records.PutWeeklyPracticeSave(ctx, agent, idempotencyKey, requestDigest, receipt)
}

func digestValue(v any) string {
	payload, _ := json.Marshal(v)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}
