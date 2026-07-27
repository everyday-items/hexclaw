package knowledge

import (
	"context"
	"database/sql"
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

// ErrInvalidEmbeddingResult rejects a provider response that cannot belong to
// the immutable target vector space.
var ErrInvalidEmbeddingResult = errors.New("knowledge: invalid embedding result")

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

// RevisionSemanticReadiness is an optional cheap control-plane probe used by
// Manager before invoking auxiliary query expansion. The scoped lexical route
// remains available when it returns false.
type RevisionSemanticReadiness interface {
	HasActiveRevision(ctx context.Context) (bool, error)
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
	corpusUID string
	revision  string
	profile   EmbeddingProfileSnapshot
}

func (s *SQLiteRevisionSemanticSearcher) loadActivePlan(ctx context.Context) (activeRevisionSearchPlan, bool, error) {
	if s.db == nil || s.registry == nil || strings.TrimSpace(s.ownerID) == "" || strings.TrimSpace(s.corpusID) == "" {
		return activeRevisionSearchPlan{}, false, fmt.Errorf("knowledge: invalid revision semantic search configuration")
	}
	var plan activeRevisionSearchPlan
	var location, availability string
	err := s.db.QueryRowContext(ctx, `SELECT c.corpus_uid,r.revision_id,
		s.profile_snapshot_id,s.resolved_profile_id,s.provider_id,s.provider_name,
		s.provider_location,s.model_name,s.dimension,s.normalization,
		s.chunk_config_hash,s.profile_config_hash,s.availability
		FROM kb_semantic_corpora c
		JOIN kb_index_revisions r
		  ON r.corpus_uid=c.corpus_uid AND r.revision_id=c.active_revision_id
		JOIN kb_embedding_profile_snapshots s
		  ON s.corpus_uid=r.corpus_uid AND s.profile_snapshot_id=r.profile_snapshot_id
		WHERE c.owner_id=? AND c.corpus_alias=? AND r.publish_state='active'`,
		s.ownerID, s.corpusID).Scan(
		&plan.corpusUID, &plan.revision,
		&plan.profile.SnapshotID, &plan.profile.Profile.ProfileID,
		&plan.profile.Profile.ProviderID, &plan.profile.Profile.ProviderName,
		&location, &plan.profile.Profile.ModelName, &plan.profile.Profile.Dimension,
		&plan.profile.Normalization, &plan.profile.ChunkConfigHash,
		&plan.profile.ProfileConfigHash, &availability,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return activeRevisionSearchPlan{}, false, nil
	}
	if err != nil {
		return activeRevisionSearchPlan{}, false, fmt.Errorf("knowledge: load active revision search plan: %w", err)
	}
	plan.profile.Profile.Location = ProviderLocation(location)
	plan.profile.Profile.Availability = ProfileAvailability(availability)
	plan.profile.Profile.Capability = "embedding"
	if err := plan.profile.Validate(); err != nil {
		return activeRevisionSearchPlan{}, false, err
	}
	return plan, true, nil
}

func (s *SQLiteRevisionSemanticSearcher) HasActiveRevision(ctx context.Context) (bool, error) {
	plan, active, err := s.loadActivePlan(ctx)
	if err != nil || !active {
		return active, err
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

// ActiveRevisionID exposes only the control-plane identity needed to freeze a
// multi-query read. It does not execute an embedding request.
func (s *SQLiteRevisionSemanticSearcher) ActiveRevisionID(ctx context.Context) (string, bool, error) {
	plan, active, err := s.loadActivePlan(ctx)
	if err != nil || !active {
		return "", active, err
	}
	return plan.revision, true, nil
}

func (s *SQLiteRevisionSemanticSearcher) Search(
	ctx context.Context,
	query string,
	topK int,
	filter Filter,
) ([]*SearchResult, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, nil
	}
	if topK <= 0 {
		topK = 3
	}
	plan, active, err := s.loadActivePlan(ctx)
	if err != nil || !active {
		return nil, false, err
	}
	executor, err := s.registry.ExecutorForProfile(ctx, plan.profile)
	if err != nil {
		return nil, false, err
	}
	if readiness, ok := executor.(ProfileEmbeddingExecutorReadiness); ok && !readiness.EmbeddingReady(ctx) {
		return nil, false, ErrEmbeddingUnavailable
	}
	var permit *resourcegov.Permit
	if s.governor != nil {
		permit, err = s.governor.Acquire(
			ctx, resourcegov.ResourceAccelerator, resourcegov.PriorityInteractive,
		)
		if err != nil {
			return nil, false, err
		}
	}
	vectors, err := executor.EmbedForPurpose(ctx, EmbeddingPurposeQuery, []string{query})
	if permit != nil {
		permit.Release()
	}
	if err != nil {
		return nil, false, err
	}
	if len(vectors) != 1 || len(vectors[0]) != plan.profile.Profile.Dimension {
		return nil, false, fmt.Errorf("%w: query vectors=%d dimension=%d want=%d",
			ErrInvalidEmbeddingResult, len(vectors), firstVectorDimension(vectors), plan.profile.Profile.Dimension)
	}
	if !embeddingVectorIsFinite(vectors[0]) {
		return nil, false, fmt.Errorf("%w: query vector contains non-finite value", ErrInvalidEmbeddingResult)
	}
	results, err := s.searchActiveVectors(ctx, plan, vectors[0], topK, filter)
	if err != nil {
		return nil, false, err
	}
	return results, true, nil
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
	results, err := s.ftsTextSearch(ctx, corpusUID, keywords, topK, filter)
	if err == nil && len(results) > 0 {
		return results, nil
	}
	return s.likeTextSearch(ctx, corpusUID, keywords, topK, filter)
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
	keywords []string,
	topK int,
	filter Filter,
) ([]*SearchResult, error) {
	clause, filterArgs := buildRevisionFilterClause(filter, "d", "b", "c")
	needDate := filter.hasDateBound()
	query := `SELECT c.id,c.doc_id,d.title,d.source,d.source_type,d.chunk_count,
		c.content,c.chunk_index,c.created_at,COALESCE(c.page_start,0),COALESCE(c.page_end,0),
		c.source_digest,COALESCE(c.source_offset_start,0),COALESCE(c.source_offset_end,0),
		d.created_at,bm25(kb_chunks_fts)
		FROM kb_chunks_fts f
		JOIN kb_chunks c ON c.id=f.chunk_id
		JOIN kb_documents d ON d.id=c.doc_id
		JOIN kb_semantic_document_bindings b
		  ON b.document_id=d.id AND b.corpus_uid=?
		WHERE kb_chunks_fts MATCH ? AND b.lifecycle_state='active' AND d.deleted=0`
	args := []any{corpusUID, strings.Join(keywords, " OR ")}
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
			&chunk.ID, &chunk.DocID, &chunk.DocTitle, &chunk.Source,
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
	keywords []string,
	topK int,
	filter Filter,
) ([]*SearchResult, error) {
	clause, filterArgs := buildRevisionFilterClause(filter, "d", "b", "c")
	needDate := filter.hasDateBound()
	var query strings.Builder
	query.WriteString(`SELECT c.id,c.doc_id,d.title,d.source,d.source_type,d.chunk_count,
		c.content,c.chunk_index,c.created_at,COALESCE(c.page_start,0),COALESCE(c.page_end,0),
		c.source_digest,COALESCE(c.source_offset_start,0),COALESCE(c.source_offset_end,0),d.created_at
		FROM kb_chunks c
		JOIN kb_documents d ON d.id=c.doc_id
		JOIN kb_semantic_document_bindings b
		  ON b.document_id=d.id AND b.corpus_uid=?
		WHERE b.lifecycle_state='active' AND d.deleted=0 AND (`)
	args := []any{corpusUID}
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
			&chunk.ID, &chunk.DocID, &chunk.DocTitle, &chunk.Source,
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
	query := `SELECT v.chunk_id,v.document_id,v.chunk_index,v.embedding,
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
			&chunk.ID, &chunk.DocID, &chunk.Index, &blob,
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
