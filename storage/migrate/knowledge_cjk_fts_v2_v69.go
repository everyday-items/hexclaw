package migrate

// KnowledgeCJKFTSV2V69 installs the durable schema required by the pre-tokenized
// CJK lexical lane. Content backfill intentionally stays in knowledge.SQLiteStore:
// token generation is application logic and its version marker is committed only
// after the complete index has been rebuilt.
var KnowledgeCJKFTSV2V69 = Migration{
	Version:     69,
	Description: "Knowledge CJK bigram FTS v2 schema and index-version ledger",
	SQL: `
CREATE VIRTUAL TABLE IF NOT EXISTS kb_chunks_fts_v2 USING fts5(
    tokens,
    chunk_id UNINDEXED
);

CREATE TABLE IF NOT EXISTS kb_search_index_metadata (
    index_name TEXT PRIMARY KEY,
    version    INTEGER  NOT NULL CHECK(version >= 1),
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- These durable dirty fences survive a rollback to a binary that knows only
-- kb_chunks_fts. A later v2 binary must rebuild instead of trusting stale
-- tokens merely because chunk IDs and row counts still happen to match.
CREATE TRIGGER IF NOT EXISTS kb_chunks_cjk_fts_v2_dirty_insert
AFTER INSERT ON kb_chunks
BEGIN
    DELETE FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts';
END;

CREATE TRIGGER IF NOT EXISTS kb_chunks_cjk_fts_v2_dirty_update
AFTER UPDATE OF id, doc_id, content ON kb_chunks
WHEN OLD.id IS NOT NEW.id OR OLD.doc_id IS NOT NEW.doc_id OR OLD.content IS NOT NEW.content
BEGIN
    DELETE FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts';
END;

CREATE TRIGGER IF NOT EXISTS kb_chunks_cjk_fts_v2_dirty_delete
AFTER DELETE ON kb_chunks
BEGIN
    DELETE FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts';
END;

CREATE TRIGGER IF NOT EXISTS kb_documents_cjk_fts_v2_dirty_lifecycle
AFTER UPDATE OF deleted ON kb_documents
WHEN OLD.deleted IS NOT NEW.deleted
BEGIN
    DELETE FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts';
END;
`,
}
