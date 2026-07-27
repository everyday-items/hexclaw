package usecase_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type bug20260726008BindingGroundingSpy struct {
	freezeCalls int
	queryCalls  int
	requested   usecase.GroundingSnapshot
}

func (s *bug20260726008BindingGroundingSpy) Ground(
	context.Context, string, string, string,
) (string, bool, error) {
	return "legacy evidence must not be used", true, nil
}

func (s *bug20260726008BindingGroundingSpy) FreezeGroundingSnapshot(
	_ context.Context,
	requested usecase.GroundingSnapshot,
) (usecase.GroundingSnapshot, error) {
	s.freezeCalls++
	s.requested = requested
	requested.VectorRevisionID = "revision-active"
	return requested, nil
}

func (s *bug20260726008BindingGroundingSpy) GroundSnapshot(
	_ context.Context,
	_ usecase.GroundingSnapshot,
	_, _ string,
) (string, bool, error) {
	s.queryCalls++
	return "教材中的同分母分数图示", true, nil
}

func seedBUG20260726008ActiveTextbookBinding(t *testing.T, d usecase.Deps) {
	t.Helper()
	db := d.Records.DB()
	digest := strings.Repeat("a", 64)
	catalogDigest := strings.Repeat("b", 64)
	catalog := `{"subject":"math","textbook_edition":"人教版","textbook_version":"2022","title":"义务教育教科书·数学五年级下册","volume":"下册","page_min":1,"page_max":100,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":20,"lessons":[]}],"page_refs":[{"logical_page":1,"pdf_page":1,"segment_refs":["segment-1"]}]}`
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO kb_semantic_corpora
			(corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
			VALUES('corpus-mingming','mingming','default','general',1,1,1)`, nil},
		{`INSERT INTO kb_documents
			(id,title,content,source,deleted,corpus_uid,created_at,updated_at)
			VALUES('doc-math','义务教育教科书·数学五年级下册.pdf','教材正文',
			'upload:math.pdf',0,'corpus-mingming',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, nil},
		{`INSERT INTO kb_semantic_document_generations
			(owner_id,corpus_uid,document_id,content_generation,created_at)
			VALUES('mingming','corpus-mingming','doc-math',1,1)`, nil},
		{`INSERT INTO kb_semantic_document_bindings
			(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,
			 text_state,version,created_at,updated_at)
			VALUES('doc-math','mingming','corpus-mingming',1,'active','ready',1,1,1)`, nil},
		{`INSERT INTO k12_textbook_manifests
			(manifest_id,owner_id,document_id,document_generation,document_title,
			 subject,source_digest,state,retryable,failure_message,text_index_state,
			 vector_index_state,catalog_json,catalog_digest,created_at,updated_at)
			VALUES('manifest-math','mingming','doc-math',1,
			'义务教育教科书·数学五年级下册.pdf','math',?,
			'ready_for_confirmation',0,'','ready','ready',?,?,1,1)`,
			[]any{digest, catalog, catalogDigest}},
		{`INSERT INTO k12_textbook_manifest_segments
			(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
			 document_generation,source_digest,created_at,updated_at)
			VALUES('manifest-segment-1','manifest-math',1,'segment-1',1,
			'doc-math',1,?,1,1)`, []any{digest}},
		{`INSERT INTO k12_textbook_bindings
			(textbook_binding_id,owner_id,agent_name,subject,textbook_manifest_id,
			 document_id,document_generation,status,created_at,updated_at)
			VALUES('binding-math','mingming','mingming','math','manifest-math',
			'doc-math',1,'active',1,1)`, nil},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed BUG-20260726-008 active binding: %v", err)
		}
	}
}

func TestBUG20260726008_TutoringTipsFreezesDurableActiveBindingBeforeKnowledgeQuery(t *testing.T) {
	d := newDataDeps(t, "mingming")
	seedBUG20260726008ActiveTextbookBinding(t, d)
	if err := d.Records.PutProblemAttemptSnapshot(
		context.Background(), confirmedTipsFacts(1, "canonical"),
	); err != nil {
		t.Fatal(err)
	}
	job := driveTipsJobToAssessing(t, d)
	spy := &bug20260726008BindingGroundingSpy{}
	d.Grounding = spy
	d.Profiles = &memProfileStore{m: map[string]k12.ChildProfile{
		"mingming": {
			ChildName: "小明", GradeTerm: "五年级下", TextbookEdition: "人教版",
		},
	}}

	if _, err := d.BuildTutoringTips(
		context.Background(), "mingming", job.Record.RecordID,
	); err != nil {
		t.Fatal(err)
	}
	if spy.freezeCalls != 1 {
		t.Fatalf("BUG-20260726-008: freeze calls=%d want 1", spy.freezeCalls)
	}
	if spy.requested.TextbookBindingID != "binding-math" {
		t.Fatalf("BUG-20260726-008 RED: durable active binding was not resolved before freeze: %+v", spy.requested)
	}
}
