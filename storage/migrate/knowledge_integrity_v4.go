package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
)

const knowledgeIntegrityAuditDDL = `CREATE TABLE IF NOT EXISTS kb_integrity_audit (
	audit_id TEXT PRIMARY KEY,
	repair_version INTEGER NOT NULL,
	event_kind TEXT NOT NULL,
	survivor_document_id TEXT NOT NULL DEFAULT '',
	record_id TEXT NOT NULL,
	original_key TEXT NOT NULL DEFAULT '',
	payload_json TEXT NOT NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

type knowledgeDocumentV4 struct {
	ID           string
	Title        string
	Content      string
	Source       string
	SourceType   string
	ChunkCount   int64
	Status       string
	Deleted      int64
	ErrorMessage string
	Meta         string
	CreatedAt    any
	UpdatedAt    any
}

type knowledgeChunkV4 struct {
	ID         string
	DocumentID string
	Content    string
	Index      int64
	Embedding  []byte
	CreatedAt  any
}

// migrateKnowledgeUniqueV4 replaces the historical destructive H8 DELETEs.
// Duplicate documents are isolated by source and duplicate chunk positions are
// moved to deterministic free indexes. IDs and payloads remain recoverable from
// the no-FK audit ledger before the uniqueness constraints are installed.
func migrateKnowledgeUniqueV4(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := enableCronForeignKeys(ctx, conn); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, knowledgeIntegrityAuditDDL); err != nil {
		return fmt.Errorf("创建 Knowledge integrity audit: %w", err)
	}
	hasCorpusUID, err := cronColumnExists(ctx, tx, "kb_documents", "corpus_uid")
	if err != nil {
		return fmt.Errorf("检查 kb_documents.corpus_uid: %w", err)
	}
	if !hasCorpusUID {
		if err := isolateKnowledgeDocumentDuplicatesV4(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_documents_unique
			ON kb_documents(source,title) WHERE source<>''`); err != nil {
			return fmt.Errorf("创建 kb_documents legacy 唯一索引: %w", err)
		}
	}
	if err := reindexKnowledgeChunkDuplicatesV4(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_chunks_doc_index
		ON kb_chunks(doc_id,chunk_index)`); err != nil {
		return fmt.Errorf("创建 kb_chunks 唯一索引: %w", err)
	}
	if err := validateCronDatabase(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func isolateKnowledgeDocumentDuplicatesV4(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,title,content,source,source_type,chunk_count,status,deleted,
		error_message,meta,created_at,updated_at FROM kb_documents`)
	if err != nil {
		return fmt.Errorf("读取 Knowledge document 修复候选: %w", err)
	}
	type documentKey struct{ source, title string }
	groups := make(map[documentKey][]knowledgeDocumentV4)
	used := make(map[documentKey]struct{})
	for rows.Next() {
		var doc knowledgeDocumentV4
		if err := rows.Scan(&doc.ID, &doc.Title, &doc.Content, &doc.Source, &doc.SourceType,
			&doc.ChunkCount, &doc.Status, &doc.Deleted, &doc.ErrorMessage, &doc.Meta,
			&doc.CreatedAt, &doc.UpdatedAt); err != nil {
			rows.Close()
			return err
		}
		key := documentKey{source: doc.Source, title: doc.Title}
		used[key] = struct{}{}
		if doc.Source != "" {
			groups[key] = append(groups[key], doc)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	keys := make([]documentKey, 0, len(groups))
	for key, docs := range groups {
		if len(docs) > 1 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].source != keys[j].source {
			return keys[i].source < keys[j].source
		}
		return keys[i].title < keys[j].title
	})
	for _, key := range keys {
		docs := groups[key]
		sort.Slice(docs, func(i, j int) bool {
			return knowledgeRowLess(docs[i].CreatedAt, docs[i].ID, docs[j].CreatedAt, docs[j].ID)
		})
		for _, doc := range docs[1:] {
			base := doc.Source + " · 隔离 · " + doc.ID
			isolatedSource := base
			for suffix := 2; ; suffix++ {
				if _, exists := used[documentKey{source: isolatedSource, title: doc.Title}]; !exists {
					break
				}
				isolatedSource = base + "-" + strconv.Itoa(suffix)
			}
			payload, err := json.Marshal(map[string]any{
				"id": doc.ID, "title": doc.Title, "content": doc.Content, "source": doc.Source,
				"source_type": doc.SourceType, "chunk_count": doc.ChunkCount, "status": doc.Status,
				"deleted": doc.Deleted, "error_message": doc.ErrorMessage, "meta": doc.Meta,
				"created_at": cronJSONValue(doc.CreatedAt), "updated_at": cronJSONValue(doc.UpdatedAt),
				"isolated_source": isolatedSource,
			})
			if err != nil {
				return err
			}
			if err := insertKnowledgeIntegrityAudit(ctx, tx, "document_isolated", docs[0].ID,
				doc.ID, doc.Source+"\x00"+doc.Title, string(payload)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE kb_documents SET source=? WHERE id=?`, isolatedSource, doc.ID); err != nil {
				return fmt.Errorf("隔离 Knowledge document %s: %w", doc.ID, err)
			}
			used[documentKey{source: isolatedSource, title: doc.Title}] = struct{}{}
		}
	}
	return nil
}

func reindexKnowledgeChunkDuplicatesV4(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,doc_id,content,chunk_index,embedding,created_at FROM kb_chunks`)
	if err != nil {
		return fmt.Errorf("读取 Knowledge chunk 修复候选: %w", err)
	}
	type chunkKey struct {
		documentID string
		index      int64
	}
	groups := make(map[chunkKey][]knowledgeChunkV4)
	used := make(map[string]map[int64]struct{})
	maxIndex := make(map[string]int64)
	hasIndex := make(map[string]bool)
	for rows.Next() {
		var chunk knowledgeChunkV4
		if err := rows.Scan(&chunk.ID, &chunk.DocumentID, &chunk.Content, &chunk.Index,
			&chunk.Embedding, &chunk.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		groups[chunkKey{documentID: chunk.DocumentID, index: chunk.Index}] = append(
			groups[chunkKey{documentID: chunk.DocumentID, index: chunk.Index}], chunk)
		if used[chunk.DocumentID] == nil {
			used[chunk.DocumentID] = make(map[int64]struct{})
		}
		used[chunk.DocumentID][chunk.Index] = struct{}{}
		if !hasIndex[chunk.DocumentID] || chunk.Index > maxIndex[chunk.DocumentID] {
			maxIndex[chunk.DocumentID] = chunk.Index
			hasIndex[chunk.DocumentID] = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	keys := make([]chunkKey, 0, len(groups))
	for key, chunks := range groups {
		if len(chunks) > 1 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].documentID != keys[j].documentID {
			return keys[i].documentID < keys[j].documentID
		}
		return keys[i].index < keys[j].index
	})
	for _, key := range keys {
		chunks := groups[key]
		sort.Slice(chunks, func(i, j int) bool {
			return knowledgeRowLess(chunks[i].CreatedAt, chunks[i].ID, chunks[j].CreatedAt, chunks[j].ID)
		})
		for _, chunk := range chunks[1:] {
			isolatedIndex, err := nextKnowledgeChunkIndex(maxIndex[chunk.DocumentID], used[chunk.DocumentID])
			if err != nil {
				return fmt.Errorf("隔离 Knowledge chunk %s: %w", chunk.ID, err)
			}
			payload, err := json.Marshal(map[string]any{
				"id": chunk.ID, "doc_id": chunk.DocumentID, "content": chunk.Content,
				"chunk_index": chunk.Index, "embedding_base64": base64.StdEncoding.EncodeToString(chunk.Embedding),
				"created_at": cronJSONValue(chunk.CreatedAt), "isolated_chunk_index": isolatedIndex,
			})
			if err != nil {
				return err
			}
			if err := insertKnowledgeIntegrityAudit(ctx, tx, "chunk_reindexed", chunk.DocumentID,
				chunk.ID, fmt.Sprintf("%s\x00%d", chunk.DocumentID, chunk.Index), string(payload)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE kb_chunks SET chunk_index=? WHERE id=?`, isolatedIndex, chunk.ID); err != nil {
				return fmt.Errorf("重排 Knowledge chunk %s: %w", chunk.ID, err)
			}
			used[chunk.DocumentID][isolatedIndex] = struct{}{}
			maxIndex[chunk.DocumentID] = isolatedIndex
		}
	}
	return nil
}

func nextKnowledgeChunkIndex(currentMax int64, used map[int64]struct{}) (int64, error) {
	for {
		if currentMax == math.MaxInt64 {
			return 0, errors.New("chunk_index int64 overflow")
		}
		currentMax++
		if _, exists := used[currentMax]; !exists {
			return currentMax, nil
		}
	}
}

func knowledgeRowLess(leftCreated any, leftID string, rightCreated any, rightID string) bool {
	leftTime, leftKnown := normalizedCronCreatedAt(leftCreated)
	rightTime, rightKnown := normalizedCronCreatedAt(rightCreated)
	if leftKnown != rightKnown {
		return leftKnown
	}
	if leftKnown && !leftTime.Equal(rightTime) {
		return leftTime.Before(rightTime)
	}
	return leftID < rightID
}

func insertKnowledgeIntegrityAudit(ctx context.Context, tx *sql.Tx, eventKind, survivorDocumentID,
	recordID, originalKey, payload string,
) error {
	auditID := knowledgeIntegrityAuditID(eventKind, survivorDocumentID, recordID, originalKey)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO kb_integrity_audit
		(audit_id,repair_version,event_kind,survivor_document_id,record_id,original_key,payload_json)
		VALUES (?,4,?,?,?,?,?)`, auditID, eventKind, survivorDocumentID, recordID, originalKey, payload); err != nil {
		return fmt.Errorf("写 Knowledge integrity audit %s: %w", eventKind, err)
	}
	return nil
}

func knowledgeIntegrityAuditID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(strconv.Itoa(len(part))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(part))
	}
	return "kb-v4-" + hex.EncodeToString(h.Sum(nil))
}
