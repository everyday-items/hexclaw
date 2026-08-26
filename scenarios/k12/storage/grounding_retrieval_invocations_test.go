package k12storage

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

func TestGroundingRetrievalInvocationClaimAndSuccessSurviveRestartWithoutDuplicate(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil)
	claim := GroundingRetrievalInvocationClaim{
		OwnerID: "owner", AgentName: "child", JobID: "job-1", ProblemID: "problem-1",
		Operation: "k12_grounding_retrieval", GroundingSnapshotDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		QueryDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		DocumentID:  "doc-1", DocumentGeneration: 1, RevisionID: "revision-1",
		ProfileConfigHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ScopeDigest:       "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Provider:          "hexclaw-gpt", Model: "gpt-5.6-luna",
	}
	first, err := store.ClaimGroundingRetrievalInvocation(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Fresh || first.Status != GroundingRetrievalInvocationStatusRunning || first.InvocationID == "" {
		t.Fatalf("first claim=%+v", first)
	}
	second, err := NewStore(db, nil).ClaimGroundingRetrievalInvocation(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fresh || second.InvocationID != first.InvocationID {
		t.Fatalf("duplicate retrieval claim=%+v first=%+v", second, first)
	}
	result := GroundingRetrievalInvocationResult{
		ResultJSON:         `{"text":"教材命中","found":true}`,
		QueryReceiptDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		HitSetDigest:       "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		CitationSetDigest:  "1111111111111111111111111111111111111111111111111111111111111111",
		Provider:           claim.Provider,
		Model:              claim.Model,
		RevisionID:         claim.RevisionID,
		ProfileConfigHash:  claim.ProfileConfigHash,
	}
	if err := store.SaveGroundingRetrievalInvocation(ctx, first, result); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewStore(db, nil).ClaimGroundingRetrievalInvocation(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Fresh || restarted.Status != GroundingRetrievalInvocationStatusSucceeded ||
		restarted.ResultJSON != result.ResultJSON || restarted.HitSetDigest != result.HitSetDigest {
		t.Fatalf("restart did not reuse retrieval result=%+v", restarted)
	}
	if err := store.SaveGroundingRetrievalInvocation(ctx, first, result); err != nil {
		t.Fatalf("same result must be idempotent: %v", err)
	}
}
