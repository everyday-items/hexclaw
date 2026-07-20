package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// KnowledgeDocumentScopeV27 moves document identity from the pre-v0.5 global
// (source,title) namespace into the durable semantic corpus namespace. The
// column remains nullable only for legacy rows that have not yet been claimed
// by BindLegacyDefaultCorpus.
var KnowledgeDocumentScopeV27 = Migration{
	Version:     27,
	Description: "v0.5.0 Knowledge 文档持有 corpus UID 与作用域内活跃唯一约束",
	Func:        migrateKnowledgeDocumentScopeV27,
}

func migrateKnowledgeDocumentScopeV27(ctx context.Context, db *sql.DB) error {
	hasCorpusUID, err := columnExists(ctx, db, "kb_documents", "corpus_uid")
	if err != nil {
		return fmt.Errorf("检查 kb_documents.corpus_uid: %w", err)
	}
	if !hasCorpusUID {
		if _, err := db.ExecContext(ctx, `ALTER TABLE kb_documents ADD COLUMN corpus_uid TEXT`); err != nil {
			return fmt.Errorf("新增 kb_documents.corpus_uid: %w", err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// A partially migrated/manual database must never silently relabel a row
	// already assigned to a different corpus than its immutable binding.
	var mismatches int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM kb_documents d
		JOIN kb_semantic_document_bindings b ON b.document_id=d.id
		WHERE d.corpus_uid IS NOT NULL AND d.corpus_uid<>''
		  AND d.corpus_uid<>b.corpus_uid`).Scan(&mismatches); err != nil {
		return fmt.Errorf("校验 Knowledge 文档 corpus 归属: %w", err)
	}
	if mismatches != 0 {
		return fmt.Errorf("Knowledge 文档 corpus 归属冲突: %d rows", mismatches)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE kb_documents
		SET corpus_uid=(
			SELECT b.corpus_uid FROM kb_semantic_document_bindings b
			WHERE b.document_id=kb_documents.id
		)
		WHERE (corpus_uid IS NULL OR corpus_uid='')
		  AND EXISTS (
			SELECT 1 FROM kb_semantic_document_bindings b
			WHERE b.document_id=kb_documents.id
		  )`); err != nil {
		return fmt.Errorf("回填 Knowledge 文档 corpus 归属: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_kb_documents_unique`); err != nil {
		return fmt.Errorf("移除 Knowledge 全局文档唯一约束: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_documents_unique_scoped
		ON kb_documents(corpus_uid,source,title)
		WHERE corpus_uid IS NOT NULL AND corpus_uid<>'' AND source<>'' AND deleted=0`); err != nil {
		return fmt.Errorf("创建 Knowledge corpus 活跃唯一约束: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_documents_unique_legacy
		ON kb_documents(source,title)
		WHERE (corpus_uid IS NULL OR corpus_uid='') AND source<>'' AND deleted=0`); err != nil {
		return fmt.Errorf("创建 Knowledge legacy 活跃唯一约束: %w", err)
	}
	return tx.Commit()
}
