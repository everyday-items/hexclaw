package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
	"github.com/hexagon-codes/hexclaw/resourcegov"
)

var (
	// ErrInvalidEmbeddingResult rejects a provider response that cannot belong to
	// the immutable target vector space.
	ErrInvalidEmbeddingResult = errors.New("knowledge: invalid embedding result")
	// ErrRetrievalPlanUnavailable rejects a caller-pinned revision when the
	// configured semantic route cannot freeze or execute that exact revision.
	// A pinned request must never fall back to the mutable active pointer.
	ErrRetrievalPlanUnavailable = errors.New("knowledge: pinned retrieval plan unavailable")
)

type EmbeddingPurpose string

const (
	EmbeddingPurposeDocument EmbeddingPurpose = "document"
	EmbeddingPurposeQuery    EmbeddingPurpose = "query"
)

// ProfileEmbeddingExecutor is the deliberately small execution boundary shared
// by indexing and query embedding. The explicit purpose preserves asymmetric
// document/query transforms (for example nomic search_document/search_query
// prefixes) inside the immutable profile execution path.
type ProfileEmbeddingExecutor interface {
	EmbedForPurpose(ctx context.Context, purpose EmbeddingPurpose, texts []string) ([][]float32, error)
}

// ProfileEmbeddingExecutorReadiness is optional for executors backed by a
// runtime-gated local service. It lets retrieval remain fully text-only while
// the active model is offline, without probing it through a real embed call.
type ProfileEmbeddingExecutorReadiness interface {
	EmbeddingReady(ctx context.Context) bool
}

// ProfileEmbeddingExecutorRegistry resolves an executor from immutable profile
// facts. It must not auto-fallback to another provider or model.
type ProfileEmbeddingExecutorRegistry interface {
	ExecutorForProfile(ctx context.Context, profile EmbeddingProfileSnapshot) (ProfileEmbeddingExecutor, error)
}

// RevisionSemanticSearcher is the Manager-facing semantic route. routeRan is
// false when the corpus is text-only or has no active revision; this is
// distinct from a provider/search failure.
type RevisionSemanticSearcher interface {
	Search(ctx context.Context, query string, topK int, filter Filter) (results []*SearchResult, routeRan bool, err error)
	TextSearch(ctx context.Context, query string, topK int, filter Filter) ([]*SearchResult, error)
}

// RevisionSemanticReceiptSearcher is the optional evidence-bearing extension
// used by release gates. A receipt exists only after one real query embedding
// and its revision-bound vector scan both succeed.
type RevisionSemanticReceiptSearcher interface {
	SearchWithReceipt(
		ctx context.Context,
		query string,
		topK int,
		filter Filter,
	) (results []*SearchResult, routeRan bool, receipt *QueryEmbeddingReceipt, err error)
}

// revisionSemanticPlanner is the request-scoped immutable retrieval boundary.
// It deliberately remains package-private so legacy/external implementations of
// RevisionSemanticSearcher keep compiling. Manager requires this extension only
// for explicitly pinned revision requests; ordinary reads retain the legacy
// compatibility path.
type revisionSemanticPlanner interface {
	FreezeRetrievalPlan(
		ctx context.Context,
		expectedRevisionID string,
	) (activeRevisionSearchPlan, bool, error)
	RetrievalPlanReady(ctx context.Context, plan activeRevisionSearchPlan) (bool, error)
	ValidateRetrievalPlan(ctx context.Context, plan activeRevisionSearchPlan) error
	SearchWithPlanReceipt(
		ctx context.Context,
		plan activeRevisionSearchPlan,
		query string,
		topK int,
		filter Filter,
	) (results []*SearchResult, routeRan bool, receipt *QueryEmbeddingReceipt, err error)
	TextSearchWithPlan(
		ctx context.Context,
		plan activeRevisionSearchPlan,
		query string,
		topK int,
		filter Filter,
	) ([]*SearchResult, error)
}

type QueryEmbeddingReceipt struct {
	Operation         string `json:"operation"`
	Status            string `json:"status"`
	ProviderID        string `json:"provider_id"`
	ProviderName      string `json:"provider_name,omitempty"`
	Model             string `json:"model"`
	ProfileID         string `json:"profile_id"`
	ProfileConfigHash string `json:"profile_config_hash"`
	Dimension         int    `json:"dimension"`
	RevisionID        string `json:"revision_id"`
	QueryDigest       string `json:"query_digest"`
}

// RevisionSemanticReadiness is an optional cheap control-plane probe used by
// Manager before invoking auxiliary query expansion. The scoped lexical route
// remains available when it returns false.
type RevisionSemanticReadiness interface {
	HasActiveRevision(ctx context.Context) (bool, error)
}

// RevisionSemanticExecutionProfiler resolves model-scoped retrieval policy
// from the active immutable revision. Manager treats it as optional so legacy
// and test searchers retain the existing global defaults.
type RevisionSemanticExecutionProfiler interface {
	EmbeddingExecutionProfile(context.Context) (EmbeddingExecutionProfile, bool, error)
}

// SQLiteRevisionSemanticSearcher embeds the query with the active revision's
// snapshot and scans only vectors belonging to that same revision/vector
// space. It intentionally never reads kb_chunks.embedding.
type SQLiteRevisionSemanticSearcher struct {
	db       *sql.DB
	ownerID  string
	corpusID string
	registry ProfileEmbeddingExecutorRegistry
	governor *resourcegov.Governor
}

type RevisionSearchOption func(*SQLiteRevisionSemanticSearcher)

func WithRevisionSearchResourceGovernor(governor *resourcegov.Governor) RevisionSearchOption {
	return func(searcher *SQLiteRevisionSemanticSearcher) { searcher.governor = governor }
}

func NewSQLiteRevisionSemanticSearcher(
	db *sql.DB,
	ownerID, corpusID string,
	registry ProfileEmbeddingExecutorRegistry,
	options ...RevisionSearchOption,
) *SQLiteRevisionSemanticSearcher {
	searcher := &SQLiteRevisionSemanticSearcher{
		db: db, ownerID: ownerID, corpusID: corpusID, registry: registry,
	}
	for _, option := range options {
		if option != nil {
			option(searcher)
		}
	}
	return searcher
}

type activeRevisionSearchPlan struct {
	corpusUID        string
	revision         string
	explicitRevision bool
	contentVersion   int64
	profile          EmbeddingProfileSnapshot
}

func (s *SQLiteRevisionSemanticSearcher) loadActivePlan(ctx context.Context) (activeRevisionSearchPlan, bool, error) {
	return s.loadRetrievalPlan(ctx, "")
}

func (s *SQLiteRevisionSemanticSearcher) loadRetrievalPlan(
	ctx context.Context,
	expectedRevisionID string,
) (activeRevisionSearchPlan, bool, error) {
	if s.db == nil || s.registry == nil || strings.TrimSpace(s.ownerID) == "" || strings.TrimSpace(s.corpusID) == "" {
		return activeRevisionSearchPlan{}, false, fmt.Errorf("knowledge: invalid revision semantic search configuration")
	}
	expectedRevisionID = strings.TrimSpace(expectedRevisionID)
	var plan activeRevisionSearchPlan
	var location, availability string
	query := `SELECT c.corpus_uid,c.content_version,r.revision_id,
		s.profile_snapshot_id,s.resolved_profile_id,s.provider_id,s.provider_name,
		s.provider_location,s.model_name,s.dimension,s.normalization,
		s.chunk_config_hash,s.profile_config_hash,s.availability
		FROM kb_semantic_corpora c
		JOIN kb_index_revisions r ON r.corpus_uid=c.corpus_uid
		JOIN kb_embedding_profile_snapshots s
		  ON s.corpus_uid=r.corpus_uid AND s.profile_snapshot_id=r.profile_snapshot_id `
	args := []any{s.ownerID, s.corpusID}
	if expectedRevisionID == "" {
		query += `WHERE c.owner_id=? AND c.corpus_alias=?
		  AND r.revision_id=c.active_revision_id AND r.publish_state='active'`
	} else {
		query += `WHERE c.owner_id=? AND c.corpus_alias=? AND r.revision_id=?
		  AND r.publish_state IN ('active','superseded')`
		args = append(args, expectedRevisionID)
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&plan.corpusUID, &plan.contentVersion, &plan.revision,
		&plan.profile.SnapshotID, &plan.profile.Profile.ProfileID,
		&plan.profile.Profile.ProviderID, &plan.profile.Profile.ProviderName,
		&location, &plan.profile.Profile.ModelName, &plan.profile.Profile.Dimension,
		&plan.profile.Normalization, &plan.profile.ChunkConfigHash,
		&plan.profile.ProfileConfigHash, &availability,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if expectedRevisionID != "" {
			return activeRevisionSearchPlan{}, false, fmt.Errorf(
				"%w: revision %q", ErrRetrievalPlanUnavailable, expectedRevisionID,
			)
		}
		return activeRevisionSearchPlan{}, false, nil
	}
	if err != nil {
		return activeRevisionSearchPlan{}, false, fmt.Errorf("knowledge: load active revision search plan: %w", err)
	}
	plan.profile.Profile.Location = ProviderLocation(location)
	plan.profile.Profile.Availability = ProfileAvailability(availability)
	plan.profile.Profile.Capability = "embedding"
	plan.explicitRevision = expectedRevisionID != ""
	if err := plan.profile.Validate(); err != nil {
		return activeRevisionSearchPlan{}, false, err
	}
	return plan, true, nil
}

// FreezeRetrievalPlan resolves the active revision once, or validates an exact
// caller-pinned active/superseded revision. The returned value contains no
// mutable pointers and is reused for every expanded query in one Manager call.
func (s *SQLiteRevisionSemanticSearcher) FreezeRetrievalPlan(
	ctx context.Context,
	expectedRevisionID string,
) (activeRevisionSearchPlan, bool, error) {
	return s.loadRetrievalPlan(ctx, expectedRevisionID)
}

func (s *SQLiteRevisionSemanticSearcher) RetrievalPlanReady(
	ctx context.Context,
	plan activeRevisionSearchPlan,
) (bool, error) {
	if err := validateActiveRevisionSearchPlan(plan); err != nil {
		return false, err
	}
	executor, err := s.registry.ExecutorForProfile(ctx, plan.profile)
	if errors.Is(err, ErrProfileUnavailable) || errors.Is(err, ErrEmbeddingUnavailable) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if readiness, ok := executor.(ProfileEmbeddingExecutorReadiness); ok {
		return readiness.EmbeddingReady(ctx), nil
	}
	return true, nil
}

// ValidateRetrievalPlan closes the request-level TOCTOU window for mutable
// corpus membership. content_version is monotonic, so any document add,
// replacement or tombstone observed during a multi-query request is detected
// before results are returned. Publishing a successor revision alone is
// allowed: the frozen revision remains replayable as superseded.
func (s *SQLiteRevisionSemanticSearcher) ValidateRetrievalPlan(
	ctx context.Context,
	plan activeRevisionSearchPlan,
) error {
	if err := validateActiveRevisionSearchPlan(plan); err != nil {
		return err
	}
	var contentVersion int64
	var publishState string
	err := s.db.QueryRowContext(ctx, `SELECT c.content_version,r.publish_state
		FROM kb_semantic_corpora c
		JOIN kb_index_revisions r ON r.corpus_uid=c.corpus_uid
		WHERE c.corpus_uid=? AND r.revision_id=?`,
		plan.corpusUID,
		plan.revision,
	).Scan(&contentVersion, &publishState)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: frozen revision %q no longer exists",
			ErrRetrievalPlanUnavailable, plan.revision)
	}
	if err != nil {
		return fmt.Errorf("knowledge: validate frozen retrieval plan: %w", err)
	}
	if publishState != "active" && publishState != "superseded" {
		return fmt.Errorf("%w: frozen revision %q state=%q",
			ErrRetrievalPlanUnavailable, plan.revision, publishState)
	}
	if contentVersion != plan.contentVersion {
		return fmt.Errorf(
			"%w: corpus content version changed from %d to %d",
			ErrRetrievalEvidenceConflict,
			plan.contentVersion,
			contentVersion,
		)
	}
	return nil
}

func validateActiveRevisionSearchPlan(plan activeRevisionSearchPlan) error {
	if strings.TrimSpace(plan.corpusUID) == "" || strings.TrimSpace(plan.revision) == "" ||
		plan.contentVersion < 0 {
		return ErrRetrievalPlanUnavailable
	}
	if err := plan.profile.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrRetrievalPlanUnavailable, err)
	}
	return nil
}

func (s *SQLiteRevisionSemanticSearcher) HasActiveRevision(ctx context.Context) (bool, error) {
	plan, active, err := s.loadActivePlan(ctx)
	if err != nil || !active {
		return active, err
	}
	return s.RetrievalPlanReady(ctx, plan)
}

// ActiveRevisionID exposes only the control-plane identity needed to freeze a
// multi-query read. It does not execute an embedding request.
func (s *SQLiteRevisionSemanticSearcher) ActiveRevisionID(ctx context.Context) (string, bool, error) {
	plan, active, err := s.loadActivePlan(ctx)
	if err != nil || !active {
		return "", active, err
	}
	return plan.revision, true, nil
}

func (s *SQLiteRevisionSemanticSearcher) EmbeddingExecutionProfile(
	ctx context.Context,
) (EmbeddingExecutionProfile, bool, error) {
	plan, active, err := s.loadActivePlan(ctx)
	if err != nil || !active {
		return EmbeddingExecutionProfile{}, false, err
	}
	profile, ok := EmbeddingExecutionProfileForModel(plan.profile.Profile.ModelName)
	return profile, ok, nil
}

func (s *SQLiteRevisionSemanticSearcher) Search(
	ctx context.Context,
	query string,
	topK int,
	filter Filter,
) ([]*SearchResult, bool, error) {
	results, routeRan, _, err := s.SearchWithReceipt(ctx, query, topK, filter)
	return results, routeRan, err
}

func (s *SQLiteRevisionSemanticSearcher) SearchWithReceipt(
	ctx context.Context,
	query string,
	topK int,
	filter Filter,
) ([]*SearchResult, bool, *QueryEmbeddingReceipt, error) {
	plan, active, err := s.loadActivePlan(ctx)
	if err != nil || !active {
		return nil, false, nil, err
	}
	return s.SearchWithPlanReceipt(ctx, plan, query, topK, filter)
}

func (s *SQLiteRevisionSemanticSearcher) SearchWithPlanReceipt(
	ctx context.Context,
	plan activeRevisionSearchPlan,
	query string,
	topK int,
	filter Filter,
) ([]*SearchResult, bool, *QueryEmbeddingReceipt, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, nil, nil
	}
	if topK <= 0 {
		topK = 3
	}
	if err := validateActiveRevisionSearchPlan(plan); err != nil {
		return nil, false, nil, err
	}
	executor, err := s.registry.ExecutorForProfile(ctx, plan.profile)
	if err != nil {
		return nil, false, nil, err
	}
	if readiness, ok := executor.(ProfileEmbeddingExecutorReadiness); ok && !readiness.EmbeddingReady(ctx) {
		return nil, false, nil, ErrEmbeddingUnavailable
	}
	var permit *resourcegov.Permit
	if s.governor != nil {
		permit, err = s.governor.Acquire(
			ctx, resourcegov.ResourceAccelerator, resourcegov.PriorityInteractive,
		)
		if err != nil {
			return nil, false, nil, err
		}
	}
	vectors, err := executor.EmbedForPurpose(ctx, EmbeddingPurposeQuery, []string{query})
	if permit != nil {
		permit.Release()
	}
	if err != nil {
		return nil, false, nil, err
	}
	if len(vectors) != 1 || len(vectors[0]) != plan.profile.Profile.Dimension {
		return nil, false, nil, fmt.Errorf("%w: query vectors=%d dimension=%d want=%d",
			ErrInvalidEmbeddingResult, len(vectors), firstVectorDimension(vectors), plan.profile.Profile.Dimension)
	}
	if !embeddingVectorIsFinite(vectors[0]) {
		return nil, false, nil, fmt.Errorf("%w: query vector contains non-finite value", ErrInvalidEmbeddingResult)
	}
	results, err := s.searchActiveVectors(ctx, plan, vectors[0], topK, filter)
	if err != nil {
		return nil, false, nil, err
	}
	sum := sha256.Sum256([]byte(query))
	return results, true, &QueryEmbeddingReceipt{
		Operation:         "query_embedding",
		Status:            "succeeded",
		ProviderID:        plan.profile.Profile.ProviderID,
		ProviderName:      plan.profile.Profile.ProviderName,
		Model:             plan.profile.Profile.ModelName,
		ProfileID:         plan.profile.Profile.ProfileID,
		ProfileConfigHash: plan.profile.ProfileConfigHash,
		Dimension:         plan.profile.Profile.Dimension,
		RevisionID:        plan.revision,
		QueryDigest:       "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}

// TextSearch keeps the lexical fallback inside the same authenticated corpus
// boundary as revision vectors. It remains available when the policy is
// disabled and never consults an embedding provider.
func (s *SQLiteRevisionSemanticSearcher) TextSearch(
	ctx context.Context,
	query string,
	topK int,
	filter Filter,
) ([]*SearchResult, error) {
	keywords := splitter.SearchTokenize(query)
	if len(keywords) == 0 {
		return nil, nil
	}
	if topK <= 0 {
		topK = 3
	}
	corpusUID, err := s.resolveCorpusUID(ctx)
	if err != nil {
		return nil, err
	}
	return s.textSearchCorpus(ctx, corpusUID, "", keywords, topK, filter)
}

func (s *SQLiteRevisionSemanticSearcher) TextSearchWithPlan(
	ctx context.Context,
	plan activeRevisionSearchPlan,
	query string,
	topK int,
	filter Filter,
) ([]*SearchResult, error) {
	if err := validateActiveRevisionSearchPlan(plan); err != nil {
		return nil, err
	}
	keywords := splitter.SearchTokenize(query)
	if len(keywords) == 0 {
		return nil, nil
	}
	if topK <= 0 {
		topK = 3
	}
	revisionID := ""
	if plan.explicitRevision {
		revisionID = plan.revision
	}
	return s.textSearchCorpus(ctx, plan.corpusUID, revisionID, keywords, topK, filter)
}

func (s *SQLiteRevisionSemanticSearcher) textSearchCorpus(
	ctx context.Context,
	corpusUID string,
	revisionID string,
	keywords []string,
	topK int,
	filter Filter,
) ([]*SearchResult, error) {
	results, err := s.ftsTextSearch(ctx, corpusUID, revisionID, keywords, topK, filter)
	if err == nil && len(results) > 0 {
		return results, nil
	}
	return s.likeTextSearch(ctx, corpusUID, revisionID, keywords, topK, filter)
}

func (s *SQLiteRevisionSemanticSearcher) resolveCorpusUID(ctx context.Context) (string, error) {
	if s.db == nil || strings.TrimSpace(s.ownerID) == "" || strings.TrimSpace(s.corpusID) == "" {
		return "", fmt.Errorf("knowledge: invalid corpus text search configuration")
	}
	var corpusUID string
	err := s.db.QueryRowContext(ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id=? AND corpus_alias=?`, s.ownerID, s.corpusID).Scan(&corpusUID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSemanticIndexNotFound
	}
	if err != nil {
		return "", err
	}
	return corpusUID, nil
}

type scopedTextResult struct {
	chunk *Chunk
	score float64
}

func (s *SQLiteRevisionSemanticSearcher) ftsTextSearch(
	ctx context.Context,
	corpusUID string,
	revisionID string,
	keywords []string,
	topK int,
	filter Filter,
) ([]*SearchResult, error) {
	clause, filterArgs := buildRevisionFilterClause(filter, "d", "b", "c")
	needDate := filter.hasDateBound()
	query := `SELECT c.id,c.doc_id,b.content_generation,d.title,d.source,d.source_type,d.chunk_count,
		c.content,c.chunk_index,c.created_at,COALESCE(c.page_start,0),COALESCE(c.page_end,0),
		c.source_digest,COALESCE(c.source_offset_start,0),COALESCE(c.source_offset_end,0),
		d.created_at,bm25(kb_chunks_fts)
		FROM kb_chunks_fts f
		JOIN kb_chunks c ON c.id=f.chunk_id
		JOIN kb_documents d ON d.id=c.doc_id
		JOIN kb_semantic_document_bindings b
		  ON b.document_id=d.id AND b.corpus_uid=?`
	args := []any{corpusUID}
	if revisionID != "" {
		query += ` JOIN kb_revision_documents rd
		  ON rd.corpus_uid=b.corpus_uid AND rd.revision_id=?
		 AND rd.document_id=b.document_id
		 AND rd.content_generation=b.content_generation
		 AND rd.vector_state='ready' AND rd.visible_at IS NOT NULL`
		args = append(args, revisionID)
	}
	query += ` WHERE kb_chunks_fts MATCH ? AND b.lifecycle_state='active' AND d.deleted=0`
	args = append(args, strings.Join(keywords, " OR "))
	query += " AND " + readyIngestSegmentVisibilitySQL("b", "c")
	if clause != "" {
		query += " AND " + clause
		args = append(args, filterArgs...)
	}
	query += " ORDER BY bm25(kb_chunks_fts)"
	if !needDate {
		query += " LIMIT ?"
		args = append(args, topK)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	raw := make([]scopedTextResult, 0, topK)
	minScore, maxScore := math.Inf(1), math.Inf(-1)
	for rows.Next() {
		chunk := &Chunk{}
		var documentCreatedAt time.Time
		var bm25Score float64
		if err := rows.Scan(
			&chunk.ID, &chunk.DocID, &chunk.DocumentGeneration,
			&chunk.DocTitle, &chunk.Source,
			&chunk.SourceType, &chunk.ChunkCount, &chunk.Content,
			&chunk.Index, &chunk.CreatedAt, &chunk.PageStart, &chunk.PageEnd,
			&chunk.SourceDigest, &chunk.SourceOffsetStart, &chunk.SourceOffsetEnd,
			&documentCreatedAt, &bm25Score,
		); err != nil {
			return nil, err
		}
		if needDate && !filter.matchesDate(documentCreatedAt) {
			continue
		}
		score := math.Abs(bm25Score)
		minScore = math.Min(minScore, score)
		maxScore = math.Max(maxScore, score)
		raw = append(raw, scopedTextResult{chunk: chunk, score: score})
		if needDate && len(raw) >= topK {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	results := make([]*SearchResult, 0, len(raw))
	spread := maxScore - minScore
	for _, item := range raw {
		normalized := 1.0
		if spread > 0 {
			normalized = (item.score - minScore) / spread
		}
		results = append(results, &SearchResult{Chunk: item.chunk, TextScore: normalized})
	}
	return results, nil
}

func (s *SQLiteRevisionSemanticSearcher) likeTextSearch(
	ctx context.Context,
	corpusUID string,
	revisionID string,
	keywords []string,
	topK int,
	filter Filter,
) ([]*SearchResult, error) {
	clause, filterArgs := buildRevisionFilterClause(filter, "d", "b", "c")
	needDate := filter.hasDateBound()
	var query strings.Builder
	query.WriteString(`SELECT c.id,c.doc_id,b.content_generation,d.title,d.source,d.source_type,d.chunk_count,
		c.content,c.chunk_index,c.created_at,COALESCE(c.page_start,0),COALESCE(c.page_end,0),
		c.source_digest,COALESCE(c.source_offset_start,0),COALESCE(c.source_offset_end,0),d.created_at
		FROM kb_chunks c
		JOIN kb_documents d ON d.id=c.doc_id
		JOIN kb_semantic_document_bindings b
		  ON b.document_id=d.id AND b.corpus_uid=?`)
	args := []any{corpusUID}
	if revisionID != "" {
		query.WriteString(` JOIN kb_revision_documents rd
		  ON rd.corpus_uid=b.corpus_uid AND rd.revision_id=?
		 AND rd.document_id=b.document_id
		 AND rd.content_generation=b.content_generation
		 AND rd.vector_state='ready' AND rd.visible_at IS NOT NULL`)
		args = append(args, revisionID)
	}
	query.WriteString(` WHERE b.lifecycle_state='active' AND d.deleted=0 AND (`)
	for i, keyword := range keywords {
		if i > 0 {
			query.WriteString(" OR ")
		}
		query.WriteString("c.content LIKE ? ESCAPE '\\'")
		args = append(args, "%"+sqliteutil.EscapeLike(keyword)+"%")
	}
	query.WriteString(")")
	query.WriteString(" AND ")
	query.WriteString(readyIngestSegmentVisibilitySQL("b", "c"))
	if clause != "" {
		query.WriteString(" AND ")
		query.WriteString(clause)
		args = append(args, filterArgs...)
	}
	query.WriteString(" ORDER BY c.id")
	if !needDate {
		query.WriteString(" LIMIT ?")
		args = append(args, topK)
	}
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]*SearchResult, 0, topK)
	for rows.Next() {
		chunk := &Chunk{}
		var documentCreatedAt time.Time
		if err := rows.Scan(
			&chunk.ID, &chunk.DocID, &chunk.DocumentGeneration,
			&chunk.DocTitle, &chunk.Source,
			&chunk.SourceType, &chunk.ChunkCount, &chunk.Content,
			&chunk.Index, &chunk.CreatedAt, &chunk.PageStart, &chunk.PageEnd,
			&chunk.SourceDigest, &chunk.SourceOffsetStart, &chunk.SourceOffsetEnd,
			&documentCreatedAt,
		); err != nil {
			return nil, err
		}
		if needDate && !filter.matchesDate(documentCreatedAt) {
			continue
		}
		matches := 0
		lowerContent := strings.ToLower(chunk.Content)
		for _, keyword := range keywords {
			if strings.Contains(lowerContent, strings.ToLower(keyword)) {
				matches++
			}
		}
		results = append(results, &SearchResult{
			Chunk: chunk, TextScore: float64(matches) / float64(len(keywords)),
		})
		if needDate && len(results) >= topK {
			break
		}
	}
	return results, rows.Err()
}

func firstVectorDimension(vectors [][]float32) int {
	if len(vectors) == 0 {
		return 0
	}
	return len(vectors[0])
}

func embeddingVectorIsFinite(vector []float32) bool {
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}

func (s *SQLiteRevisionSemanticSearcher) searchActiveVectors(
	ctx context.Context,
	plan activeRevisionSearchPlan,
	queryVector []float32,
	topK int,
	filter Filter,
) ([]*SearchResult, error) {
	clause, filterArgs := buildRevisionFilterClause(filter, "d", "b", "c")
	query := `SELECT v.chunk_id,v.document_id,v.content_generation,v.chunk_index,v.embedding,v.chunk_content_hash,
		d.title,d.source,d.source_type,d.chunk_count,c.content,c.created_at,
		COALESCE(c.page_start,0),COALESCE(c.page_end,0),c.source_digest,
		COALESCE(c.source_offset_start,0),COALESCE(c.source_offset_end,0),d.created_at
		FROM kb_revision_vectors v
		JOIN kb_revision_documents rd
		  ON rd.revision_id=v.revision_id AND rd.corpus_uid=v.corpus_uid
		 AND rd.document_id=v.document_id AND rd.content_generation=v.content_generation
		JOIN kb_semantic_document_bindings b
		  ON b.corpus_uid=v.corpus_uid AND b.document_id=v.document_id
		 AND b.content_generation=v.content_generation
		JOIN kb_chunks c ON c.id=v.chunk_id AND c.doc_id=v.document_id
		JOIN kb_documents d ON d.id=v.document_id
		WHERE v.corpus_uid=? AND v.revision_id=? AND v.profile_config_hash=?
		  AND v.dimension=? AND rd.visible_at IS NOT NULL
		  AND rd.vector_state='ready' AND b.lifecycle_state='active' AND d.deleted=0`
	query += " AND " + readyIngestSegmentVisibilitySQL("b", "c")
	args := []any{plan.corpusUID, plan.revision, plan.profile.ProfileConfigHash, plan.profile.Profile.Dimension}
	if clause != "" {
		query += " AND " + clause
		args = append(args, filterArgs...)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("knowledge: scan active revision vectors: %w", err)
	}
	defer rows.Close()

	results := make([]*SearchResult, 0)
	needDate := filter.hasDateBound()
	for scanned := 0; rows.Next(); scanned++ {
		if scanned%1000 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		chunk := &Chunk{}
		var blob []byte
		var documentCreatedAt time.Time
		if err := rows.Scan(
			&chunk.ID, &chunk.DocID, &chunk.DocumentGeneration,
			&chunk.Index, &blob,
			&chunk.CitationDigest,
			&chunk.DocTitle, &chunk.Source, &chunk.SourceType,
			&chunk.ChunkCount, &chunk.Content, &chunk.CreatedAt,
			&chunk.PageStart, &chunk.PageEnd, &chunk.SourceDigest,
			&chunk.SourceOffsetStart, &chunk.SourceOffsetEnd, &documentCreatedAt,
		); err != nil {
			return nil, fmt.Errorf("knowledge: scan active revision vector row: %w", err)
		}
		if needDate && !filter.matchesDate(documentCreatedAt) {
			continue
		}
		chunk.SemanticRevisionID = plan.revision
		chunk.Embedding = decodeFloat32Slice(blob)
		if len(chunk.Embedding) != plan.profile.Profile.Dimension {
			return nil, fmt.Errorf("%w: persisted chunk %q dimension=%d want=%d",
				ErrInvalidEmbeddingResult, chunk.ID, len(chunk.Embedding), plan.profile.Profile.Dimension)
		}
		if !embeddingVectorIsFinite(chunk.Embedding) {
			return nil, fmt.Errorf("%w: persisted chunk %q contains non-finite value",
				ErrInvalidEmbeddingResult, chunk.ID)
		}
		results = append(results, &SearchResult{
			Chunk:       chunk,
			VectorScore: (cosineSimilarity(queryVector, chunk.Embedding) + 1) / 2,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].VectorScore == results[j].VectorScore {
			return results[i].Chunk.ID < results[j].Chunk.ID
		}
		return results[i].VectorScore > results[j].VectorScore
	})
	if len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}
