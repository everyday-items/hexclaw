package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type ProfileBundleMutation struct {
	OwnerID                  string
	AgentName                string
	IdempotencyKey           string
	RequestDigest            string
	ExpectedProfileRevision  int
	ExpectedProgressRevision int
	ExpectedSettingsRevision int
	AgentConfig              *k12.ProfileBundleAgentConfig
	Profile                  k12.ChildProfile
	Progress                 k12.CurriculumProgress
	Settings                 k12.WeeklyPracticeSettings
	At                       int64
}

func (s *Store) GetProfileState(ctx context.Context, agentName string) (k12.WeeklyProfile, error) {
	agentName = strings.TrimSpace(agentName)
	var metadata string
	var revision int
	err := s.db.QueryRowContext(ctx, `SELECT a.metadata,
        COALESCE((SELECT revision FROM k12_profile_revisions r WHERE r.agent_name=a.name),0)
        FROM agents a WHERE a.name=?`, agentName).Scan(&metadata, &revision)
	if err == sql.ErrNoRows {
		return k12.WeeklyProfile{}, records.ErrNotFound
	}
	if err != nil {
		return k12.WeeklyProfile{}, fmt.Errorf("k12storage: get profile state: %w", err)
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		return k12.WeeklyProfile{}, fmt.Errorf("k12storage: decode profile metadata: %w", err)
	}
	p := k12.ProfileFromMeta(meta)
	return k12.WeeklyProfile{
		ChildName: p.ChildName, GradeTerm: p.GradeTerm,
		SubjectTextbooks: p.SubjectTextbooks,
		TextbookEdition:  p.TextbookEdition, Revision: revision,
	}, nil
}

func (s *Store) PatchLegacyProfile(ctx context.Context, agentName string,
	profile k12.ChildProfile, at int64) (k12.ChildProfile, error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" || at <= 0 {
		return k12.ChildProfile{}, fmt.Errorf("k12storage: incomplete legacy profile patch")
	}
	if err := ensureAgentRegistered(ctx, s.db, agentName); err != nil {
		return k12.ChildProfile{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ChildProfile{}, err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRowContext(ctx,
		`SELECT metadata FROM agents WHERE name=?`, agentName).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return k12.ChildProfile{}, records.ErrNotFound
		}
		return k12.ChildProfile{}, err
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return k12.ChildProfile{}, err
	}
	meta = k12.ApplyProfileToMeta(meta, profile)
	encoded, err := json.Marshal(meta)
	if err != nil {
		return k12.ChildProfile{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agents SET metadata=?,updated_at=? WHERE name=?`,
		string(encoded), at, agentName); err != nil {
		return k12.ChildProfile{}, err
	}
	if err := tx.Commit(); err != nil {
		return k12.ChildProfile{}, err
	}
	return k12.ProfileFromMeta(meta), nil
}

func (s *Store) GetCurriculumProgress(ctx context.Context, agentName, subject string) (k12.CurriculumProgress, error) {
	row := s.db.QueryRowContext(ctx, curriculumProgressSelect+` WHERE agent_name=? AND subject=?`,
		strings.TrimSpace(agentName), strings.TrimSpace(subject))
	return scanCurriculumProgress(row)
}

const curriculumProgressSelect = `SELECT progress_id,agent_name,subject,revision,
    textbook_binding_id,textbook_manifest_id,textbook_edition,textbook_version,title,volume,
    unit_id,unit_title,lesson_id,lesson_title,requested_page_from,requested_page_to,
    verified_page_from,verified_page_to,page_verification_status,segment_refs_json,
    evidence_source,confirmed_at,created_at,updated_at FROM k12_curriculum_progress`

func scanCurriculumProgress(row rowScanner) (k12.CurriculumProgress, error) {
	var p k12.CurriculumProgress
	var requestedFrom, requestedTo, verifiedFrom, verifiedTo sql.NullInt64
	var refsJSON string
	err := row.Scan(
		&p.ProgressID, &p.AgentName, &p.Subject, &p.Revision,
		&p.TextbookBindingID, &p.TextbookManifestID, &p.TextbookEdition,
		&p.TextbookVersion, &p.Title, &p.Volume,
		&p.UnitID, &p.UnitTitle, &p.LessonID, &p.LessonTitle,
		&requestedFrom, &requestedTo, &verifiedFrom, &verifiedTo,
		&p.PageVerificationStatus, &refsJSON, &p.EvidenceSource,
		&p.ConfirmedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return k12.CurriculumProgress{}, records.ErrNotFound
	}
	if err != nil {
		return k12.CurriculumProgress{}, fmt.Errorf("k12storage: scan curriculum progress: %w", err)
	}
	p.RequestedPageFrom = nullableInt(requestedFrom)
	p.RequestedPageTo = nullableInt(requestedTo)
	p.VerifiedPageFrom = nullableInt(verifiedFrom)
	p.VerifiedPageTo = nullableInt(verifiedTo)
	if err := json.Unmarshal([]byte(refsJSON), &p.SegmentRefs); err != nil {
		return k12.CurriculumProgress{}, fmt.Errorf("k12storage: decode segment refs: %w", err)
	}
	if p.SegmentRefs == nil {
		p.SegmentRefs = []string{}
	}
	return p, nil
}

func nullableInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func (s *Store) GetWeeklyPracticeSettings(ctx context.Context, agentName string) (k12.WeeklyPracticeSettings, error) {
	agentName = strings.TrimSpace(agentName)
	if err := ensureAgentRegistered(ctx, s.db, agentName); err != nil {
		return k12.WeeklyPracticeSettings{}, err
	}
	var out k12.WeeklyPracticeSettings
	var due, textbook, arithmetic int
	err := s.db.QueryRowContext(ctx, `SELECT agent_name,revision,timezone,
        due_review_enabled,textbook_consolidation_enabled,arithmetic_warmup_enabled,
        textbook_consolidation_tier,arithmetic_minutes,created_at,updated_at
        FROM k12_weekly_practice_settings WHERE agent_name=?`, agentName).Scan(
		&out.AgentName, &out.Revision, &out.Timezone, &due, &textbook, &arithmetic,
		&out.TextbookConsolidationTier, &out.ArithmeticMinutes, &out.CreatedAt, &out.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return k12.DefaultWeeklyPracticeSettings(agentName), nil
	}
	if err != nil {
		return k12.WeeklyPracticeSettings{}, fmt.Errorf("k12storage: get weekly settings: %w", err)
	}
	out.DueReviewEnabled = due == 1
	out.TextbookConsolidationEnabled = textbook == 1
	out.ArithmeticWarmupEnabled = arithmetic == 1
	return out, nil
}

func (s *Store) UpdateProfileBundle(ctx context.Context, in ProfileBundleMutation) (k12.ProfileBundleResult, bool, error) {
	if strings.TrimSpace(in.OwnerID) == "" || strings.TrimSpace(in.AgentName) == "" ||
		strings.TrimSpace(in.IdempotencyKey) == "" ||
		strings.TrimSpace(in.RequestDigest) == "" || in.At <= 0 {
		return k12.ProfileBundleResult{}, false, fmt.Errorf("k12storage: incomplete profile bundle")
	}
	if err := ensureAgentRegistered(ctx, s.db, in.AgentName); err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	defer tx.Rollback()

	var digest, responseJSON string
	err = tx.QueryRowContext(ctx, `SELECT request_digest,response_json
        FROM k12_profile_bundle_commands WHERE agent_name=? AND idempotency_key=?`,
		in.AgentName, in.IdempotencyKey).Scan(&digest, &responseJSON)
	if err == nil {
		if digest != in.RequestDigest {
			return k12.ProfileBundleResult{}, false, records.ErrVersionConflict
		}
		var replay k12.ProfileBundleResult
		if err := json.Unmarshal([]byte(responseJSON), &replay); err != nil {
			return k12.ProfileBundleResult{}, false, err
		}
		replay.Replayed = true
		return replay, true, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return k12.ProfileBundleResult{}, false, err
	}

	var metadata string
	var profileRevision int
	if err := tx.QueryRowContext(ctx, `SELECT a.metadata,
        COALESCE((SELECT revision FROM k12_profile_revisions r WHERE r.agent_name=a.name),0)
        FROM agents a WHERE a.name=?`, in.AgentName).Scan(&metadata, &profileRevision); err != nil {
		if err == sql.ErrNoRows {
			return k12.ProfileBundleResult{}, false, records.ErrNotFound
		}
		return k12.ProfileBundleResult{}, false, err
	}
	progressRevision, err := revisionVia(ctx, tx,
		`SELECT revision FROM k12_curriculum_progress WHERE agent_name=? AND subject=?`,
		in.AgentName, in.Progress.Subject)
	if err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	settingsRevision, err := revisionVia(ctx, tx,
		`SELECT revision FROM k12_weekly_practice_settings WHERE agent_name=?`, in.AgentName)
	if err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	if profileRevision != in.ExpectedProfileRevision ||
		progressRevision != in.ExpectedProgressRevision ||
		settingsRevision != in.ExpectedSettingsRevision {
		return k12.ProfileBundleResult{}, false, records.ErrVersionConflict
	}
	if strings.TrimSpace(in.Progress.TextbookManifestID) != "" {
		bindingID, bindErr := activateTextbookBindingTx(
			ctx, tx, TextbookScope{
				OwnerID: in.OwnerID, AgentName: in.AgentName, Subject: in.Progress.Subject,
			}, in.Profile, in.Progress, in.At,
		)
		if bindErr != nil {
			return k12.ProfileBundleResult{}, false, bindErr
		}
		in.Progress.TextbookBindingID = bindingID
	}

	var meta map[string]string
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	meta = k12.ApplyProfileToMeta(meta, in.Profile)
	metadataBytes, err := json.Marshal(meta)
	if err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	if in.AgentConfig != nil {
		skillsJSON, marshalErr := json.Marshal(in.AgentConfig.Skills)
		if marshalErr != nil {
			return k12.ProfileBundleResult{}, false, marshalErr
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents
		    SET display_name=?,description=?,system_prompt=?,provider=?,model=?,skills=?,
		        metadata=?,updated_at=? WHERE name=?`,
			in.AgentConfig.DisplayName, in.AgentConfig.Description,
			in.AgentConfig.SystemPrompt, in.AgentConfig.Provider, in.AgentConfig.Model,
			string(skillsJSON), string(metadataBytes), in.At, in.AgentName); err != nil {
			return k12.ProfileBundleResult{}, false, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET metadata=?,updated_at=? WHERE name=?`,
			string(metadataBytes), in.At, in.AgentName); err != nil {
			return k12.ProfileBundleResult{}, false, err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM k12_profile_revisions WHERE agent_name=?`,
		in.AgentName).Scan(&profileRevision); err != nil {
		return k12.ProfileBundleResult{}, false, err
	}

	in.Progress.Revision = progressRevision + 1
	in.Progress.AgentName = in.AgentName
	in.Progress.UpdatedAt = in.At
	if progressRevision == 0 {
		in.Progress.CreatedAt = in.At
	}
	if err := upsertCurriculumProgress(ctx, tx, in.Progress); err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	in.Settings.Revision = settingsRevision + 1
	in.Settings.AgentName = in.AgentName
	in.Settings.DueReviewEnabled = true
	in.Settings.UpdatedAt = in.At
	if settingsRevision == 0 {
		in.Settings.CreatedAt = in.At
	}
	if err := upsertWeeklySettings(ctx, tx, in.Settings); err != nil {
		return k12.ProfileBundleResult{}, false, err
	}

	result := k12.ProfileBundleResult{
		AgentConfig: in.AgentConfig,
		Profile: k12.ProfileBundleProfile{
			ChildName: in.Profile.ChildName, GradeTerm: in.Profile.GradeTerm,
			SubjectTextbooks: in.Profile.SubjectTextbooks,
			TextbookEdition:  in.Profile.SubjectTextbooks.Math,
		},
		CurriculumProgress: in.Progress, WeeklyPracticeSettings: in.Settings,
	}
	responseBytes, err := json.Marshal(result)
	if err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_profile_bundle_commands
        (agent_name,idempotency_key,request_digest,response_json,created_at)
        VALUES(?,?,?,?,?)`, in.AgentName, in.IdempotencyKey, in.RequestDigest,
		string(responseBytes), in.At); err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.ProfileBundleResult{}, false, err
	}
	return result, false, nil
}

func revisionVia(ctx context.Context, tx *sql.Tx, query string, args ...any) (int, error) {
	var revision int
	err := tx.QueryRowContext(ctx, query, args...).Scan(&revision)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return revision, err
}

func upsertCurriculumProgress(ctx context.Context, tx *sql.Tx, p k12.CurriculumProgress) error {
	refs, _ := json.Marshal(p.SegmentRefs)
	_, err := tx.ExecContext(ctx, `INSERT INTO k12_curriculum_progress
        (progress_id,agent_name,subject,revision,textbook_binding_id,textbook_manifest_id,textbook_edition,
         textbook_version,title,volume,unit_id,unit_title,lesson_id,lesson_title,
         requested_page_from,requested_page_to,verified_page_from,verified_page_to,
         page_verification_status,segment_refs_json,evidence_source,confirmed_at,created_at,updated_at)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(agent_name,subject) DO UPDATE SET
         progress_id=excluded.progress_id,revision=excluded.revision,
         textbook_binding_id=excluded.textbook_binding_id,
         textbook_manifest_id=excluded.textbook_manifest_id,
         textbook_edition=excluded.textbook_edition,textbook_version=excluded.textbook_version,
         title=excluded.title,volume=excluded.volume,unit_id=excluded.unit_id,
         unit_title=excluded.unit_title,lesson_id=excluded.lesson_id,
         lesson_title=excluded.lesson_title,requested_page_from=excluded.requested_page_from,
         requested_page_to=excluded.requested_page_to,verified_page_from=excluded.verified_page_from,
         verified_page_to=excluded.verified_page_to,
         page_verification_status=excluded.page_verification_status,
         segment_refs_json=excluded.segment_refs_json,evidence_source=excluded.evidence_source,
         confirmed_at=excluded.confirmed_at,updated_at=excluded.updated_at`,
		p.ProgressID, p.AgentName, p.Subject, p.Revision, p.TextbookBindingID,
		p.TextbookManifestID,
		p.TextbookEdition, p.TextbookVersion, p.Title, p.Volume, p.UnitID, p.UnitTitle,
		p.LessonID, p.LessonTitle, p.RequestedPageFrom, p.RequestedPageTo,
		p.VerifiedPageFrom, p.VerifiedPageTo, p.PageVerificationStatus, string(refs),
		p.EvidenceSource, p.ConfirmedAt, p.CreatedAt, p.UpdatedAt)
	return err
}

func upsertWeeklySettings(ctx context.Context, tx *sql.Tx, v k12.WeeklyPracticeSettings) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_practice_settings
        (agent_name,revision,timezone,due_review_enabled,textbook_consolidation_enabled,
         arithmetic_warmup_enabled,textbook_consolidation_tier,arithmetic_minutes,created_at,updated_at)
        VALUES(?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(agent_name) DO UPDATE SET revision=excluded.revision,
         timezone=excluded.timezone,due_review_enabled=excluded.due_review_enabled,
         textbook_consolidation_enabled=excluded.textbook_consolidation_enabled,
         arithmetic_warmup_enabled=excluded.arithmetic_warmup_enabled,
         textbook_consolidation_tier=excluded.textbook_consolidation_tier,
         arithmetic_minutes=excluded.arithmetic_minutes,updated_at=excluded.updated_at`,
		v.AgentName, v.Revision, v.Timezone, 1, boolInt(v.TextbookConsolidationEnabled),
		boolInt(v.ArithmeticWarmupEnabled), v.TextbookConsolidationTier,
		v.ArithmeticMinutes, v.CreatedAt, v.UpdatedAt)
	return err
}

func (s *Store) ReconcileWeeklyPracticeBoundary(ctx context.Context, agentName string,
	year, week int, timezone string, at int64) error {
	agentName = strings.TrimSpace(agentName)
	timezone = strings.TrimSpace(timezone)
	if agentName == "" || year < 1 || week < 1 || timezone == "" || at <= 0 {
		return fmt.Errorf("k12storage: incomplete weekly boundary")
	}
	if err := ensureAgentRegistered(ctx, s.db, agentName); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE k12_weekly_practice_plans
        SET status=CASE WHEN EXISTS(
            SELECT 1 FROM k12_weekly_practice_snapshots s
            WHERE s.plan_id=k12_weekly_practice_plans.plan_id
        ) THEN 'archived' ELSE 'expired_unused' END,updated_at=?
        WHERE agent_name=? AND status IN ('draft','frozen')
          AND NOT (iso_week_year=? AND iso_week_number=? AND timezone=?)`,
		at, agentName, year, week, timezone)
	return err
}

func (s *Store) ReplayWeeklyPracticePlan(ctx context.Context, agentName,
	idempotencyKey, requestDigest string, year, week int, timezone string,
	at int64) (k12.WeeklyPracticePlan, bool, error) {
	agentName = strings.TrimSpace(agentName)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	timezone = strings.TrimSpace(timezone)
	if agentName == "" || idempotencyKey == "" || len(requestDigest) != 64 ||
		year < 1 || week < 1 || timezone == "" || at <= 0 {
		return k12.WeeklyPracticePlan{}, false,
			fmt.Errorf("k12storage: incomplete weekly replay lookup")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	defer tx.Rollback()

	var storedDigest, responseJSON string
	err = tx.QueryRowContext(ctx, `SELECT request_digest,response_json
        FROM k12_weekly_practice_plan_commands
        WHERE agent_name=? AND idempotency_key=?`,
		agentName, idempotencyKey).Scan(&storedDigest, &responseJSON)
	if err == nil {
		if storedDigest != requestDigest {
			return k12.WeeklyPracticePlan{}, false, records.ErrVersionConflict
		}
		var plan k12.WeeklyPracticePlan
		if err := json.Unmarshal([]byte(responseJSON), &plan); err != nil {
			return k12.WeeklyPracticePlan{}, false, err
		}
		return plan, true, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return k12.WeeklyPracticePlan{}, false, err
	}

	plan, err := getWeeklyPlanVia(ctx, tx, agentName,
		`iso_week_year=? AND iso_week_number=? AND timezone=?`,
		year, week, timezone)
	if errors.Is(err, records.ErrNotFound) {
		return k12.WeeklyPracticePlan{}, false, tx.Commit()
	}
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	if plan.SourceDigest != requestDigest {
		return k12.WeeklyPracticePlan{}, false, tx.Commit()
	}
	responseBytes, _ := json.Marshal(plan)
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_practice_plan_commands
        (agent_name,idempotency_key,request_digest,plan_id,plan_revision,response_json,created_at)
        VALUES(?,?,?,?,?,?,?)`, agentName, idempotencyKey, requestDigest,
		plan.PlanID, plan.Revision, string(responseBytes), at); err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	return plan, true, nil
}

func (s *Store) UpsertWeeklyPracticePlan(ctx context.Context, plan k12.WeeklyPracticePlan,
	idempotencyKey, requestDigest string) (k12.WeeklyPracticePlan, bool, error) {
	if err := ensureAgentRegistered(ctx, s.db, plan.AgentName); err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	defer tx.Rollback()
	var digest, responseJSON string
	err = tx.QueryRowContext(ctx, `SELECT request_digest,response_json
        FROM k12_weekly_practice_plan_commands WHERE agent_name=? AND idempotency_key=?`,
		plan.AgentName, idempotencyKey).Scan(&digest, &responseJSON)
	if err == nil {
		if digest != requestDigest {
			return k12.WeeklyPracticePlan{}, false, records.ErrVersionConflict
		}
		var frozen k12.WeeklyPracticePlan
		if err := json.Unmarshal([]byte(responseJSON), &frozen); err != nil {
			return k12.WeeklyPracticePlan{}, false, err
		}
		return frozen, true, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return k12.WeeklyPracticePlan{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_weekly_practice_plans
        SET status=CASE WHEN EXISTS(
            SELECT 1 FROM k12_weekly_practice_snapshots s
            WHERE s.plan_id=k12_weekly_practice_plans.plan_id
        ) THEN 'archived' ELSE 'expired_unused' END,updated_at=?
        WHERE agent_name=? AND status IN ('draft','frozen')
          AND NOT (iso_week_year=? AND iso_week_number=? AND timezone=?)`,
		plan.UpdatedAt, plan.AgentName, plan.ISOWeekYear, plan.ISOWeekNumber, plan.Timezone); err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}

	current, getErr := getWeeklyPlanVia(ctx, tx, plan.AgentName,
		`iso_week_year=? AND iso_week_number=? AND timezone=?`,
		plan.ISOWeekYear, plan.ISOWeekNumber, plan.Timezone)
	switch {
	case getErr == nil && current.SourceDigest == requestDigest:
		plan = current
	case getErr == nil && current.Status == k12.WeeklyPlanDraft:
		plan.PlanID = current.PlanID
		plan.Revision = current.Revision + 1
		plan.CreatedAt = current.CreatedAt
		if err := updateWeeklyPlanTx(ctx, tx, plan, requestDigest); err != nil {
			return k12.WeeklyPracticePlan{}, false, err
		}
	case getErr == nil:
		plan = current
	case getErr == records.ErrNotFound:
		plan.Revision = 1
		if err := insertWeeklyPlanTx(ctx, tx, plan, requestDigest); err != nil {
			return k12.WeeklyPracticePlan{}, false, err
		}
	default:
		return k12.WeeklyPracticePlan{}, false, getErr
	}
	responseBytes, _ := json.Marshal(plan)
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_practice_plan_commands
        (agent_name,idempotency_key,request_digest,plan_id,plan_revision,response_json,created_at)
        VALUES(?,?,?,?,?,?,?)`, plan.AgentName, idempotencyKey, requestDigest,
		plan.PlanID, plan.Revision, string(responseBytes), plan.UpdatedAt); err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.WeeklyPracticePlan{}, false, err
	}
	return plan, getErr == nil && current.SourceDigest == requestDigest, nil
}

func insertWeeklyPlanTx(ctx context.Context, tx *sql.Tx, p k12.WeeklyPracticePlan, digest string) error {
	planJSON, _ := json.Marshal(p)
	keysJSON, _ := json.Marshal(p.AnswerKeys)
	_, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_practice_plans
        (plan_id,agent_name,revision,iso_week_year,iso_week_number,timezone,
         week_start,week_end,local_start_date,local_end_date,status,settings_revision,
         curriculum_progress_revision,source_digest,plan_json,answer_keys_json,created_at,updated_at)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.PlanID, p.AgentName, p.Revision, p.ISOWeekYear, p.ISOWeekNumber, p.Timezone,
		p.WeekStart, p.WeekEnd, p.LocalStartDate, p.LocalEndDate, p.Status,
		p.SettingsRevision, p.CurriculumProgressRevision, digest, string(planJSON),
		string(keysJSON), p.CreatedAt, p.UpdatedAt)
	return err
}

func updateWeeklyPlanTx(ctx context.Context, tx *sql.Tx, p k12.WeeklyPracticePlan, digest string) error {
	planJSON, _ := json.Marshal(p)
	keysJSON, _ := json.Marshal(p.AnswerKeys)
	_, err := tx.ExecContext(ctx, `UPDATE k12_weekly_practice_plans SET
        revision=?,status=?,settings_revision=?,curriculum_progress_revision=?,
        source_digest=?,plan_json=?,answer_keys_json=?,updated_at=?
        WHERE plan_id=? AND agent_name=? AND status='draft'`,
		p.Revision, p.Status, p.SettingsRevision, p.CurriculumProgressRevision,
		digest, string(planJSON), string(keysJSON), p.UpdatedAt, p.PlanID, p.AgentName)
	return err
}

type weeklyPlanQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getWeeklyPlanVia(ctx context.Context, q weeklyPlanQuerier, agentName, where string,
	args ...any) (k12.WeeklyPracticePlan, error) {
	query := `SELECT plan_json,answer_keys_json,status,revision,source_digest,created_at,updated_at
        FROM k12_weekly_practice_plans WHERE agent_name=? AND ` + where
	all := append([]any{agentName}, args...)
	var planJSON, keysJSON, status, digest string
	var revision int
	var createdAt, updatedAt int64
	err := q.QueryRowContext(ctx, query, all...).Scan(
		&planJSON, &keysJSON, &status, &revision, &digest, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return k12.WeeklyPracticePlan{}, records.ErrNotFound
	}
	if err != nil {
		return k12.WeeklyPracticePlan{}, err
	}
	var plan k12.WeeklyPracticePlan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return k12.WeeklyPracticePlan{}, err
	}
	_ = json.Unmarshal([]byte(keysJSON), &plan.AnswerKeys)
	plan.Status, plan.Revision, plan.SourceDigest = status, revision, digest
	plan.CreatedAt, plan.UpdatedAt = createdAt, updatedAt
	for trackIndex := range plan.Tracks {
		items := plan.Tracks[trackIndex].Items
		projected := make([]k12.WeeklyPracticeItem, 0, len(items))
		for _, item := range items {
			review, reviewErr := getMistakeReviewStateVia(
				ctx, q, agentName, item.SourceRef,
			)
			if reviewErr != nil && !errors.Is(reviewErr, records.ErrNotFound) {
				return k12.WeeklyPracticePlan{}, reviewErr
			}
			if reviewErr == nil {
				if review.State == k12.MistakeReviewSuppressed ||
					review.State == k12.MistakeReviewMastered ||
					(review.State == k12.MistakeReviewDeferredThisWeek &&
						review.DeferredISOYear == plan.ISOWeekYear &&
						review.DeferredISOWeek == plan.ISOWeekNumber) {
					continue
				}
			}
			projected = append(projected, item)
		}
		plan.Tracks[trackIndex].Items = projected
	}
	return plan, nil
}

func (s *Store) GetWeeklyPracticePlan(ctx context.Context, agentName, planID string) (k12.WeeklyPracticePlan, error) {
	return getWeeklyPlanVia(ctx, s.db, strings.TrimSpace(agentName), `plan_id=?`, strings.TrimSpace(planID))
}

func (s *Store) ResolveWeeklyPracticePlanAgent(ctx context.Context, planID string) (string, error) {
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return "", records.ErrInvalidFields
	}
	var agentName string
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_name FROM k12_weekly_practice_plans WHERE plan_id=?`, planID,
	).Scan(&agentName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", records.ErrNotFound
	}
	return agentName, err
}

func (s *Store) GetWeeklyPracticePlanForWeek(ctx context.Context, agentName string,
	year, week int, timezone string) (k12.WeeklyPracticePlan, error) {
	return getWeeklyPlanVia(ctx, s.db, strings.TrimSpace(agentName),
		`iso_week_year=? AND iso_week_number=? AND timezone=?`, year, week, timezone)
}

func (s *Store) FreezeWeeklyPracticeSnapshot(ctx context.Context,
	snapshot k12.WeeklyPracticeSnapshot) (k12.WeeklyPracticeSnapshot, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WeeklyPracticeSnapshot{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE k12_weekly_practice_plans SET revision=revision
        WHERE plan_id=? AND agent_name=?`, snapshot.PlanID, snapshot.AgentName); err != nil {
		return k12.WeeklyPracticeSnapshot{}, false, err
	}
	plan, err := getWeeklyPlanVia(ctx, tx, snapshot.AgentName, `plan_id=?`, snapshot.PlanID)
	if err != nil {
		return k12.WeeklyPracticeSnapshot{}, false, err
	}
	if plan.Revision != snapshot.PlanRevision {
		return k12.WeeklyPracticeSnapshot{}, false, records.ErrVersionConflict
	}
	if plan.Status == k12.WeeklyPlanFrozen {
		existing, err := getWeeklySnapshotVia(ctx, tx, snapshot.AgentName,
			`plan_id=? AND plan_revision=?`, snapshot.PlanID, snapshot.PlanRevision)
		return existing, true, err
	}
	if plan.Status != k12.WeeklyPlanDraft {
		return k12.WeeklyPracticeSnapshot{}, false, records.ErrIllegalTransition
	}
	snapshotJSON, _ := json.Marshal(snapshot)
	keysJSON, _ := json.Marshal(snapshot.AnswerKeys)
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_practice_snapshots
        (snapshot_id,plan_id,plan_revision,agent_name,snapshot_digest,snapshot_json,
         answer_keys_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		snapshot.SnapshotID, snapshot.PlanID, snapshot.PlanRevision, snapshot.AgentName,
		snapshot.SnapshotDigest, string(snapshotJSON), string(keysJSON), snapshot.CreatedAt); err != nil {
		return k12.WeeklyPracticeSnapshot{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_weekly_practice_plans
        SET status='frozen',updated_at=? WHERE plan_id=? AND agent_name=? AND status='draft'`,
		snapshot.CreatedAt, snapshot.PlanID, snapshot.AgentName); err != nil {
		return k12.WeeklyPracticeSnapshot{}, false, err
	}
	return snapshot, false, tx.Commit()
}

func (s *Store) FreezeWeeklyPracticeOutput(ctx context.Context,
	snapshot k12.WeeklyPracticeSnapshot, artifact k12.PrintArtifact,
	render k12.PrintArtifactRender) (storedSnapshot k12.WeeklyPracticeSnapshot,
	storedArtifact k12.PrintArtifact, storedRender k12.PrintArtifactRender,
	replay bool, err error) {
	snapshot.ArtifactID = artifact.ArtifactID
	if strings.TrimSpace(snapshot.SnapshotID) == "" ||
		strings.TrimSpace(snapshot.PlanID) == "" || snapshot.PlanRevision < 1 ||
		strings.TrimSpace(snapshot.AgentName) == "" ||
		len(snapshot.SnapshotDigest) != 64 || snapshot.CreatedAt <= 0 ||
		artifact.AgentName != snapshot.AgentName ||
		artifact.SourceKind != k12.PrintSourceWeeklyPracticeSnapshot ||
		artifact.SourceRef != snapshot.SnapshotID ||
		strings.TrimSpace(artifact.ArtifactID) == "" ||
		strings.TrimSpace(artifact.Title) == "" ||
		strings.TrimSpace(artifact.CanonicalMarkdown) == "" ||
		len(artifact.SourceDigest) != 64 || artifact.CreatedAt <= 0 ||
		!validPrintArtifactRender(artifact, render) {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false,
			fmt.Errorf("k12storage: incomplete weekly output")
	}
	if err := ensureAgentRegistered(ctx, s.db, snapshot.AgentName); err != nil {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE k12_weekly_practice_plans
        SET revision=revision WHERE plan_id=? AND agent_name=?`,
		snapshot.PlanID, snapshot.AgentName); err != nil {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, err
	}
	plan, err := getWeeklyPlanVia(
		ctx, tx, snapshot.AgentName, `plan_id=?`, snapshot.PlanID)
	if err != nil {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, err
	}
	if plan.Revision != snapshot.PlanRevision {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, records.ErrVersionConflict
	}
	switch plan.Status {
	case k12.WeeklyPlanFrozen:
		storedSnapshot, err = getWeeklySnapshotVia(ctx, tx, snapshot.AgentName,
			`plan_id=? AND plan_revision=?`, snapshot.PlanID, snapshot.PlanRevision)
		if err != nil {
			return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
				k12.PrintArtifactRender{}, false, err
		}
		if storedSnapshot.SnapshotID != snapshot.SnapshotID ||
			storedSnapshot.SnapshotDigest != snapshot.SnapshotDigest {
			return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
				k12.PrintArtifactRender{}, false, records.ErrVersionConflict
		}
		replay = true
	case k12.WeeklyPlanDraft:
		storedSnapshot = snapshot
	default:
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, records.ErrIllegalTransition
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_print_artifacts
        (artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,
         source_digest,created_at) VALUES(?,?,?,?,?,?,?,?)
        ON CONFLICT DO NOTHING`, artifact.ArtifactID, artifact.AgentName,
		artifact.SourceKind, artifact.SourceRef, artifact.Title,
		artifact.CanonicalMarkdown, artifact.SourceDigest, artifact.CreatedAt); err != nil {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, err
	}
	storedArtifact, err = getPrintArtifactVia(
		ctx, tx, artifact.AgentName, artifact.ArtifactID)
	if err != nil {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, err
	}
	if storedArtifact.SourceKind != artifact.SourceKind ||
		storedArtifact.SourceRef != artifact.SourceRef ||
		storedArtifact.Title != artifact.Title ||
		storedArtifact.CanonicalMarkdown != artifact.CanonicalMarkdown ||
		storedArtifact.SourceDigest != artifact.SourceDigest {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false,
			fmt.Errorf("k12storage: weekly artifact identity conflict")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_print_artifact_renders
        (artifact_id,format,render_contract_version,content_type,byte_digest,
         byte_size,payload,created_at) VALUES(?,?,?,?,?,?,?,?)
        ON CONFLICT DO NOTHING`, render.ArtifactID, render.Format,
		render.RenderContractVersion, render.ContentType, render.ByteDigest,
		render.ByteSize, render.Payload, render.CreatedAt); err != nil {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, err
	}
	storedRender, err = getPrintArtifactRenderVia(
		ctx, tx, artifact.AgentName, artifact.ArtifactID)
	if err != nil {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, err
	}
	if !validPrintArtifactRender(storedArtifact, storedRender) {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false,
			fmt.Errorf("k12storage: invalid frozen weekly PDF")
	}
	if plan.Status == k12.WeeklyPlanDraft {
		snapshotJSON, _ := json.Marshal(snapshot)
		keysJSON, _ := json.Marshal(snapshot.AnswerKeys)
		if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_practice_snapshots
            (snapshot_id,plan_id,plan_revision,agent_name,snapshot_digest,snapshot_json,
             answer_keys_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			snapshot.SnapshotID, snapshot.PlanID, snapshot.PlanRevision,
			snapshot.AgentName, snapshot.SnapshotDigest, string(snapshotJSON),
			string(keysJSON), snapshot.CreatedAt); err != nil {
			return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
				k12.PrintArtifactRender{}, false, err
		}
		res, err := tx.ExecContext(ctx, `UPDATE k12_weekly_practice_plans
            SET status='frozen',updated_at=?
            WHERE plan_id=? AND agent_name=? AND status='draft'`,
			snapshot.CreatedAt, snapshot.PlanID, snapshot.AgentName)
		if err != nil {
			return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
				k12.PrintArtifactRender{}, false, err
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
				k12.PrintArtifactRender{}, false, records.ErrVersionConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return k12.WeeklyPracticeSnapshot{}, k12.PrintArtifact{},
			k12.PrintArtifactRender{}, false, err
	}
	return storedSnapshot, storedArtifact, storedRender, replay, nil
}

func getWeeklySnapshotVia(ctx context.Context, q weeklyPlanQuerier, agentName, where string,
	args ...any) (k12.WeeklyPracticeSnapshot, error) {
	all := append([]any{agentName}, args...)
	var snapshotJSON, keysJSON, artifactID string
	err := q.QueryRowContext(ctx, `SELECT s.snapshot_json,s.answer_keys_json,
        COALESCE((SELECT a.artifact_id FROM k12_print_artifacts a
          WHERE a.agent_name=s.agent_name
            AND a.source_kind='weekly_practice_snapshot'
            AND a.source_ref=s.snapshot_id LIMIT 1),'')
        FROM k12_weekly_practice_snapshots s WHERE s.agent_name=? AND `+where, all...).Scan(
		&snapshotJSON, &keysJSON, &artifactID)
	if err == sql.ErrNoRows {
		return k12.WeeklyPracticeSnapshot{}, records.ErrNotFound
	}
	if err != nil {
		return k12.WeeklyPracticeSnapshot{}, err
	}
	var snapshot k12.WeeklyPracticeSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
		return k12.WeeklyPracticeSnapshot{}, err
	}
	_ = json.Unmarshal([]byte(keysJSON), &snapshot.AnswerKeys)
	snapshot.ArtifactID = artifactID
	return snapshot, nil
}

func (s *Store) GetWeeklyPracticeSnapshot(ctx context.Context, agentName, snapshotID string) (k12.WeeklyPracticeSnapshot, error) {
	return getWeeklySnapshotVia(ctx, s.db, strings.TrimSpace(agentName), `snapshot_id=?`, strings.TrimSpace(snapshotID))
}

func (s *Store) GetWeeklyPracticeSnapshotForPlan(ctx context.Context, agentName, planID string,
	revision int) (k12.WeeklyPracticeSnapshot, error) {
	return getWeeklySnapshotVia(ctx, s.db, strings.TrimSpace(agentName),
		`plan_id=? AND plan_revision=?`, strings.TrimSpace(planID), revision)
}

func (s *Store) ListWeeklyPracticeHistory(ctx context.Context, agentName, cursor string,
	limit int) ([]k12.WeeklyPracticeHistorySummary, *string, error) {
	offset := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return nil, nil, fmt.Errorf("%w: invalid history cursor", records.ErrInvalidFields)
		}
		offset = n
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.snapshot_json,p.updated_at,artifact.artifact_id,
        COALESCE(SUM(CASE WHEN json_extract(a.attempt_json,'$.result')='correct' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN json_extract(a.attempt_json,'$.result')='wrong' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN json_extract(a.attempt_json,'$.result')='needs_review' THEN 1 ELSE 0 END),0)
        FROM k12_weekly_practice_snapshots s
        JOIN k12_weekly_practice_plans p ON p.plan_id=s.plan_id
        JOIN k12_print_artifacts artifact
          ON artifact.agent_name=s.agent_name
         AND artifact.source_kind='weekly_practice_snapshot'
         AND artifact.source_ref=s.snapshot_id
        LEFT JOIN (
          SELECT attempt_id,agent_name,snapshot_id,item_id,attempt_json FROM (
            SELECT attempt_id,agent_name,snapshot_id,item_id,attempt_json,
              ROW_NUMBER() OVER (
                PARTITION BY agent_name,snapshot_id,item_id
                ORDER BY updated_at DESC,attempt_id DESC
              ) AS latest_rank
            FROM k12_weekly_practice_attempts
          ) latest WHERE latest_rank=1
        ) a ON a.snapshot_id=s.snapshot_id AND a.agent_name=s.agent_name
        WHERE s.agent_name=? AND p.status='archived'
        GROUP BY s.snapshot_id,s.snapshot_json,p.updated_at,artifact.artifact_id
        ORDER BY p.updated_at DESC,s.snapshot_id DESC LIMIT ? OFFSET ?`,
		strings.TrimSpace(agentName), limit+1, offset)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := make([]k12.WeeklyPracticeHistorySummary, 0, limit)
	for rows.Next() {
		var snapshotJSON, artifactID string
		var archivedAt int64
		var correctCount, wrongCount, needsReviewCount int
		if err := rows.Scan(&snapshotJSON, &archivedAt, &artifactID,
			&correctCount, &wrongCount, &needsReviewCount); err != nil {
			return nil, nil, err
		}
		if len(out) == limit {
			next := strconv.Itoa(offset + limit)
			return out, &next, nil
		}
		var snapshot k12.WeeklyPracticeSnapshot
		if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
			return nil, nil, err
		}
		count := 0
		for _, track := range snapshot.Tracks {
			count += len(track.Items)
		}
		out = append(out, k12.WeeklyPracticeHistorySummary{
			SnapshotID: snapshot.SnapshotID, ArtifactID: artifactID, PlanID: snapshot.PlanID,
			ISOWeekYear: snapshot.ISOWeekYear, ISOWeekNumber: snapshot.ISOWeekNumber,
			Timezone: snapshot.Timezone, LocalStartDate: snapshot.LocalStartDate,
			LocalEndDate: snapshot.LocalEndDate, ItemCount: count,
			CorrectCount: correctCount, WrongCount: wrongCount,
			NeedsReviewCount: needsReviewCount, ArchivedAt: archivedAt,
		})
	}
	return out, nil, rows.Err()
}

func (s *Store) PutWeeklyPracticeAttempt(ctx context.Context, agentName, itemID,
	idempotencyKey, requestDigest string, attempt k12.WeeklyPracticeAttempt) (k12.WeeklyPracticeAttempt, bool, error) {
	attemptJSON, _ := json.Marshal(attempt)
	_, err := s.db.ExecContext(ctx, `INSERT INTO k12_weekly_practice_attempts
        (attempt_id,agent_name,snapshot_id,item_id,idempotency_key,request_digest,
         attempt_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		attempt.AttemptID, agentName, attempt.SnapshotID, itemID, idempotencyKey,
		requestDigest, string(attemptJSON), attempt.CreatedAt, attempt.CreatedAt)
	if err == nil {
		return attempt, false, nil
	}
	var storedDigest, storedJSON string
	getErr := s.db.QueryRowContext(ctx, `SELECT request_digest,attempt_json
        FROM k12_weekly_practice_attempts
        WHERE snapshot_id=? AND item_id=? AND idempotency_key=? AND agent_name=?`,
		attempt.SnapshotID, itemID, idempotencyKey, agentName).Scan(&storedDigest, &storedJSON)
	if getErr != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	if storedDigest != requestDigest {
		return k12.WeeklyPracticeAttempt{}, false, records.ErrVersionConflict
	}
	var stored k12.WeeklyPracticeAttempt
	if err := json.Unmarshal([]byte(storedJSON), &stored); err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	return stored, true, nil
}

func (s *Store) UpdateWeeklyPracticeAttempt(ctx context.Context, agentName string,
	attempt k12.WeeklyPracticeAttempt, at int64) error {
	payload, _ := json.Marshal(attempt)
	res, err := s.db.ExecContext(ctx, `UPDATE k12_weekly_practice_attempts
        SET attempt_json=?,updated_at=? WHERE attempt_id=? AND agent_name=?`,
		string(payload), at, attempt.AttemptID, agentName)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return records.ErrNotFound
	}
	return nil
}

func (s *Store) GetWeeklySendCommand(ctx context.Context, agentName, key,
	digest string) (string, bool, error) {
	var storedDigest, batchID string
	err := s.db.QueryRowContext(ctx, `SELECT request_digest,delivery_batch_id
        FROM k12_weekly_practice_sends WHERE agent_name=? AND idempotency_key=?`,
		agentName, key).Scan(&storedDigest, &batchID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if storedDigest != digest {
		return "", false, records.ErrVersionConflict
	}
	return batchID, true, nil
}

func (s *Store) PutWeeklySendCommand(ctx context.Context, agentName, key, snapshotID,
	digest, batchID string, at int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO k12_weekly_practice_sends
        (agent_name,idempotency_key,snapshot_id,request_digest,delivery_batch_id,created_at)
        VALUES(?,?,?,?,?,?)`, agentName, key, snapshotID, digest, batchID, at)
	if err != nil {
		return fmt.Errorf("k12storage: bind weekly send: %w", err)
	}
	return nil
}

func (s *Store) PutWeeklyPracticeSave(ctx context.Context, agentName,
	idempotencyKey, requestDigest string, receipt k12.WeeklyPracticeSaveReceipt) (k12.WeeklyPracticeSaveReceipt, bool, error) {
	var existingJSON, existingDigest string
	err := s.db.QueryRowContext(ctx, `SELECT receipt_json,request_digest
        FROM k12_weekly_practice_saves
        WHERE (plan_id=? AND plan_revision=?) OR (agent_name=? AND idempotency_key=?)
        ORDER BY CASE WHEN plan_id=? AND plan_revision=? THEN 0 ELSE 1 END LIMIT 1`,
		receipt.PlanID, receipt.PlanRevision, agentName, idempotencyKey,
		receipt.PlanID, receipt.PlanRevision).Scan(&existingJSON, &existingDigest)
	if err == nil {
		if existingDigest != requestDigest {
			return k12.WeeklyPracticeSaveReceipt{}, false, records.ErrVersionConflict
		}
		var existing k12.WeeklyPracticeSaveReceipt
		if err := json.Unmarshal([]byte(existingJSON), &existing); err != nil {
			return k12.WeeklyPracticeSaveReceipt{}, false, err
		}
		return existing, true, nil
	}
	if err != sql.ErrNoRows {
		return k12.WeeklyPracticeSaveReceipt{}, false, err
	}
	payload, _ := json.Marshal(receipt)
	_, err = s.db.ExecContext(ctx, `INSERT INTO k12_weekly_practice_saves
        (save_receipt_id,agent_name,plan_id,plan_revision,snapshot_id,practice_set_id,
         idempotency_key,request_digest,receipt_json,created_at)
        VALUES(?,?,?,?,?,?,?,?,?,?)`, receipt.SaveReceiptID, agentName, receipt.PlanID,
		receipt.PlanRevision, receipt.SnapshotID, receipt.PracticeSetID, idempotencyKey,
		requestDigest, string(payload), receipt.CreatedAt)
	if err != nil {
		return k12.WeeklyPracticeSaveReceipt{}, false, err
	}
	return receipt, false, nil
}
