package k12storage_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestCommitPracticeGeneration_CommittedReplayIgnoresStaleCandidateBasket(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	fields := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceMixed,
		Title:      "待打印篮",
		Items: []k12.PracticeItem{{
			ItemID: "item-stable", Subject: "数学", AddedVia: k12.PracticeAddedViaCustom,
			QuestionMarkdown: "1+1=?", ExpectedAnswerMarkdown: "2",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
		}},
	}
	rec, err := k12.NewPracticeSetRecord("mingming", "session-1", fields)
	if err != nil {
		t.Fatal(err)
	}
	job := k12.PracticeGenerationJob{
		GenerationJobID: "pgen-stable", AgentName: "mingming", IdempotencyKey: "same-command",
		RequestDigest: "same-digest", Scope: "week", VariantsPerSource: 1,
		Difficulty: "same", Total: "5", Textbook: "人教版",
		Status: k12.PracticeGenerationCommitted, ResultItemIDs: []string{"item-stable"},
		CreatedAt: 100, UpdatedAt: 100,
	}
	committed, replay, err := store.CommitPracticeGeneration(ctx, rec, -1, job)
	if err != nil || replay {
		t.Fatalf("首次提交: replay=%v err=%v", replay, err)
	}

	// 并发调用可能在首次提交前读过幂等收据、却在提交后才读到篮子；它会基于
	// 过期命令快照再次拼入同一批确定性 item ID。committed 收据已是唯一真相，
	// 存储层必须先收敛到它，不能让输家的候选篮校验覆盖幂等结果。
	staleFields, err := k12.ParsePracticeSetFields(committed.Fields)
	if err != nil {
		t.Fatal(err)
	}
	staleFields.Items = append(staleFields.Items, staleFields.Items[0])
	raw, err := json.Marshal(staleFields)
	if err != nil {
		t.Fatal(err)
	}
	committed.Fields = string(raw)

	got, replay, err := store.CommitPracticeGeneration(ctx, committed, committed.Version, job)
	if err != nil {
		t.Fatalf("committed 幂等重放不得校验输家的过期候选篮: %v", err)
	}
	if !replay || got.RecordID != committed.RecordID {
		t.Fatalf("重放必须返回已提交集合: replay=%v got=%+v", replay, got)
	}
}
