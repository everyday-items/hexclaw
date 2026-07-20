package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type semanticTestResolver struct {
	profiles      map[string]EmbeddingProfileSnapshot
	autoProfileID string
	calls         int
}

func (r *semanticTestResolver) Resolve(
	_ context.Context,
	_, _ string,
	selection EmbeddingSelection,
) (EmbeddingProfileSnapshot, error) {
	r.calls++
	key := selection.ProfileID
	if selection.Kind == EmbeddingSelectionAuto {
		key = r.autoProfileID
		if key == "" {
			key = "profile-a"
		}
	}
	profile, ok := r.profiles[key]
	if !ok {
		return EmbeddingProfileSnapshot{}, ErrProfileUnavailable
	}
	return profile, nil
}

func (r *semanticTestResolver) Catalog(context.Context, string, string) (EmbeddingProfileCatalog, error) {
	profiles := make([]EmbeddingProfile, 0, len(r.profiles))
	for _, snapshot := range r.profiles {
		profiles = append(profiles, snapshot.Profile)
	}
	recommended := "profile-a"
	return EmbeddingProfileCatalog{
		Profiles: profiles,
		Recommendation: &EmbeddingRecommendation{
			ProfileID:  &recommended,
			ReasonCode: "default",
			ReasonText: "deterministic test recommendation",
		},
		Version: 7,
	}, nil
}

type semanticHarness struct {
	t        *testing.T
	ctx      context.Context
	db       *sql.DB
	repo     *SQLiteSemanticIndexRepository
	resolver *semanticTestResolver
	service  *SemanticIndexService
}

func newSemanticHarness(t *testing.T) *semanticHarness {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "semantic-index.db") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE kb_documents (
		id TEXT PRIMARY KEY, title TEXT NOT NULL, content TEXT NOT NULL, source TEXT NOT NULL DEFAULT '',
		deleted INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE kb_chunks (
		id TEXT PRIMARY KEY, doc_id TEXT NOT NULL, content TEXT NOT NULL,
		chunk_index INTEGER NOT NULL, embedding BLOB
	)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, semanticIndexTestMigrations()); err != nil {
		t.Fatalf("migrate semantic index: %v", err)
	}

	profiles := map[string]EmbeddingProfileSnapshot{
		"profile-a": semanticProfile("profile-a", "ollama", "bge-m3", ProviderLocationLocal, ProfileAvailabilityInstalled, 3, "hash-a"),
		"profile-b": semanticProfile("profile-b", "openai", "text-embedding-3-small", ProviderLocationCloud, ProfileAvailabilityConnected, 4, "hash-b"),
		"profile-c": semanticProfile("profile-c", "ollama", "nomic-embed-text", ProviderLocationLocal, ProfileAvailabilityDownloadable, 5, "hash-c"),
	}
	resolver := &semanticTestResolver{profiles: profiles}
	repo := NewSQLiteSemanticIndexRepository(db)
	return &semanticHarness{
		t: t, ctx: ctx, db: db, repo: repo, resolver: resolver,
		service: NewSemanticIndexService(repo, resolver),
	}
}

func semanticIndexTestMigrations() []migrate.Migration {
	return []migrate.Migration{
		migrate.KnowledgeIndexV23,
		migrate.KnowledgeIngestV24,
		migrate.KnowledgeIngestGenerationsV26,
		migrate.KnowledgeDocumentScopeV27,
		migrate.KnowledgeIngestCheckpointV28,
	}
}

func semanticProfile(
	profileID, providerID, modelName string,
	location ProviderLocation,
	availability ProfileAvailability,
	dimension int,
	configHash string,
) EmbeddingProfileSnapshot {
	return EmbeddingProfileSnapshot{
		Profile: EmbeddingProfile{
			ProfileID: profileID, ModelName: modelName,
			ProviderID: providerID, ProviderName: providerID,
			Location: location, Capability: "embedding", Dimension: dimension,
			Availability: availability,
		},
		Normalization:     "l2",
		ChunkConfigHash:   "chunk-v1",
		ProfileConfigHash: configHash,
	}
}

func TestSemanticIndexBootstrapIsExplicitAndSameSelectionIsNoop(t *testing.T) {
	h := newSemanticHarness(t)

	_, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if !errors.Is(err, ErrSemanticIndexNotFound) {
		t.Fatalf("GetPolicy missing error = %v, want ErrSemanticIndexNotFound", err)
	}
	var corpora int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_semantic_corpora`).Scan(&corpora); err != nil {
		t.Fatal(err)
	}
	if corpora != 0 {
		t.Fatalf("GetPolicy must be read-only, created %d corpus rows", corpora)
	}

	result, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != ApplyPolicyImmediatePublish || result.PolicyVersion != 1 {
		t.Fatalf("bootstrap result = %+v, want immediate publish at version 1", result)
	}
	if result.ActiveRevisionID == nil || result.DesiredRevisionID != nil || result.JobID != nil {
		t.Fatalf("empty corpus bootstrap must be active immediately without desired/job: %+v", result)
	}

	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if projection.Selection.Kind != EmbeddingSelectionAuto || projection.ActiveRevision == nil {
		t.Fatalf("bootstrap left auto without active revision: %+v", projection)
	}
	if projection.ActiveRevision.Profile.ProfileID != "profile-a" || projection.CatalogVersion != 7 {
		t.Fatalf("unexpected active/catalog projection: %+v", projection)
	}
	if projection.IndexingActivity.State != IndexingActivityIdle {
		t.Fatalf("empty active corpus activity = %q, want idle", projection.IndexingActivity.State)
	}

	noOp, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", 1, EmbeddingSelection{Kind: EmbeddingSelectionAuto})
	if err != nil {
		t.Fatal(err)
	}
	if noOp.Branch != ApplyPolicyNoop || noOp.PolicyVersion != 1 || noOp.JobID != nil {
		t.Fatalf("same selection must be a version-stable no-op: %+v", noOp)
	}
}

func TestSemanticIndexStartupPreservesFrozenAutoActiveWhenCatalogPriorityOrAvailabilityChanges(t *testing.T) {
	h := newSemanticHarness(t)
	// Bootstrap with cloud B. A later local installation makes A the resolver's
	// preferred auto candidate, but startup must retain the published snapshot.
	h.resolver.autoProfileID = "profile-b"
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil || boot.ActiveRevisionID == nil {
		t.Fatalf("bootstrap result=%+v err=%v", boot, err)
	}
	resolverCalls := h.resolver.calls
	var revisionsBefore, jobsBefore int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_index_revisions`).Scan(&revisionsBefore); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}

	h.resolver.autoProfileID = "profile-a" // newly installed local model now sorts first
	restarted, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if h.resolver.calls != resolverCalls {
		t.Fatalf("startup re-resolved durable auto: calls=%d, want %d", h.resolver.calls, resolverCalls)
	}
	if restarted.Branch != ApplyPolicyNoop || restarted.PolicyVersion != boot.PolicyVersion ||
		restarted.ActiveRevisionID == nil || *restarted.ActiveRevisionID != *boot.ActiveRevisionID ||
		restarted.DesiredRevisionID != nil || restarted.JobID != nil {
		t.Fatalf("startup changed frozen cloud profile after local install: restarted=%+v boot=%+v", restarted, boot)
	}

	// Even if the old cloud profile temporarily disappears from the live
	// catalog, startup must not resolve/fallback to a different model.
	delete(h.resolver.profiles, "profile-b")
	h.resolver.autoProfileID = "missing-profile"
	unavailableRestart, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatalf("temporary active-profile outage caused startup fallback/error: %v", err)
	}
	if h.resolver.calls != resolverCalls || unavailableRestart.PolicyVersion != boot.PolicyVersion ||
		unavailableRestart.ActiveRevisionID == nil || *unavailableRestart.ActiveRevisionID != *boot.ActiveRevisionID {
		t.Fatalf("temporary outage changed frozen auto execution: %+v", unavailableRestart)
	}
	var revisionsAfter, jobsAfter int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_index_revisions`).Scan(&revisionsAfter); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs`).Scan(&jobsAfter); err != nil {
		t.Fatal(err)
	}
	if revisionsAfter != revisionsBefore || jobsAfter != jobsBefore {
		t.Fatalf("startup created durable work: revisions %d→%d jobs %d→%d",
			revisionsBefore, revisionsAfter, jobsBefore, jobsAfter)
	}
}

func TestSemanticIndexStartupPreservesFrozenAutoDesiredWithoutResolverCall(t *testing.T) {
	h := newSemanticHarness(t)
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_documents(id,title,content,deleted)
		VALUES('doc-desired','Desired','body',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
		VALUES('chunk-desired','doc-desired','body',0,NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	h.resolver.autoProfileID = "profile-b"
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil || boot.DesiredRevisionID == nil || boot.JobID == nil {
		t.Fatalf("bootstrap desired result=%+v err=%v", boot, err)
	}
	resolverCalls := h.resolver.calls
	h.resolver.autoProfileID = "profile-a"
	restarted, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if h.resolver.calls != resolverCalls || restarted.Branch != ApplyPolicyNoop ||
		restarted.PolicyVersion != boot.PolicyVersion || restarted.DesiredRevisionID == nil ||
		*restarted.DesiredRevisionID != *boot.DesiredRevisionID || restarted.JobID == nil ||
		*restarted.JobID != *boot.JobID {
		t.Fatalf("startup replaced frozen desired snapshot/job: restarted=%+v boot=%+v calls=%d→%d",
			restarted, boot, resolverCalls, h.resolver.calls)
	}
}

func TestSemanticIndexStartupDoesNotReplaceFixedProfileIntent(t *testing.T) {
	h := newSemanticHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-a"})
	if err != nil {
		t.Fatal(err)
	}
	changed := semanticProfile("profile-a-v2", "ollama", "bge-m3", ProviderLocationLocal,
		ProfileAvailabilityInstalled, 3, "hash-a-v2")
	h.resolver.profiles["profile-a"] = changed

	reconciled, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Branch != ApplyPolicyNoop || reconciled.PolicyVersion != fixed.PolicyVersion ||
		reconciled.Selection.Kind != EmbeddingSelectionProfile || reconciled.Selection.ProfileID != "profile-a" ||
		reconciled.DesiredRevisionID != nil {
		t.Fatalf("startup auto reconcile overwrote fixed intent: %+v", reconciled)
	}
}

func TestSemanticIndexExplicitDisabledIntentSurvivesDeferredBootstrap(t *testing.T) {
	h := newSemanticHarness(t)
	unavailable := h.resolver.profiles["profile-a"]
	unavailable.Profile.Availability = ProfileAvailabilityDownloadable
	h.resolver.profiles["profile-a"] = unavailable

	if _, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := h.repo.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.PolicyVersion != 0 || bootstrap.Selection.Kind != EmbeddingSelectionDisabled ||
		bootstrap.ActiveRevision != nil || bootstrap.DesiredRevision != nil {
		t.Fatalf("legacy bootstrap policy = %+v, want version 0 disabled without revision pointers", bootstrap)
	}

	if _, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default"); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("unavailable bootstrap error = %v, want ErrProfileUnavailable", err)
	}
	unchanged, err := h.repo.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.PolicyVersion != 0 || unchanged.Selection.Kind != EmbeddingSelectionDisabled ||
		unchanged.ActiveRevision != nil || unchanged.DesiredRevision != nil {
		t.Fatalf("unavailable bootstrap mutated policy: %+v", unchanged)
	}

	disabled, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", 0,
		EmbeddingSelection{Kind: EmbeddingSelectionDisabled})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Branch != ApplyPolicyDisabled || disabled.PolicyVersion != 1 ||
		disabled.ActiveRevisionID != nil || disabled.DesiredRevisionID != nil || disabled.JobID != nil {
		t.Fatalf("first explicit disabled result = %+v, want persisted version 1 disabled intent", disabled)
	}
	repeated, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", disabled.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionDisabled})
	if err != nil || repeated.Branch != ApplyPolicyNoop || repeated.PolicyVersion != disabled.PolicyVersion {
		t.Fatalf("repeated disabled result = %+v err=%v, want version-stable no-op", repeated, err)
	}

	available := unavailable
	available.Profile.Availability = ProfileAvailabilityInstalled
	h.resolver.profiles["profile-a"] = available
	deferred, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if deferred.Branch != ApplyPolicyNoop || deferred.PolicyVersion != disabled.PolicyVersion ||
		deferred.Selection.Kind != EmbeddingSelectionDisabled ||
		deferred.ActiveRevisionID != nil || deferred.DesiredRevisionID != nil {
		t.Fatalf("deferred bootstrap overwrote explicit disabled intent: %+v", deferred)
	}
}

func TestSemanticIndexLegacyBootstrapStagesBackfillInsteadOfPublishingEmptyActive(t *testing.T) {
	h := newSemanticHarness(t)
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_documents(id,title,content,deleted)
		VALUES('legacy-doc','Legacy','legacy body',0)`); err != nil {
		t.Fatal(err)
	}
	for i, content := range []string{"legacy chunk one", "legacy chunk two"} {
		if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
			VALUES(?,?,?,?,NULL)`, "legacy-chunk-"+string(rune('a'+i)), "legacy-doc", content, i); err != nil {
			t.Fatal(err)
		}
	}

	bound, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if bound.Documents != 1 || bound.Chunks != 2 || bound.ContentVersion == 0 {
		t.Fatalf("legacy binding = %+v, want one document/two chunks and non-zero version", bound)
	}
	result, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != ApplyPolicyStagedRebuild || result.ActiveRevisionID != nil ||
		result.DesiredRevisionID == nil || result.JobID == nil {
		t.Fatalf("non-empty legacy corpus must stage backfill, not publish empty active: %+v", result)
	}
	var documents, expected int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*),COALESCE(SUM(expected_chunks),0)
		FROM kb_revision_documents WHERE revision_id=?`, *result.DesiredRevisionID).Scan(&documents, &expected); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || expected != 2 {
		t.Fatalf("revision document manifest = documents:%d chunks:%d, want 1/2", documents, expected)
	}
	now := time.Unix(1_800_000_050, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-activity", now, time.Minute,
	)
	if err != nil || !ok || result.JobID == nil || job.JobID != *result.JobID {
		t.Fatalf("claim desired rebuild: job=%+v ok=%v result=%+v err=%v", job, ok, result, err)
	}
	done, total := int64(1), int64(2)
	if err := h.repo.SaveJobProgress(h.ctx, job.Lease(), now.Add(time.Second), JobProgressUpdate{
		Stage: JobStageEmbedding, ChunksDone: &done, ChunksTotal: &total,
	}); err != nil {
		t.Fatal(err)
	}
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	activity := projection.IndexingActivity
	if activity.State != IndexingActivityBuilding || activity.ProcessingDocuments != 1 ||
		activity.ChunksDone == nil || *activity.ChunksDone != 1 ||
		activity.ChunksTotal == nil || *activity.ChunksTotal != 2 {
		t.Fatalf("desired rebuild activity = %+v, want building with one document and root progress 1/2", activity)
	}
}

func TestSemanticIndexActivityIgnoresFailedSupersededActiveDocumentGeneration(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_800_300_100, 0).UTC()
	stale, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-stale", now, time.Minute,
	)
	if err != nil || !ok {
		t.Fatalf("claim stale generation: job=%+v ok=%v err=%v", stale, ok, err)
	}
	if _, err := h.repo.FailJob(h.ctx, stale.Lease(), now.Add(time.Second), "superseded generation"); err != nil {
		t.Fatal(err)
	}

	doc.Content = "current generation"
	doc.ChunkCount = 1
	doc.UpdatedAt = now.Add(2 * time.Second)
	currentChunks := []*Chunk{{
		ID: "chunk-current", DocID: doc.ID, Content: doc.Content, Index: 0, CreatedAt: doc.UpdatedAt,
	}}
	if err := h.store.Replace(h.ctx, doc, currentChunks); err != nil {
		t.Fatal(err)
	}
	h.runWorker(now.Add(3*time.Second), "worker-current")

	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	activity := projection.IndexingActivity
	if activity.State != IndexingActivityIdle || activity.ProcessingDocuments != 0 ||
		activity.ChunksDone != nil || activity.ChunksTotal != nil {
		t.Fatalf("active current-generation activity = %+v, want idle without stale failed work", activity)
	}
}

func TestSemanticIndexLegacyBindingReconcilesNewDocumentsAfterInitialization(t *testing.T) {
	h := newSemanticHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil || boot.ActiveRevisionID == nil {
		t.Fatalf("empty bootstrap: result=%+v err=%v", boot, err)
	}
	insertLegacy := func(documentID, chunkID, content string) {
		t.Helper()
		if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_documents(id,title,content,deleted)
			VALUES(?,?,?,0)`, documentID, documentID, content); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
			VALUES(?,?,?,0,NULL)`, chunkID, documentID, content); err != nil {
			t.Fatal(err)
		}
	}
	insertLegacy("doc-1", "chunk-1", "alpha")
	first, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentVersion != 1 {
		t.Fatalf("first reconcile content version=%d, want 1", first.ContentVersion)
	}
	var activeJobs, manifests int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND target_revision_id=? AND document_id='doc-1'`,
		*boot.ActiveRevisionID).Scan(&activeJobs); err != nil || activeJobs != 1 {
		t.Fatalf("active incremental jobs=%d err=%v, want 1", activeJobs, err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE revision_id=? AND document_id='doc-1' AND content_generation=1`,
		*boot.ActiveRevisionID).Scan(&manifests); err != nil || manifests != 1 {
		t.Fatalf("active revision document manifests=%d err=%v, want 1", manifests, err)
	}

	again, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default")
	if err != nil || again.ContentVersion != first.ContentVersion {
		t.Fatalf("same-scope reconcile must be idempotent: first=%+v again=%+v err=%v", first, again, err)
	}
	insertLegacy("doc-2", "chunk-2", "beta")
	third, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default")
	if err != nil || third.Documents != 2 || third.ContentVersion != 2 {
		t.Fatalf("new legacy document reconcile: %+v err=%v", third, err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_semantic_document_generations
		WHERE corpus_uid=?`, third.CorpusUID).Scan(&manifests); err != nil || manifests != 2 {
		t.Fatalf("immutable generation facts=%d err=%v, want 2", manifests, err)
	}
}

func TestSemanticIndexLegacyBindingRoutesPastFailedDesiredToActive(t *testing.T) {
	h := newSemanticHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil || boot.ActiveRevisionID == nil {
		t.Fatalf("bootstrap active revision: result=%+v err=%v", boot, err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_documents(id,title,content,deleted)
		VALUES('doc-before-failure','Before failure','alpha',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
		VALUES('chunk-before-failure','doc-before-failure','alpha',0,NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	var activeJobID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT job_id FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND document_id='doc-before-failure'`).Scan(&activeJobID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelJob(h.ctx, "owner-1", activeJobID); err != nil {
		t.Fatalf("clear active document job before staging rebuild: %v", err)
	}

	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.JobID == nil || staged.DesiredRevisionID == nil {
		t.Fatalf("stage desired revision: result=%+v err=%v", staged, err)
	}
	now := time.Unix(1_800_311_000, 0).UTC()
	rebuild, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-fail-before-legacy-reconcile", now.Add(time.Second), time.Minute,
	)
	if err != nil || !ok || rebuild.JobID != *staged.JobID {
		t.Fatalf("claim desired rebuild: job=%+v ok=%v err=%v", rebuild, ok, err)
	}
	if _, err := h.repo.FailJob(h.ctx, rebuild.Lease(), now.Add(2*time.Second), "provider rejected"); err != nil {
		t.Fatal(err)
	}

	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_documents(id,title,content,deleted)
		VALUES('doc-after-failure','After failure','beta',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
		VALUES('chunk-after-failure','doc-after-failure','beta',0,NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default"); err != nil {
		t.Fatalf("reconcile legacy document after desired failure: %v", err)
	}

	var activeJobs, activeRows, abandonedRows int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND target_revision_id=? AND document_id='doc-after-failure'
		  AND document_generation=1 AND state='queued'`, *boot.ActiveRevisionID).Scan(&activeJobs); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE revision_id=? AND document_id='doc-after-failure' AND content_generation=1`,
		*boot.ActiveRevisionID).Scan(&activeRows); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE revision_id=? AND document_id='doc-after-failure'`, *staged.DesiredRevisionID).
		Scan(&abandonedRows); err != nil {
		t.Fatal(err)
	}
	if activeJobs != 1 || activeRows != 1 || abandonedRows != 0 {
		t.Fatalf("terminal desired legacy routing: active jobs=%d active rows=%d abandoned rows=%d",
			activeJobs, activeRows, abandonedRows)
	}
}

func TestSemanticIndexApplyPolicyFiveOutcomes(t *testing.T) {
	t.Run("empty corpus switches active profile atomically", func(t *testing.T) {
		h := newSemanticHarness(t)
		boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
		if err != nil {
			t.Fatal(err)
		}
		got, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
			EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Branch != ApplyPolicyImmediatePublish || got.ActiveRevisionID == nil ||
			*got.ActiveRevisionID == *boot.ActiveRevisionID || got.DesiredRevisionID != nil {
			t.Fatalf("empty corpus switch = %+v, boot=%+v", got, boot)
		}
		var active int
		if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_index_revisions
			WHERE publish_state='active'`).Scan(&active); err != nil || active != 1 {
			t.Fatalf("active revisions=%d err=%v, want exactly one", active, err)
		}
	})

	t.Run("compatible active updates intent without rebuild", func(t *testing.T) {
		h := newSemanticHarness(t)
		boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
		if err != nil {
			t.Fatal(err)
		}
		got, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
			EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-a"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Branch != ApplyPolicyIntentOnly || got.PolicyVersion != 2 || got.JobID != nil {
			t.Fatalf("compatible profile outcome = %+v", got)
		}
		if got.ActiveRevisionID == nil || *got.ActiveRevisionID != *boot.ActiveRevisionID {
			t.Fatalf("compatible intent update changed active revision: boot=%+v got=%+v", boot, got)
		}
	})

	t.Run("installed profile stages rebuild while old active serves", func(t *testing.T) {
		h := newSemanticHarness(t)
		boot, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
		if _, err := h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default"); err != nil {
			t.Fatal(err)
		}
		got, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
			EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Branch != ApplyPolicyStagedRebuild || got.DesiredRevisionID == nil || got.JobID == nil {
			t.Fatalf("staged outcome = %+v", got)
		}
		if got.ActiveRevisionID == nil || *got.ActiveRevisionID != *boot.ActiveRevisionID {
			t.Fatalf("old active must remain during staged rebuild: boot=%+v got=%+v", boot, got)
		}
		job, err := h.service.GetJob(h.ctx, "owner-1", *got.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Kind != KnowledgeJobRebuildRevision || job.State != KnowledgeJobQueued {
			t.Fatalf("unexpected rebuild job: %+v", job)
		}
		projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
		if err != nil {
			t.Fatal(err)
		}
		if projection.ActiveRevision == nil || projection.DesiredRevision == nil ||
			projection.IndexingActivity.State != IndexingActivityBuilding {
			t.Fatalf("staged projection lost active/desired/activity: %+v", projection)
		}

		reused, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", got.PolicyVersion,
			EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
		if err != nil {
			t.Fatal(err)
		}
		if reused.Branch != ApplyPolicyNoop || reused.PolicyVersion != got.PolicyVersion ||
			reused.JobID == nil || *reused.JobID != *got.JobID {
			t.Fatalf("healthy desired must reuse revision/job without version bump: first=%+v replay=%+v", got, reused)
		}
	})

	t.Run("downloadable profile requires independent installation before policy apply", func(t *testing.T) {
		h := newSemanticHarness(t)
		boot, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
		_, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
			EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-c"})
		if !errors.Is(err, ErrProfileUnavailable) {
			t.Fatalf("downloadable apply error=%v, want ErrProfileUnavailable", err)
		}
		projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
		if err != nil {
			t.Fatal(err)
		}
		var downloadJobs int
		if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
			WHERE kind='download_model'`).Scan(&downloadJobs); err != nil {
			t.Fatal(err)
		}
		if projection.PolicyVersion != boot.PolicyVersion || projection.DesiredRevision != nil ||
			downloadJobs != 0 {
			t.Fatalf("downloadable apply mutated policy=%+v downloadJobs=%d", projection, downloadJobs)
		}
	})

	t.Run("disabled clears revisions without resolving provider", func(t *testing.T) {
		h := newSemanticHarness(t)
		boot, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
		callsBefore := h.resolver.calls
		got, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
			EmbeddingSelection{Kind: EmbeddingSelectionDisabled})
		if err != nil {
			t.Fatal(err)
		}
		if got.Branch != ApplyPolicyDisabled || got.ActiveRevisionID != nil || got.DesiredRevisionID != nil || got.JobID != nil {
			t.Fatalf("disabled outcome = %+v", got)
		}
		if h.resolver.calls != callsBefore {
			t.Fatal("disabled selection must not resolve or call an embedding provider")
		}
		noOp, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", got.PolicyVersion,
			EmbeddingSelection{Kind: EmbeddingSelectionDisabled})
		if err != nil || noOp.Branch != ApplyPolicyNoop || noOp.PolicyVersion != got.PolicyVersion {
			t.Fatalf("repeated disabled must be no-op: result=%+v err=%v", noOp, err)
		}
	})
}

func TestSemanticIndexFailedDesiredCanBeExplicitlyCancelled(t *testing.T) {
	h := newSemanticHarness(t)
	boot, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	_, _ = h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default")
	staged, _ := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	now := time.Unix(1_800_000_500, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-1", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, err := h.repo.FailJob(h.ctx, job.Lease(), now.Add(time.Second), "provider_rejected"); err != nil {
		t.Fatal(err)
	}
	failed, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if failed.DesiredRevision == nil || failed.DesiredRevision.State != VectorIndexFailed ||
		failed.DesiredRevision.JobID == nil || *failed.DesiredRevision.JobID != *staged.JobID {
		t.Fatalf("failed desired must retain its actionable root job id: %+v", failed.DesiredRevision)
	}
	cancelled, err := h.service.CancelJob(h.ctx, "owner-1", *staged.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != KnowledgeJobFailed || !cancelled.CancelRequested {
		t.Fatalf("failed job audit state must be retained and marked cancelled-by-user: %+v", cancelled)
	}
	after, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if after.PolicyVersion != staged.PolicyVersion+1 || after.DesiredRevision != nil ||
		after.ActiveRevision == nil || after.ActiveRevision.RevisionID != *boot.ActiveRevisionID ||
		after.Selection.Kind != EmbeddingSelectionAuto {
		t.Fatalf("failed desired cancellation did not restore prior policy/active: %+v", after)
	}
	repeated, err := h.service.CancelJob(h.ctx, "owner-1", *staged.JobID)
	if err != nil {
		t.Fatal(err)
	}
	repeatedPolicy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if repeated.State != KnowledgeJobFailed || !repeated.CancelRequested ||
		repeated.LeaseEpoch != cancelled.LeaseEpoch || !repeated.UpdatedAt.Equal(cancelled.UpdatedAt) ||
		repeatedPolicy.PolicyVersion != after.PolicyVersion {
		t.Fatalf("repeated failed cancellation was not idempotent: first=%+v second=%+v policy=%d->%d",
			cancelled, repeated, after.PolicyVersion, repeatedPolicy.PolicyVersion)
	}
}

func TestSemanticIndexCancelSupersedingDesiredRestoresLastPublishedPolicy(t *testing.T) {
	for _, tt := range []struct {
		name             string
		failFirstDesired bool
	}{
		{name: "queued desired"},
		{name: "failed desired", failFirstDesired: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSemanticHarness(t)
			boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
			if err != nil || boot.ActiveRevisionID == nil {
				t.Fatalf("bootstrap active revision: result=%+v err=%v", boot, err)
			}
			if _, err := h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default"); err != nil {
				t.Fatal(err)
			}
			firstDesired, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
				EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
			if err != nil || firstDesired.JobID == nil || firstDesired.DesiredRevisionID == nil {
				t.Fatalf("stage first desired revision: result=%+v err=%v", firstDesired, err)
			}
			if tt.failFirstDesired {
				now := time.Unix(1_800_312_000, 0).UTC()
				job, ok, err := h.repo.ClaimNextJobForCorpus(
					h.ctx, "owner-1", "default", "worker-fail-first-desired", now, time.Minute,
				)
				if err != nil || !ok || job.JobID != *firstDesired.JobID {
					t.Fatalf("claim first desired revision: job=%+v ok=%v err=%v", job, ok, err)
				}
				if _, err := h.repo.FailJob(h.ctx, job.Lease(), now.Add(time.Second), "provider rejected"); err != nil {
					t.Fatal(err)
				}
			}
			profileC := h.resolver.profiles["profile-c"]
			profileC.Profile.Availability = ProfileAvailabilityInstalled
			h.resolver.profiles["profile-c"] = profileC
			secondDesired, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", firstDesired.PolicyVersion,
				EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-c"})
			if err != nil || secondDesired.JobID == nil || secondDesired.DesiredRevisionID == nil {
				t.Fatalf("supersede desired revision: result=%+v err=%v", secondDesired, err)
			}
			if _, err := h.service.CancelJob(h.ctx, "owner-1", *secondDesired.JobID); err != nil {
				t.Fatalf("cancel superseding desired revision: %v", err)
			}

			afterCancel, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
			if err != nil {
				t.Fatal(err)
			}
			if afterCancel.PolicyVersion != secondDesired.PolicyVersion+1 ||
				afterCancel.Selection.Kind != EmbeddingSelectionAuto ||
				afterCancel.ActiveRevision == nil || afterCancel.ActiveRevision.RevisionID != *boot.ActiveRevisionID ||
				afterCancel.DesiredRevision != nil {
				t.Fatalf("cancelled superseding desired did not restore last published policy: %+v", afterCancel)
			}

			restarted := NewSemanticIndexService(h.repo, h.resolver)
			reconciled, err := restarted.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
			if err != nil {
				t.Fatalf("restart policy reconciliation: %v", err)
			}
			if reconciled.PolicyVersion != afterCancel.PolicyVersion || reconciled.Branch != ApplyPolicyNoop ||
				reconciled.Selection.Kind != EmbeddingSelectionAuto || reconciled.ActiveRevisionID == nil ||
				*reconciled.ActiveRevisionID != *boot.ActiveRevisionID || reconciled.DesiredRevisionID != nil {
				t.Fatalf("restart did not preserve converged active policy: result=%+v afterCancel=%+v", reconciled, afterCancel)
			}
		})
	}
}

func TestSemanticIndexConcurrentReplacementAndCancelConverge(t *testing.T) {
	const rounds = 8
	for round := 0; round < rounds; round++ {
		t.Run(fmt.Sprintf("round-%02d", round), func(t *testing.T) {
			h := newSemanticHarness(t)
			boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
			if err != nil || boot.ActiveRevisionID == nil {
				t.Fatalf("bootstrap active revision: result=%+v err=%v", boot, err)
			}
			if _, err := h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default"); err != nil {
				t.Fatal(err)
			}
			firstDesired, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
				EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
			if err != nil || firstDesired.JobID == nil || firstDesired.DesiredRevisionID == nil {
				t.Fatalf("stage B: result=%+v err=%v", firstDesired, err)
			}

			now := time.Unix(1_800_320_000+int64(round*100), 0).UTC()
			claimedB, ok, err := h.repo.ClaimNextJobForCorpus(
				h.ctx, "owner-1", "default", fmt.Sprintf("worker-b-%d", round), now, time.Minute,
			)
			if err != nil || !ok || claimedB.JobID != *firstDesired.JobID {
				t.Fatalf("claim B: job=%+v ok=%v err=%v", claimedB, ok, err)
			}
			leaseB := claimedB.Lease()
			if err := h.repo.PrepareRevisionForPublish(h.ctx, leaseB, now.Add(time.Second), RevisionPublishPreparation{
				IndexedThroughVersion: 1, ChunkSetDigest: "empty-b", ExpectedChunks: 0,
			}); err != nil {
				t.Fatalf("prepare B: %v", err)
			}

			profileC := h.resolver.profiles["profile-c"]
			profileC.Profile.Availability = ProfileAvailabilityInstalled
			h.resolver.profiles["profile-c"] = profileC
			type outcome struct {
				name  string
				apply ApplyPolicyResult
				job   KnowledgeJob
				err   error
			}
			ready := make(chan struct{}, 2)
			start := make(chan struct{})
			results := make(chan outcome, 2)
			go func() {
				ready <- struct{}{}
				<-start
				applied, applyErr := h.service.ApplyPolicy(h.ctx, "owner-1", "default", firstDesired.PolicyVersion,
					EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-c"})
				results <- outcome{name: "apply-c", apply: applied, err: applyErr}
			}()
			go func() {
				ready <- struct{}{}
				<-start
				cancelled, cancelErr := h.service.CancelJob(h.ctx, "owner-1", *firstDesired.JobID)
				results <- outcome{name: "cancel-b", job: cancelled, err: cancelErr}
			}()
			<-ready
			<-ready
			close(start)

			var applyOutcome, cancelOutcome outcome
			for i := 0; i < 2; i++ {
				got := <-results
				if got.name == "apply-c" {
					applyOutcome = got
				} else {
					cancelOutcome = got
				}
			}
			if cancelOutcome.err != nil {
				t.Fatalf("cancel B lost with unexpected error: %v", cancelOutcome.err)
			}
			if cancelOutcome.job.State != KnowledgeJobCancelled || !cancelOutcome.job.CancelRequested {
				t.Fatalf("cancel B did not return its idempotent terminal state: %+v", cancelOutcome.job)
			}
			if applyOutcome.err != nil && !errors.Is(applyOutcome.err, ErrPolicyVersionConflict) {
				t.Fatalf("apply C error=%v, want success or ErrPolicyVersionConflict", applyOutcome.err)
			}

			policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
			if err != nil {
				t.Fatal(err)
			}
			if policy.ActiveRevision == nil || policy.ActiveRevision.RevisionID != *boot.ActiveRevisionID {
				t.Fatalf("replacement/cancel changed serving A before publish: %+v", policy)
			}
			if applyOutcome.err == nil {
				if policy.Selection.Kind != EmbeddingSelectionProfile || policy.Selection.ProfileID != "profile-c" ||
					policy.DesiredRevision == nil || applyOutcome.apply.DesiredRevisionID == nil ||
					policy.DesiredRevision.RevisionID != *applyOutcome.apply.DesiredRevisionID {
					t.Fatalf("apply C winner did not converge to desired C: apply=%+v policy=%+v",
						applyOutcome.apply, policy)
				}
				var previousKind string
				var previousProfile sql.NullString
				if err := h.db.QueryRowContext(h.ctx, `SELECT previous_selection_kind,previous_selected_profile_id
					FROM kb_index_revisions WHERE revision_id=?`, *applyOutcome.apply.DesiredRevisionID).
					Scan(&previousKind, &previousProfile); err != nil {
					t.Fatal(err)
				}
				if previousKind != string(EmbeddingSelectionAuto) || previousProfile.Valid {
					t.Fatalf("C rollback snapshot=%s/%v, want last published auto", previousKind, previousProfile)
				}
			} else if policy.Selection.Kind != EmbeddingSelectionAuto || policy.DesiredRevision != nil {
				t.Fatalf("cancel B winner did not converge to A: %+v", policy)
			}

			jobB, err := h.service.GetJob(h.ctx, "owner-1", *firstDesired.JobID)
			if err != nil || jobB.State != KnowledgeJobCancelled || !jobB.CancelRequested {
				t.Fatalf("B job not durably fenced: job=%+v err=%v", jobB, err)
			}
			var revisionBState string
			if err := h.db.QueryRowContext(h.ctx, `SELECT publish_state FROM kb_index_revisions WHERE revision_id=?`,
				*firstDesired.DesiredRevisionID).Scan(&revisionBState); err != nil || revisionBState != "abandoned" {
				t.Fatalf("B revision state=%q err=%v, want abandoned", revisionBState, err)
			}
			if err := h.repo.SaveJobProgress(h.ctx, leaseB, now.Add(2*time.Second),
				JobProgressUpdate{Stage: JobStagePublishing}); !errors.Is(err, ErrJobFenced) {
				t.Fatalf("late B progress error=%v, want ErrJobFenced", err)
			}
			if err := h.repo.PublishRevisionCAS(h.ctx, PublishRevisionCommand{
				Lease: leaseB, Now: now.Add(2 * time.Second), OwnerID: "owner-1", CorpusID: "default",
				RevisionID: *firstDesired.DesiredRevisionID, ExpectedPolicyVersion: firstDesired.PolicyVersion,
				ExpectedActiveRevisionID: boot.ActiveRevisionID, ExpectedContentVersion: 1,
			}); !errors.Is(err, ErrPublishConflict) {
				t.Fatalf("late B publish error=%v, want ErrPublishConflict", err)
			}
		})
	}
}

func TestSemanticIndexConcurrentCancelAndPublishReplacementHaveOneWinner(t *testing.T) {
	const rounds = 8
	for round := 0; round < rounds; round++ {
		t.Run(fmt.Sprintf("round-%02d", round), func(t *testing.T) {
			h := newSemanticHarness(t)
			boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
			if err != nil || boot.ActiveRevisionID == nil {
				t.Fatalf("bootstrap active revision: result=%+v err=%v", boot, err)
			}
			if _, err := h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default"); err != nil {
				t.Fatal(err)
			}
			firstDesired, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
				EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
			if err != nil || firstDesired.JobID == nil || firstDesired.DesiredRevisionID == nil {
				t.Fatalf("stage B: result=%+v err=%v", firstDesired, err)
			}
			now := time.Unix(1_800_330_000+int64(round*100), 0).UTC()
			claimedB, ok, err := h.repo.ClaimNextJobForCorpus(
				h.ctx, "owner-1", "default", fmt.Sprintf("worker-old-b-%d", round), now, time.Minute,
			)
			if err != nil || !ok || claimedB.JobID != *firstDesired.JobID {
				t.Fatalf("claim B: job=%+v ok=%v err=%v", claimedB, ok, err)
			}
			leaseB := claimedB.Lease()

			profileC := h.resolver.profiles["profile-c"]
			profileC.Profile.Availability = ProfileAvailabilityInstalled
			h.resolver.profiles["profile-c"] = profileC
			secondDesired, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", firstDesired.PolicyVersion,
				EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-c"})
			if err != nil || secondDesired.JobID == nil || secondDesired.DesiredRevisionID == nil {
				t.Fatalf("replace B with C: result=%+v err=%v", secondDesired, err)
			}
			if err := h.repo.SaveJobProgress(h.ctx, leaseB, now.Add(time.Second),
				JobProgressUpdate{Stage: JobStagePublishing}); !errors.Is(err, ErrJobFenced) {
				t.Fatalf("replacement did not fence B lease: %v", err)
			}
			var previousKind string
			var previousProfile sql.NullString
			if err := h.db.QueryRowContext(h.ctx, `SELECT previous_selection_kind,previous_selected_profile_id
				FROM kb_index_revisions WHERE revision_id=?`, *secondDesired.DesiredRevisionID).
				Scan(&previousKind, &previousProfile); err != nil {
				t.Fatal(err)
			}
			if previousKind != string(EmbeddingSelectionAuto) || previousProfile.Valid {
				t.Fatalf("C rollback snapshot=%s/%v, want last published auto", previousKind, previousProfile)
			}

			claimedC, ok, err := h.repo.ClaimNextJobForCorpus(
				h.ctx, "owner-1", "default", fmt.Sprintf("worker-c-%d", round), now.Add(2*time.Second), time.Minute,
			)
			if err != nil || !ok || claimedC.JobID != *secondDesired.JobID {
				t.Fatalf("claim C: job=%+v ok=%v err=%v", claimedC, ok, err)
			}
			leaseC := claimedC.Lease()
			if err := h.repo.PrepareRevisionForPublish(h.ctx, leaseC, now.Add(3*time.Second), RevisionPublishPreparation{
				IndexedThroughVersion: 1, ChunkSetDigest: "empty-c", ExpectedChunks: 0,
			}); err != nil {
				t.Fatalf("prepare C: %v", err)
			}

			type outcome struct {
				name string
				job  KnowledgeJob
				err  error
			}
			ready := make(chan struct{}, 2)
			start := make(chan struct{})
			results := make(chan outcome, 2)
			go func() {
				ready <- struct{}{}
				<-start
				cancelled, cancelErr := h.service.CancelJob(h.ctx, "owner-1", *secondDesired.JobID)
				results <- outcome{name: "cancel-c", job: cancelled, err: cancelErr}
			}()
			go func() {
				ready <- struct{}{}
				<-start
				publishErr := h.repo.PublishRevisionCAS(h.ctx, PublishRevisionCommand{
					Lease: leaseC, Now: now.Add(4 * time.Second), OwnerID: "owner-1", CorpusID: "default",
					RevisionID: *secondDesired.DesiredRevisionID, ExpectedPolicyVersion: secondDesired.PolicyVersion,
					ExpectedActiveRevisionID: boot.ActiveRevisionID, ExpectedContentVersion: 1,
				})
				results <- outcome{name: "publish-c", err: publishErr}
			}()
			<-ready
			<-ready
			close(start)

			var cancelOutcome, publishOutcome outcome
			for i := 0; i < 2; i++ {
				got := <-results
				if got.name == "cancel-c" {
					cancelOutcome = got
				} else {
					publishOutcome = got
				}
			}
			if cancelOutcome.err != nil {
				t.Fatalf("cancel C error=%v", cancelOutcome.err)
			}
			if publishOutcome.err != nil && !errors.Is(publishOutcome.err, ErrPublishConflict) &&
				!errors.Is(publishOutcome.err, ErrJobFenced) {
				t.Fatalf("publish C error=%v, want success or fenced conflict", publishOutcome.err)
			}

			policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
			if err != nil {
				t.Fatal(err)
			}
			if policy.DesiredRevision != nil {
				t.Fatalf("cancel/publish winner left desired revision: %+v", policy)
			}
			jobC, err := h.service.GetJob(h.ctx, "owner-1", *secondDesired.JobID)
			if err != nil {
				t.Fatal(err)
			}
			if cancelOutcome.job.State != jobC.State || cancelOutcome.job.LeaseEpoch != jobC.LeaseEpoch {
				t.Fatalf("cancel did not return converged terminal job: returned=%+v stored=%+v", cancelOutcome.job, jobC)
			}
			var wantActive, wantRevisionState string
			if publishOutcome.err == nil {
				wantActive, wantRevisionState = *secondDesired.DesiredRevisionID, "active"
				if policy.Selection.Kind != EmbeddingSelectionProfile || policy.Selection.ProfileID != "profile-c" ||
					policy.ActiveRevision == nil || policy.ActiveRevision.RevisionID != wantActive ||
					jobC.State != KnowledgeJobSucceeded {
					t.Fatalf("publish C winner did not converge: policy=%+v job=%+v", policy, jobC)
				}
			} else {
				wantActive, wantRevisionState = *boot.ActiveRevisionID, "abandoned"
				if policy.Selection.Kind != EmbeddingSelectionAuto || policy.ActiveRevision == nil ||
					policy.ActiveRevision.RevisionID != wantActive || jobC.State != KnowledgeJobCancelled {
					t.Fatalf("cancel C winner did not restore A: policy=%+v job=%+v", policy, jobC)
				}
			}
			var activeCount int
			if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_index_revisions
				WHERE corpus_uid=? AND publish_state='active'`, claimedC.CorpusUID).Scan(&activeCount); err != nil || activeCount != 1 {
				t.Fatalf("active revision count=%d err=%v, want exactly one", activeCount, err)
			}
			var revisionCState string
			if err := h.db.QueryRowContext(h.ctx, `SELECT publish_state FROM kb_index_revisions WHERE revision_id=?`,
				*secondDesired.DesiredRevisionID).Scan(&revisionCState); err != nil || revisionCState != wantRevisionState {
				t.Fatalf("C revision state=%q err=%v, want %q", revisionCState, err, wantRevisionState)
			}
			if err := h.repo.SaveJobProgress(h.ctx, leaseC, now.Add(5*time.Second),
				JobProgressUpdate{Stage: JobStagePublishing}); !errors.Is(err, ErrJobFenced) {
				t.Fatalf("winner did not fence old C lease: %v", err)
			}
			if err := h.repo.PublishRevisionCAS(h.ctx, PublishRevisionCommand{
				Lease: leaseC, Now: now.Add(5 * time.Second), OwnerID: "owner-1", CorpusID: "default",
				RevisionID: *secondDesired.DesiredRevisionID, ExpectedPolicyVersion: secondDesired.PolicyVersion,
				ExpectedActiveRevisionID: boot.ActiveRevisionID, ExpectedContentVersion: 1,
			}); !errors.Is(err, ErrPublishConflict) {
				t.Fatalf("late C publish error=%v, want ErrPublishConflict", err)
			}

			restarted := NewSemanticIndexService(h.repo, h.resolver)
			reconciled, err := restarted.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
			if err != nil || reconciled.ActiveRevisionID == nil || *reconciled.ActiveRevisionID != wantActive ||
				reconciled.DesiredRevisionID != nil || reconciled.PolicyVersion != policy.PolicyVersion {
				t.Fatalf("restart changed converged winner: result=%+v policy=%+v err=%v", reconciled, policy, err)
			}
		})
	}
}

func TestSemanticIndexPublishPreservesStorageErrors(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "missing-semantic-schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := NewSQLiteSemanticIndexRepository(db)
	err = repo.PublishRevisionCAS(context.Background(), PublishRevisionCommand{
		Lease: JobLease{
			JobID: "job-c", OwnerID: "owner-1", CorpusUID: "corpus-1", WorkerID: "worker-c", Epoch: 1,
		},
		Now: time.Unix(1_800_340_000, 0).UTC(), OwnerID: "owner-1", CorpusID: "default", RevisionID: "revision-c",
	})
	if err == nil || errors.Is(err, ErrPublishConflict) {
		t.Fatalf("publish storage error=%v, want original storage error", err)
	}
}

func TestSemanticIndexCancelFencesLateWorkerAndRestoresPreviousPolicy(t *testing.T) {
	h := newSemanticHarness(t)
	boot, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	_, _ = h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default")
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	claimed, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-1", now, time.Minute)
	if err != nil || !ok || claimed.JobID != *staged.JobID {
		t.Fatalf("claim staged job: job=%+v ok=%v err=%v", claimed, ok, err)
	}
	lease := claimed.Lease()
	chunksDone, chunksTotal := int64(3), int64(10)
	if err := h.repo.SaveJobProgress(h.ctx, lease, now.Add(time.Second), JobProgressUpdate{
		Stage: JobStageEmbedding, ChunksDone: &chunksDone, ChunksTotal: &chunksTotal,
	}); err != nil {
		t.Fatal(err)
	}

	cancelled, err := h.service.CancelJob(h.ctx, "owner-1", claimed.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != KnowledgeJobCancelled || cancelled.LeaseEpoch <= lease.Epoch {
		t.Fatalf("cancel did not advance fencing epoch: before=%+v after=%+v", claimed, cancelled)
	}
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if projection.PolicyVersion != staged.PolicyVersion+1 || projection.Selection.Kind != EmbeddingSelectionAuto ||
		projection.ActiveRevision == nil || projection.ActiveRevision.RevisionID != *boot.ActiveRevisionID ||
		projection.DesiredRevision != nil {
		t.Fatalf("cancel must restore prior intent and old active: %+v", projection)
	}

	if err := h.repo.SaveJobProgress(h.ctx, lease, now.Add(2*time.Second), JobProgressUpdate{Stage: JobStagePublishing}); !errors.Is(err, ErrJobFenced) {
		t.Fatalf("late progress error = %v, want ErrJobFenced", err)
	}
	if err := h.repo.PublishRevisionCAS(h.ctx, PublishRevisionCommand{
		Lease: lease, Now: now.Add(2 * time.Second), OwnerID: "owner-1", CorpusID: "default",
		RevisionID: *staged.DesiredRevisionID, ExpectedPolicyVersion: staged.PolicyVersion,
		ExpectedActiveRevisionID: boot.ActiveRevisionID, ExpectedContentVersion: 1,
	}); !errors.Is(err, ErrPublishConflict) {
		t.Fatalf("late publish error = %v, want ErrPublishConflict", err)
	}
	if _, err := h.service.GetJob(h.ctx, "owner-2", claimed.JobID); !errors.Is(err, ErrSemanticIndexNotFound) {
		t.Fatalf("cross-owner GetJob error = %v, want not found", err)
	}
}

func TestSemanticIndexCancelClearsRetryScheduleBeforeCancelledTransition(t *testing.T) {
	h := newSemanticHarness(t)
	boot, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	_, _ = h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default")
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.JobID == nil {
		t.Fatalf("stage desired: result=%+v err=%v", staged, err)
	}
	now := time.Unix(1_800_000_700, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-retry-cancel", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim desired: job=%+v ok=%v err=%v", job, ok, err)
	}
	if _, err := h.repo.RetryJob(h.ctx, job.Lease(), now.Add(time.Second), now.Add(time.Minute), "temporary"); err != nil {
		t.Fatal(err)
	}
	cancelled, err := h.service.CancelJob(h.ctx, "owner-1", *staged.JobID)
	if err != nil {
		t.Fatalf("cancel retry_wait job: %v", err)
	}
	if cancelled.State != KnowledgeJobCancelled || cancelled.NextAttemptAt != nil {
		t.Fatalf("cancelled retry job=%+v", cancelled)
	}
}

func TestSemanticIndexActiveCompatibleIntentClearsDesiredRetrySchedule(t *testing.T) {
	h := newSemanticHarness(t)
	boot, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	_, _ = h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default")
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.JobID == nil {
		t.Fatalf("stage desired: result=%+v err=%v", staged, err)
	}
	now := time.Unix(1_800_000_800, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-retry-intent", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim desired: job=%+v ok=%v err=%v", job, ok, err)
	}
	if _, err := h.repo.RetryJob(h.ctx, job.Lease(), now.Add(time.Second), now.Add(time.Minute), "temporary"); err != nil {
		t.Fatal(err)
	}
	restored, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", staged.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionAuto})
	if err != nil {
		t.Fatalf("restore active-compatible intent from retry_wait: %v", err)
	}
	if restored.Branch != ApplyPolicyIntentOnly || restored.DesiredRevisionID != nil {
		t.Fatalf("restored intent=%+v", restored)
	}
	cancelled, err := h.service.GetJob(h.ctx, "owner-1", *staged.JobID)
	if err != nil || cancelled.State != KnowledgeJobCancelled || cancelled.NextAttemptAt != nil {
		t.Fatalf("abandoned retry job=%+v err=%v", cancelled, err)
	}
}

func TestSemanticIndexPublishCASKeepsOldActiveUntilCommit(t *testing.T) {
	h := newSemanticHarness(t)
	boot, _ := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	_, _ = h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default")
	staged, _ := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	now := time.Unix(1_800_000_100, 0).UTC()
	claimed, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-1", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	lease := claimed.Lease()
	if err := h.repo.PrepareRevisionForPublish(h.ctx, lease, now.Add(time.Second), RevisionPublishPreparation{
		IndexedThroughVersion: 1, ChunkSetDigest: "empty-test-digest", ExpectedChunks: 0,
	}); err != nil {
		t.Fatal(err)
	}

	bad := PublishRevisionCommand{
		Lease: lease, Now: now.Add(2 * time.Second), OwnerID: "owner-1", CorpusID: "default",
		RevisionID: *staged.DesiredRevisionID, ExpectedPolicyVersion: staged.PolicyVersion,
		ExpectedActiveRevisionID: boot.ActiveRevisionID, ExpectedContentVersion: 2,
	}
	if err := h.repo.PublishRevisionCAS(h.ctx, bad); !errors.Is(err, ErrPublishConflict) {
		t.Fatalf("bad CAS error = %v, want ErrPublishConflict", err)
	}
	before, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if before.ActiveRevision == nil || before.ActiveRevision.RevisionID != *boot.ActiveRevisionID || before.DesiredRevision == nil {
		t.Fatalf("failed CAS changed active/desired: %+v", before)
	}

	bad.ExpectedContentVersion = 1
	if err := h.repo.PublishRevisionCAS(h.ctx, bad); err != nil {
		t.Fatalf("publish staged revision: %v", err)
	}
	after, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveRevision == nil || after.ActiveRevision.RevisionID != *staged.DesiredRevisionID || after.DesiredRevision != nil {
		t.Fatalf("successful CAS did not atomically switch active: %+v", after)
	}
	job, _ := h.service.GetJob(h.ctx, "owner-1", claimed.JobID)
	if job.State != KnowledgeJobSucceeded {
		t.Fatalf("published job state = %q, want succeeded", job.State)
	}
}

func TestSemanticIndexPublishRejectsExpiredLeaseBeforeReclaim(t *testing.T) {
	h := newSemanticHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.RecordCorpusContentChange(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_200, 0).UTC()
	claimed, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-expired", now, time.Minute,
	)
	if err != nil || !ok {
		t.Fatalf("claim: job=%+v ok=%v err=%v", claimed, ok, err)
	}
	lease := claimed.Lease()
	if err := h.repo.PrepareRevisionForPublish(h.ctx, lease, now.Add(time.Second), RevisionPublishPreparation{
		IndexedThroughVersion: 1, ChunkSetDigest: "empty-expired-digest", ExpectedChunks: 0,
	}); err != nil {
		t.Fatal(err)
	}

	before, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	jobBefore, err := h.service.GetJob(h.ctx, "owner-1", claimed.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var revisionStateBefore string
	var revisionEpochBefore int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT publish_state,lease_epoch
		FROM kb_index_revisions WHERE revision_id=?`, *staged.DesiredRevisionID).Scan(
		&revisionStateBefore, &revisionEpochBefore,
	); err != nil {
		t.Fatal(err)
	}
	err = h.repo.PublishRevisionCAS(h.ctx, PublishRevisionCommand{
		Lease: lease, OwnerID: "owner-1", CorpusID: "default",
		RevisionID: *staged.DesiredRevisionID, ExpectedPolicyVersion: staged.PolicyVersion,
		ExpectedActiveRevisionID: boot.ActiveRevisionID, ExpectedContentVersion: 1,
		Now: now.Add(time.Minute + time.Millisecond),
	})
	if !errors.Is(err, ErrJobFenced) {
		t.Fatalf("expired publish error = %v, want ErrJobFenced", err)
	}
	after, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if after.PolicyVersion != before.PolicyVersion || after.ActiveRevision == nil ||
		before.ActiveRevision == nil || after.ActiveRevision.RevisionID != before.ActiveRevision.RevisionID ||
		after.DesiredRevision == nil || before.DesiredRevision == nil ||
		after.DesiredRevision.RevisionID != before.DesiredRevision.RevisionID {
		t.Fatalf("expired publish changed policy projection: before=%+v after=%+v", before, after)
	}
	job, err := h.service.GetJob(h.ctx, "owner-1", claimed.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != KnowledgeJobRunning || job.LeaseEpoch != lease.Epoch ||
		job.LeaseOwner != lease.WorkerID || !job.UpdatedAt.Equal(jobBefore.UpdatedAt) {
		t.Fatalf("expired publish mutated job: before=%+v after=%+v", jobBefore, job)
	}
	var revisionStateAfter string
	var revisionEpochAfter int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT publish_state,lease_epoch
		FROM kb_index_revisions WHERE revision_id=?`, *staged.DesiredRevisionID).Scan(
		&revisionStateAfter, &revisionEpochAfter,
	); err != nil {
		t.Fatal(err)
	}
	if revisionStateBefore != "staged" || revisionStateAfter != revisionStateBefore ||
		revisionEpochAfter != revisionEpochBefore {
		t.Fatalf("expired publish mutated revision: before=%s/%d after=%s/%d",
			revisionStateBefore, revisionEpochBefore, revisionStateAfter, revisionEpochAfter)
	}
}

func TestSemanticIndexClaimIsCorpusScopedAndExpiredLeaseFencesCheckpoint(t *testing.T) {
	h := newSemanticHarness(t)
	for _, ownerID := range []string{"owner-1", "owner-2"} {
		boot, err := h.service.EnsureDefaultPolicy(h.ctx, ownerID, "default")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.repo.RecordCorpusContentChange(h.ctx, ownerID, "default"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.service.ApplyPolicy(h.ctx, ownerID, "default", boot.PolicyVersion,
			EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Unix(1_800_001_000, 0).UTC()
	first, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-1", now, time.Minute)
	if err != nil || !ok || first.OwnerID != "owner-1" {
		t.Fatalf("owner-1 claim: job=%+v ok=%v err=%v", first, ok, err)
	}
	renewed, err := h.repo.RenewJobLease(h.ctx, first.Lease(), now.Add(30*time.Second), 2*time.Minute)
	if err != nil || renewed.Epoch != first.LeaseEpoch || !renewed.ExpiresAt.After(*first.LeaseExpiresAt) {
		t.Fatalf("renew lease: renewed=%+v err=%v", renewed, err)
	}
	if other, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-extra", now, time.Minute); err != nil || ok {
		t.Fatalf("owner-1 must not steal owner-2 queued work: job=%+v ok=%v err=%v", other, ok, err)
	}
	ownerTwo, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-2", "default", "worker-2", now, time.Minute)
	if err != nil || !ok || ownerTwo.OwnerID != "owner-2" {
		t.Fatalf("owner-2 claim: job=%+v ok=%v err=%v", ownerTwo, ok, err)
	}

	checkpoint := StageCheckpoint{
		Stage: JobStageEmbedding, InputFingerprint: "input-v1",
		ArtifactRef: "local://manifest-1", ArtifactDigest: "digest-1",
		State: StageCheckpointSucceeded,
	}
	if err := h.repo.SaveStageCheckpoint(h.ctx, first.Lease(), now.Add(time.Second), checkpoint); err != nil {
		t.Fatal(err)
	}
	persisted, err := h.repo.GetStageCheckpoint(h.ctx, "owner-1", first.JobID, JobStageEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.InputFingerprint != checkpoint.InputFingerprint || persisted.LeaseEpoch != first.LeaseEpoch {
		t.Fatalf("persisted checkpoint = %+v, want %+v", persisted, checkpoint)
	}

	reclaimed, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-restart", now.Add(3*time.Minute), time.Minute,
	)
	if err != nil || !ok || reclaimed.JobID != first.JobID || reclaimed.LeaseEpoch <= first.LeaseEpoch {
		t.Fatalf("expired lease reclaim: first=%+v reclaimed=%+v ok=%v err=%v", first, reclaimed, ok, err)
	}
	late := StageCheckpoint{
		Stage: JobStagePublishing, InputFingerprint: "late",
		ArtifactDigest: "late", State: StageCheckpointSucceeded,
	}
	if err := h.repo.SaveStageCheckpoint(h.ctx, first.Lease(), now.Add(3*time.Minute+time.Second), late); !errors.Is(err, ErrJobFenced) {
		t.Fatalf("old lease checkpoint error = %v, want ErrJobFenced", err)
	}
}
