package k12storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// LearningArchiveCreativeWork 冻结作品根与 current generation；不读取 legacy Versions。
type LearningArchiveCreativeWork struct {
	Record  *records.AgentRecord
	Fields  k12.CreativeWorkFields
	Initial *k12.WorkFeedbackGeneration
	Latest  *k12.WorkFeedbackGeneration
}

// LearningArchiveSourceSnapshot 是五对象导出的唯一 SQLite 只读事务边界。
type LearningArchiveSourceSnapshot struct {
	Profile       k12.ChildProfile
	AsOf          int64
	WeeklyReview  []k12.WeeklyPracticeItem
	Mistakes      []*records.AgentRecord
	PracticeSets  []*records.AgentRecord
	Accumulations []*records.AgentRecord
	CreativeWorks []LearningArchiveCreativeWork
}

// ReadLearningArchiveSourceSnapshot 在同一读事务中冻结当前 Tutor/学期的五对象。
func (s *Store) ReadLearningArchiveSourceSnapshot(
	ctx context.Context,
	agentName string,
	asOf int64,
) (snapshot LearningArchiveSourceSnapshot, retErr error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" || asOf <= 0 {
		return snapshot, fmt.Errorf("k12storage: learning archive agent and as_of are required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snapshot, fmt.Errorf("k12storage: begin learning archive snapshot: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil &&
			!errors.Is(rollbackErr, sql.ErrTxDone) && retErr == nil {
			retErr = fmt.Errorf("k12storage: roll back learning archive snapshot: %w", rollbackErr)
		}
	}()

	profile, err := readAgentProfileVia(ctx, tx, agentName)
	if err != nil {
		return snapshot, err
	}
	if strings.TrimSpace(profile.GradeTerm) == "" {
		return snapshot, fmt.Errorf("k12storage: learning archive current grade term is required")
	}
	snapshot.Profile = profile
	snapshot.AsOf = asOf

	plan, planErr := getWeeklyPlanVia(ctx, tx, agentName,
		`week_start<=? AND week_end>? AND status IN ('draft','frozen')
		 ORDER BY updated_at DESC,plan_id DESC LIMIT 1`, asOf, asOf)
	if planErr != nil && !errors.Is(planErr, records.ErrNotFound) {
		return snapshot, fmt.Errorf("k12storage: read current weekly practice plan: %w", planErr)
	}
	if planErr == nil {
		for _, track := range plan.Tracks {
			for _, item := range track.Items {
				if item.Verification.Status == k12.WeeklyVerificationVerified {
					snapshot.WeeklyReview = append(snapshot.WeeklyReview, item)
				}
			}
		}
	}
	if snapshot.WeeklyReview == nil {
		snapshot.WeeklyReview = []k12.WeeklyPracticeItem{}
	}
	sort.Slice(snapshot.WeeklyReview, func(i, j int) bool {
		left, right := snapshot.WeeklyReview[i], snapshot.WeeklyReview[j]
		leftRank, rightRank := weeklyArchiveSectionRank(left.PlanSection), weeklyArchiveSectionRank(right.PlanSection)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.Position != right.Position {
			return left.Position < right.Position
		}
		return left.ItemID < right.ItemID
	})

	snapshot.Mistakes, err = s.queryRecordsVia(ctx, tx, mistakeMapper{},
		`WHERE agent_name=? AND grade_term=? ORDER BY created_at,record_id`,
		agentName, profile.GradeTerm)
	if err != nil {
		return snapshot, fmt.Errorf("k12storage: read learning archive mistakes: %w", err)
	}
	snapshot.PracticeSets, err = s.queryRecordsVia(ctx, tx, practiceSetMapper{},
		`WHERE agent_name=? AND grade_term=? ORDER BY created_at,record_id`,
		agentName, profile.GradeTerm)
	if err != nil {
		return snapshot, fmt.Errorf("k12storage: read learning archive practice sets: %w", err)
	}
	snapshot.Accumulations, err = s.queryRecordsVia(ctx, tx, accumMapper{},
		`WHERE agent_name=? AND grade_term=? AND deleted_at IS NULL ORDER BY created_at,record_id`,
		agentName, profile.GradeTerm)
	if err != nil {
		return snapshot, fmt.Errorf("k12storage: read learning archive accumulations: %w", err)
	}
	snapshot.CreativeWorks, err = s.readLearningArchiveCreativeWorksTx(
		ctx, tx, agentName, profile.GradeTerm,
	)
	if err != nil {
		return snapshot, err
	}

	for _, group := range [][]*records.AgentRecord{
		snapshot.Mistakes, snapshot.PracticeSets, snapshot.Accumulations,
	} {
		for _, record := range group {
			schema, schemaErr := s.registry.Get(record.Collection)
			if schemaErr != nil {
				return snapshot, schemaErr
			}
			if schema.ValidateFields != nil {
				if validateErr := schema.ValidateFields(record.Fields); validateErr != nil {
					return snapshot, fmt.Errorf("k12storage: invalid learning archive record %s: %w",
						record.RecordID, validateErr)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return snapshot, fmt.Errorf("k12storage: commit learning archive snapshot: %w", err)
	}
	return snapshot, nil
}

func (s *Store) readLearningArchiveCreativeWorksTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName, gradeTerm string,
) ([]LearningArchiveCreativeWork, error) {
	mapper := creativeWorkMapper{}
	query := fmt.Sprintf(`SELECT %s,%s,initial_feedback_generation_id,
		latest_feedback_generation_id FROM %s
		WHERE agent_name=? AND grade_term=? AND deleted_at IS NULL
		ORDER BY created_at,record_id`, baseCols, strings.Join(mapper.domainCols(), ","), mapper.table())
	rows, err := tx.QueryContext(ctx, query, agentName, gradeTerm)
	if err != nil {
		return nil, fmt.Errorf("k12storage: read learning archive creative works: %w", err)
	}
	type creativeRoot struct {
		record    *records.AgentRecord
		fields    k12.CreativeWorkFields
		initialID string
		latestID  string
	}
	var roots []creativeRoot
	for rows.Next() {
		record := &records.AgentRecord{Collection: k12.CollectionCreativeWork}
		domainDest, finish := mapper.newScan()
		var initialID, latestID string
		dest := append([]any{
			&record.RecordID, &record.AgentName, &record.SchemaVersion, &record.Status,
			&record.DedupeKey, &record.Tags, &record.DueAt, &record.SourceSession,
			&record.Version, &record.CreatedAt, &record.UpdatedAt,
		}, domainDest...)
		dest = append(dest, &initialID, &latestID)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("k12storage: scan learning archive creative work: %w", err)
		}
		fieldsJSON, err := finish()
		if err != nil {
			rows.Close()
			return nil, err
		}
		fields, err := k12.ParseCreativeWorkFields(fieldsJSON)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("k12storage: parse learning archive creative work: %w", err)
		}
		if err := validateCreativeWorkForArchive(fieldsJSON); err != nil {
			rows.Close()
			return nil, err
		}
		record.Fields = fieldsJSON
		roots = append(roots, creativeRoot{record: record, fields: fields, initialID: initialID, latestID: latestID})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	out := make([]LearningArchiveCreativeWork, 0, len(roots))
	for _, root := range roots {
		if root.initialID == "" {
			return nil, fmt.Errorf("k12storage: learning archive creative work %s has no initial generation", root.record.RecordID)
		}
		work := LearningArchiveCreativeWork{Record: root.record, Fields: root.fields}
		initial, err := getWorkFeedbackGenerationVia(ctx, tx, agentName, root.initialID)
		if err != nil {
			return nil, fmt.Errorf("k12storage: read learning archive initial generation: %w", err)
		}
		work.Initial = &initial
		if root.latestID != "" {
			latest, err := getWorkFeedbackGenerationVia(ctx, tx, agentName, root.latestID)
			if err != nil {
				return nil, fmt.Errorf("k12storage: read learning archive latest generation: %w", err)
			}
			work.Latest = &latest
		}
		out = append(out, work)
	}
	return out, nil
}

func weeklyArchiveSectionRank(section string) int {
	switch section {
	case k12.WeeklySectionDueReview:
		return 0
	case k12.WeeklySectionTextbookConsolidation:
		return 1
	case k12.WeeklySectionArithmeticWarmup:
		return 2
	default:
		return 3
	}
}

func validateCreativeWorkForArchive(fieldsJSON string) error {
	fields, err := k12.ParseCreativeWorkFields(fieldsJSON)
	if err != nil {
		return err
	}
	if fields.GradeTerm == "" ||
		(fields.WorkType != k12.WorkTypeWriting && fields.WorkType != k12.WorkTypeArt) {
		return fmt.Errorf("k12storage: invalid current learning archive creative work")
	}
	return nil
}

// FreezeLearningArchiveArtifact 把同一 source digest 收敛到唯一不可变 Artifact。
func (s *Store) FreezeLearningArchiveArtifact(
	ctx context.Context,
	artifact k12.PrintArtifact,
) (stored k12.PrintArtifact, replay bool, retErr error) {
	if artifact.SourceKind != k12.PrintSourceLearningArchive || artifact.ArtifactID == "" ||
		artifact.AgentName == "" || strings.TrimSpace(artifact.SourceRef) == "" ||
		strings.TrimSpace(artifact.Title) == "" || strings.TrimSpace(artifact.CanonicalMarkdown) == "" ||
		len(artifact.SourceDigest) != 64 || artifact.CreatedAt <= 0 {
		return stored, false, fmt.Errorf("k12storage: incomplete learning archive artifact")
	}
	if _, err := hex.DecodeString(artifact.SourceDigest); err != nil ||
		artifact.SourceDigest != strings.ToLower(artifact.SourceDigest) {
		return stored, false, fmt.Errorf("k12storage: invalid learning archive source digest")
	}
	if err := ensureAgentRegistered(ctx, s.db, artifact.AgentName); err != nil {
		return stored, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return stored, false, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil &&
			!errors.Is(rollbackErr, sql.ErrTxDone) && retErr == nil {
			retErr = fmt.Errorf("k12storage: roll back learning archive artifact: %w", rollbackErr)
		}
	}()
	result, err := tx.ExecContext(ctx, `INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		artifact.ArtifactID, artifact.AgentName, artifact.SourceKind, artifact.SourceRef,
		artifact.Title, artifact.CanonicalMarkdown, artifact.SourceDigest, artifact.CreatedAt)
	if err != nil {
		return stored, false, fmt.Errorf("k12storage: freeze learning archive artifact: %w", err)
	}
	stored, err = getPrintArtifactVia(ctx, tx, artifact.AgentName, artifact.ArtifactID)
	if err != nil {
		return stored, false, fmt.Errorf("k12storage: read frozen learning archive artifact: %w", err)
	}
	if stored.AgentName != artifact.AgentName || stored.SourceKind != artifact.SourceKind ||
		stored.SourceRef != artifact.SourceRef || stored.Title != artifact.Title ||
		stored.CanonicalMarkdown != artifact.CanonicalMarkdown || stored.SourceDigest != artifact.SourceDigest {
		return k12.PrintArtifact{}, false, fmt.Errorf("k12storage: learning archive artifact identity conflict")
	}
	affected, _ := result.RowsAffected()
	replay = affected == 0
	if err := tx.Commit(); err != nil {
		return k12.PrintArtifact{}, false, err
	}
	return stored, replay, nil
}
