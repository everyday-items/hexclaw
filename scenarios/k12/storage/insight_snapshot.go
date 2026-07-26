package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// InsightSourceSnapshot is the one read-transaction boundary for every value
// rendered by the learning-insight page and its canonical Markdown projection.
// Only records whose immutable creation-term matches the current profile are
// returned. Legacy blank-term rows remain auditable but are never guessed.
type InsightSourceSnapshot struct {
	Learner             string
	Profile             k12.ChildProfile
	AsOf                int64
	Mistakes            []*records.AgentRecord
	Accumulations       []*records.AgentRecord
	PracticeSets        []*records.AgentRecord
	SourceRecordIDs     []string
	SourceDigest        string
	UnscopedSourceCount int
}

type insightDigestRecord struct {
	Collection string `json:"collection"`
	RecordID   string `json:"record_id"`
	Status     string `json:"status"`
	Version    int    `json:"version"`
	DueAt      *int64 `json:"due_at,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	Fields     string `json:"fields"`
}

func profileFromMetadataJSON(raw string) (k12.ChildProfile, error) {
	meta := map[string]string{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			return k12.ChildProfile{}, fmt.Errorf("k12storage: 解析 agent metadata: %w", err)
		}
	}
	return k12.ProfileFromMeta(meta), nil
}

func readAgentProfileVia(ctx context.Context, q dbQueryer, agentName string) (k12.ChildProfile, error) {
	var metadata string
	if err := q.QueryRowContext(
		ctx,
		`SELECT metadata FROM agents WHERE name = ?`,
		agentName,
	).Scan(&metadata); err != nil {
		if err == sql.ErrNoRows {
			return k12.ChildProfile{}, records.ErrScopeNotFound
		}
		return k12.ChildProfile{}, fmt.Errorf("k12storage: 读取 agent metadata: %w", err)
	}
	return profileFromMetadataJSON(metadata)
}

// AgentGradeTerm reads the current profile term for creation-time attribution.
// It is intentionally separate from report reads; ReadInsightSourceSnapshot is
// the only API allowed to assemble report values.
func (s *Store) AgentGradeTerm(ctx context.Context, agentName string) (string, error) {
	profile, err := readAgentProfileVia(ctx, s.db, agentName)
	if err != nil {
		return "", err
	}
	return profile.GradeTerm, nil
}

// ReadInsightSourceSnapshot freezes profile and all three report source
// collections in one SQLite read transaction. asOf comes from the usecase
// clock and is included in the digest, making repeated reads deterministic.
func (s *Store) ReadInsightSourceSnapshot(
	ctx context.Context,
	agentName string,
	asOf int64,
) (InsightSourceSnapshot, error) {
	if strings.TrimSpace(agentName) == "" {
		return InsightSourceSnapshot{}, records.ErrScopeNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return InsightSourceSnapshot{}, fmt.Errorf("k12storage: 开启学情只读事务: %w", err)
	}
	defer tx.Rollback()

	profile, err := readAgentProfileVia(ctx, tx, agentName)
	if err != nil {
		return InsightSourceSnapshot{}, err
	}
	allMistakes, err := s.queryRecordsVia(
		ctx, tx, mistakeMapper{}, `WHERE agent_name = ? ORDER BY created_at, record_id`, agentName,
	)
	if err != nil {
		return InsightSourceSnapshot{}, err
	}
	allAccumulations, err := s.queryRecordsVia(
		ctx, tx, accumMapper{},
		`WHERE agent_name = ? AND deleted_at IS NULL ORDER BY created_at, record_id`,
		agentName,
	)
	if err != nil {
		return InsightSourceSnapshot{}, err
	}
	allPracticeSets, err := s.queryRecordsVia(
		ctx, tx, practiceSetMapper{}, `WHERE agent_name = ? ORDER BY created_at, record_id`, agentName,
	)
	if err != nil {
		return InsightSourceSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return InsightSourceSnapshot{}, fmt.Errorf("k12storage: 提交学情只读事务: %w", err)
	}

	snapshot := InsightSourceSnapshot{
		Learner: agentName,
		Profile: profile,
		AsOf:    asOf,
	}
	var digestRecords []insightDigestRecord
	appendSelected := func(record *records.AgentRecord) {
		snapshot.SourceRecordIDs = append(snapshot.SourceRecordIDs, record.RecordID)
		digestRecords = append(digestRecords, insightDigestRecord{
			Collection: record.Collection,
			RecordID:   record.RecordID,
			Status:     record.Status,
			Version:    record.Version,
			DueAt:      record.DueAt,
			CreatedAt:  record.CreatedAt,
			UpdatedAt:  record.UpdatedAt,
			Fields:     record.Fields,
		})
	}
	for _, record := range allMistakes {
		fields, parseErr := k12.ParseMistakeFields(record.Fields)
		if parseErr != nil {
			return InsightSourceSnapshot{}, parseErr
		}
		switch {
		case fields.GradeTerm == "":
			snapshot.UnscopedSourceCount++
		case fields.GradeTerm == profile.GradeTerm && profile.GradeTerm != "":
			snapshot.Mistakes = append(snapshot.Mistakes, record)
			appendSelected(record)
		}
	}
	for _, record := range allAccumulations {
		fields, parseErr := k12.ParseAccumFields(record.Fields)
		if parseErr != nil {
			return InsightSourceSnapshot{}, parseErr
		}
		switch {
		case fields.GradeTerm == "":
			snapshot.UnscopedSourceCount++
		case fields.GradeTerm == profile.GradeTerm && profile.GradeTerm != "":
			snapshot.Accumulations = append(snapshot.Accumulations, record)
			appendSelected(record)
		}
	}
	for _, record := range allPracticeSets {
		fields, parseErr := k12.ParsePracticeSetFields(record.Fields)
		if parseErr != nil {
			return InsightSourceSnapshot{}, parseErr
		}
		switch {
		case fields.GradeTerm == "":
			snapshot.UnscopedSourceCount++
		case fields.GradeTerm == profile.GradeTerm && profile.GradeTerm != "":
			snapshot.PracticeSets = append(snapshot.PracticeSets, record)
			appendSelected(record)
		}
	}
	sort.Slice(digestRecords, func(i, j int) bool {
		if digestRecords[i].Collection != digestRecords[j].Collection {
			return digestRecords[i].Collection < digestRecords[j].Collection
		}
		return digestRecords[i].RecordID < digestRecords[j].RecordID
	})
	sort.Strings(snapshot.SourceRecordIDs)
	payload := struct {
		Learner             string                `json:"learner"`
		GradeTerm           string                `json:"grade_term"`
		AsOf                int64                 `json:"as_of"`
		UnscopedSourceCount int                   `json:"unscoped_source_count"`
		Records             []insightDigestRecord `json:"records"`
	}{
		Learner:             snapshot.Learner,
		GradeTerm:           snapshot.Profile.GradeTerm,
		AsOf:                snapshot.AsOf,
		UnscopedSourceCount: snapshot.UnscopedSourceCount,
		Records:             digestRecords,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return InsightSourceSnapshot{}, fmt.Errorf("k12storage: 编码学情快照: %w", err)
	}
	sum := sha256.Sum256(raw)
	snapshot.SourceDigest = hex.EncodeToString(sum[:])
	return snapshot, nil
}
