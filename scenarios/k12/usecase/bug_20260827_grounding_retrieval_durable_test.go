package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"database/sql"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type durableGroundingRetrievalProbe struct {
	calls int
}

func (p *durableGroundingRetrievalProbe) GroundSnapshotWithEvidence(
	_ context.Context, snapshot GroundingSnapshot, query, _ string,
) (GroundingSnapshotResult, error) {
	p.calls++
	querySum := sha256.Sum256([]byte(query))
	citationSum := sha256.Sum256([]byte("citation"))
	return GroundingSnapshotResult{
		Text: "教材中的两位数加法依据", Found: true,
		Receipts: []GroundingEvidenceReceipt{{
			TextbookBindingID: snapshot.TextbookBindingID, TextbookManifestID: snapshot.TextbookManifestID,
			DocumentID: snapshot.DocumentID, DocumentGeneration: snapshot.DocumentGeneration,
			VectorRevisionID: snapshot.VectorRevisionID, QueryDigest: "sha256:" + hex.EncodeToString(querySum[:]),
			ChunkID: "segment-1", LogicalPage: 1, PDFPage: 1, SourceDigest: snapshot.SourceDigest,
			CitationDigest: hex.EncodeToString(citationSum[:]),
		}},
	}, nil
}

func TestGroundingRetrievalInvocationIsReusedAfterSessionRestart(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	store := k12storage.NewStore(db, nil)
	snapshot := GroundingSnapshot{
		AgentName: "child", LearnerID: "learner", Subject: "数学",
		TextbookBindingID: "binding", TextbookManifestID: "manifest", DocumentID: "document",
		DocumentGeneration: 1, SourceDigest: strings.Repeat("a", 64), Edition: "人教版", Volume: "下册",
		SegmentRefs:      []string{"segment-1"},
		PageRefs:         []k12.TextbookGroundingPageRef{{LogicalPage: 1, PDFPage: 1, SegmentRefs: []string{"segment-1"}}},
		VectorRevisionID: "revision-1",
	}
	q := RecognizedQuestion{ProblemID: "problem-1", AttemptID: "attempt-1", ConfirmedVersion: 1, InputDigest: "input-1", Question: "57+38="}
	req := GradeRequest{Subject: "数学", Grade: "五年级下", KnowledgePoints: []string{"两位数加法"}}
	firstProbe := &durableGroundingRetrievalProbe{}
	first := &gradingGroundingSession{
		required: true, snapshot: snapshot, evidenceSource: firstProbe, retrieval: store,
		ownerID: "owner", agentName: "child", jobID: "job-1", items: make(map[string]*gradingGroundingItemState),
	}
	firstEvidence, err := first.resolveItem(context.Background(), q, req)
	if err != nil {
		t.Fatal(err)
	}
	if firstProbe.calls != 1 {
		t.Fatalf("first retrieval calls=%d want 1", firstProbe.calls)
	}
	secondProbe := &durableGroundingRetrievalProbe{}
	second := &gradingGroundingSession{
		required: true, snapshot: snapshot, evidenceSource: secondProbe, retrieval: k12storage.NewStore(db, nil),
		ownerID: "owner", agentName: "child", jobID: "job-1", items: make(map[string]*gradingGroundingItemState),
	}
	secondEvidence, err := second.resolveItem(context.Background(), q, req)
	if err != nil {
		t.Fatal(err)
	}
	if secondProbe.calls != 0 {
		t.Fatalf("restarted retrieval calls=%d want 0", secondProbe.calls)
	}
	if secondEvidence.text != firstEvidence.text || secondEvidence.identityDigest != firstEvidence.identityDigest {
		t.Fatalf("restarted evidence drifted: first=%+v second=%+v", firstEvidence, secondEvidence)
	}
}
