package migrate

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestE3KbChunksUnique 验证 v0.4.0 E3 收口：kb_chunks 加 (doc_id, chunk_index) UNIQUE 后
// 重复同位置 chunk 不会膨胀，UPSERT 语义生效。
func TestE3KbChunksUnique(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// 跑全部 migration
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 准备 1 条 kb_documents
	if _, err := db.Exec(`INSERT INTO kb_documents (id, title, content, source, source_type, chunk_count, status, deleted, created_at, updated_at)
		VALUES ('doc1', 't', 'c', 's', 'manual', 0, 'indexed', 0, ?, ?)`, time.Now(), time.Now()); err != nil {
		t.Fatalf("insert doc: %v", err)
	}

	// 100 轮 ingestion retry：同 doc 同 index 反复写
	const rounds = 100
	for i := 0; i < rounds; i++ {
		_, err := db.Exec(`INSERT INTO kb_chunks (id, doc_id, content, chunk_index, embedding, created_at)
			VALUES (?, 'doc1', ?, 0, NULL, ?)
			ON CONFLICT(doc_id, chunk_index) DO UPDATE SET
				id = excluded.id, content = excluded.content, created_at = excluded.created_at`,
			"chunk-"+itoa(i), "content "+itoa(i), time.Now())
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}

	// 行数应 = 1（不是 100），证明 UNIQUE 生效
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM kb_chunks WHERE doc_id='doc1'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("100 轮后行数应 = 1（UNIQUE 生效），got %d", count)
	}

	// 内容应被最后一次 UPSERT 覆盖
	var content string
	if err := db.QueryRow("SELECT content FROM kb_chunks WHERE doc_id='doc1'").Scan(&content); err != nil {
		t.Fatalf("query content: %v", err)
	}
	if content != "content "+itoa(rounds-1) {
		t.Errorf("UPSERT 应保留最新 content；got=%q", content)
	}
}

// TestE3KbChunksDifferentIndexes 验证不同 chunk_index 仍可独立存在（UNIQUE 不影响合法多 chunk）。
func TestE3KbChunksDifferentIndexes(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO kb_documents (id, title, content, source, source_type, chunk_count, status, deleted, created_at, updated_at)
		VALUES ('doc2', 't', 'c', 's', 'manual', 0, 'indexed', 0, ?, ?)`, time.Now(), time.Now()); err != nil {
		t.Fatalf("insert doc: %v", err)
	}

	// 50 个不同 chunk_index 的 chunk
	for i := 0; i < 50; i++ {
		_, err := db.Exec(`INSERT INTO kb_chunks (id, doc_id, content, chunk_index, embedding, created_at)
			VALUES (?, 'doc2', ?, ?, NULL, ?)`,
			"c-"+itoa(i), "content "+itoa(i), i, time.Now())
		if err != nil {
			t.Fatalf("insert chunk %d: %v", i, err)
		}
	}

	var count int
	db.QueryRow("SELECT COUNT(*) FROM kb_chunks WHERE doc_id='doc2'").Scan(&count)
	if count != 50 {
		t.Errorf("不同 chunk_index 应保留 50 条；got %d", count)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
