package recall

import (
	"context"
	"testing"
)

// BUG-20260712-L：真机取证——问「1+1=2?」，记忆召回命中「大大阿达大大阿达」（词法零重叠、
// 无向量证据），前端渲染无关「记忆命中」卡，模型上下文被垃圾记忆污染。
//
// 机械路径：MinScore=0（桌面默认无 embedding → engine 置 0 防漏召）时，
// `rel < r.MinScore` 即 `0 < 0` 为假——**relevance=0 的候选照样通过**。
// 零证据（BM25=0 且无向量分）不是「低相关」而是「无相关」，不应受“地板关闭”豁免。
//
// 契约：CuratedRetriever 无条件剔除 relevance=0 的候选；有任何词法/语义证据（rel>0）
// 的行为不变（保住 S2「花生酱→花生过敏」低分真命中场景）。
func TestBug20260712_ZeroEvidenceCandidatesDropped(t *testing.T) {
	items := []Candidate{
		{ // 零证据垃圾：与 query 词法零重叠、无向量
			Entry:     Entry{ID: "garbage", UserID: "u1", Type: TypeFact, Content: "大大阿达大大阿达"},
			BM25Score: 0,
		},
		{ // 低分但有词法证据（S2 场景）：必须保留
			Entry:     Entry{ID: "weak-real", UserID: "u1", Type: TypeFact, Content: "孩子花生过敏"},
			BM25Score: 0.12,
		},
	}
	r := newRetriever(items, 0 /* MinScore=0：桌面无 embedding 常态 */, 5)
	got, err := r.Retrieve(context.Background(), "u1", "", "花生酱能吃吗")
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := ids(got)
	if contains(gotIDs, "garbage") {
		t.Fatalf("零证据候选（rel=0）不得通过召回（MinScore=0 只是关地板，不是免证据）：%v", gotIDs)
	}
	if !contains(gotIDs, "weak-real") {
		t.Fatalf("低分真命中（S2 花生酱场景）被误杀：%v", gotIDs)
	}
}

// 全部候选零证据 → 返回空（供上游注入层「空即不注入」，而非兜底把垃圾端给模型）。
func TestBug20260712_AllZeroEvidenceReturnsEmpty(t *testing.T) {
	items := []Candidate{
		{Entry: Entry{ID: "g1", UserID: "u1", Type: TypeFact, Content: "大大阿达大大阿达"}, BM25Score: 0},
		{Entry: Entry{ID: "g2", UserID: "u1", Type: TypeFact, Content: "asdfgh qwerty"}, BM25Score: 0},
	}
	r := newRetriever(items, 0, 5)
	got, err := r.Retrieve(context.Background(), "u1", "", "1+1=2?")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("全零证据应返回空，got %v", ids(got))
	}
}

// BUG-20260712-O 真机标定回归锁：nomic-embed-text 中文实测刻度（2026-07-12 取证）——
// 无关对 hybrid(0.7·cos) ≈ 0.36~0.45（你好↔花生过敏 cos=0.640→0.448），
// 相关对 ≥ 0.58（花生酱↔花生过敏 cos=0.905→0.633）。默认地板必须落在分界带内，
// 否则真实嵌入下无关记忆必过（旧 0.3 的实锤真机 bug）。
func TestBug20260712_FloorCalibratedForRealEmbeddings(t *testing.T) {
	items := []Candidate{
		{Entry: Entry{ID: "unrelated", UserID: "u1", Type: TypeFact, Content: "孩子对花生过敏"}, VectorScore: 0.640, HasVector: true},     // 你好↔花生（真机实测）
		{Entry: Entry{ID: "related", UserID: "u1", Type: TypeFact, Content: "孩子对花生过敏，不能吃花生"}, VectorScore: 0.905, HasVector: true}, // 花生酱↔花生（真机实测）
	}
	r := newRetriever(items, 0.5 /* = config 默认 RecallMinScore（真机标定） */, 5)
	got, err := r.Retrieve(context.Background(), "u1", "", "任意查询")
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := ids(got)
	if contains(gotIDs, "unrelated") {
		t.Fatalf("真机无关刻度（cos 0.640→hybrid 0.448）必须被默认地板砍掉：%v", gotIDs)
	}
	if !contains(gotIDs, "related") {
		t.Fatalf("真机相关刻度（cos 0.905→hybrid 0.633）必须保留：%v", gotIDs)
	}
}
