package knowledge

import (
	"reflect"
	"strings"
	"testing"
)

type bug20260726034A02DocumentGenerationRef struct {
	documentID string
	generation int64
}

func bug20260726034A02SetInteger(value reflect.Value, number int64) bool {
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(number)
		return true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(uint64(number))
		return true
	default:
		return false
	}
}

func bug20260726034A02PopulateReference(
	value reflect.Value,
	ref bug20260726034A02DocumentGenerationRef,
) bool {
	if value.Kind() == reflect.Pointer {
		value.Set(reflect.New(value.Type().Elem()))
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return false
	}
	documentField := value.FieldByName("DocumentID")
	generationField := value.FieldByName("DocumentGeneration")
	if !generationField.IsValid() {
		generationField = value.FieldByName("ContentGeneration")
	}
	if !documentField.IsValid() || !documentField.CanSet() ||
		documentField.Kind() != reflect.String ||
		!generationField.IsValid() || !generationField.CanSet() {
		return false
	}
	documentField.SetString(ref.documentID)
	return bug20260726034A02SetInteger(generationField, ref.generation)
}

func bug20260726034A02Filter(
	t *testing.T,
	refs ...bug20260726034A02DocumentGenerationRef,
) Filter {
	t.Helper()
	var filter Filter
	value := reflect.ValueOf(&filter).Elem()
	kind := value.Type()
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		fieldType := kind.Field(i)
		if !field.CanSet() {
			continue
		}
		if field.Kind() == reflect.Slice {
			slice := reflect.MakeSlice(field.Type(), 0, len(refs))
			matched := true
			for _, ref := range refs {
				element := reflect.New(field.Type().Elem()).Elem()
				if !bug20260726034A02PopulateReference(element, ref) {
					matched = false
					break
				}
				slice = reflect.Append(slice, element)
			}
			if matched {
				field.Set(slice)
				return filter
			}
		}
		if field.Kind() == reflect.Map &&
			field.Type().Key().Kind() == reflect.String &&
			(strings.Contains(fieldType.Name, "Generation") ||
				strings.Contains(fieldType.Name, "Document")) {
			mapping := reflect.MakeMap(field.Type())
			matched := true
			for _, ref := range refs {
				entry := reflect.New(field.Type().Elem()).Elem()
				if !bug20260726034A02SetInteger(entry, ref.generation) {
					matched = false
					break
				}
				mapping.SetMapIndex(reflect.ValueOf(ref.documentID).Convert(
					field.Type().Key()), entry)
			}
			if matched {
				field.Set(mapping)
				return filter
			}
		}
	}
	t.Fatal("BUG-20260726-034-A02: knowledge.Filter lacks a paired document_id + document_generation whitelist")
	return Filter{}
}

func bug20260726034A02RevisionFixture(
	t *testing.T,
) (*revisionSearchHarness, *SQLiteRevisionSemanticSearcher, string) {
	t.Helper()
	harness := newRevisionSearchHarness(t)
	boot, err := harness.service.EnsureDefaultPolicy(
		harness.ctx, "owner-1", "default",
	)
	if err != nil {
		t.Fatal(err)
	}
	var corpusUID string
	if err := harness.db.QueryRowContext(harness.ctx, `SELECT corpus_uid
		FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	if boot.ActiveRevisionID == nil {
		t.Fatal("active revision is absent")
	}
	for i := 0; i < 5; i++ {
		documentID := "noise-" + string(rune('0'+i))
		harness.addLegacyDocument(documentID,
			"a02token a02token a02token noise", nil)
		harness.bindDocument("owner-1", corpusUID, documentID)
		harness.seedVisibleRevisionVector(
			*boot.ActiveRevisionID, corpusUID, documentID, []float32{1, 0, 0},
		)
	}
	harness.addLegacyDocument("target-doc", "a02token target", nil)
	harness.bindDocument("owner-1", corpusUID, "target-doc")
	harness.seedVisibleRevisionVector(
		*boot.ActiveRevisionID, corpusUID, "target-doc", []float32{0, 1, 0},
	)
	searcher := NewSQLiteRevisionSemanticSearcher(
		harness.db, "owner-1", "default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{
			"profile-a": {dimension: 3},
		}},
	)
	return harness, searcher, corpusUID
}

func TestBUG20260726034A02_FilterCarriesPairedDocumentGenerationWhitelist(t *testing.T) {
	filter := bug20260726034A02Filter(t,
		bug20260726034A02DocumentGenerationRef{
			documentID: "doc-a", generation: 7,
		},
	)
	if filter.IsZero() {
		t.Fatal("BUG-20260726-034-A02: document-generation whitelist is treated as zero filter")
	}
	if filter.normalize().IsZero() {
		t.Fatal("BUG-20260726-034-A02: normalize discarded document-generation whitelist")
	}
}

func TestBUG20260726034A02_DocumentGenerationFilterPushesBeforeFTSAndVectorTopK(t *testing.T) {
	_, searcher, _ := bug20260726034A02RevisionFixture(t)
	allowed := bug20260726034A02Filter(t,
		bug20260726034A02DocumentGenerationRef{
			documentID: "target-doc", generation: 1,
		},
	)
	textResults, err := searcher.TextSearch(
		t.Context(), "a02token", 1, allowed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(textResults) != 1 || textResults[0].Chunk.DocID != "target-doc" {
		t.Fatalf("BUG-20260726-034-A02: FTS whitelist ran after LIMIT: %+v",
			textResults)
	}
	vectorResults, routeRan, err := searcher.Search(
		t.Context(), "a02token", 1, allowed,
	)
	if err != nil || !routeRan {
		t.Fatalf("vector route ran=%v err=%v", routeRan, err)
	}
	if len(vectorResults) != 1 || vectorResults[0].Chunk.DocID != "target-doc" {
		t.Fatalf("BUG-20260726-034-A02: vector whitelist ran after topK: %+v",
			vectorResults)
	}

	wrongGeneration := bug20260726034A02Filter(t,
		bug20260726034A02DocumentGenerationRef{
			documentID: "target-doc", generation: 2,
		},
	)
	textResults, err = searcher.TextSearch(
		t.Context(), "a02token", 1, wrongGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	vectorResults, routeRan, err = searcher.Search(
		t.Context(), "a02token", 1, wrongGeneration,
	)
	if err != nil || !routeRan {
		t.Fatalf("wrong-generation vector route ran=%v err=%v", routeRan, err)
	}
	if len(textResults) != 0 || len(vectorResults) != 0 {
		t.Fatalf("BUG-20260726-034-A02: generation mismatch leaked FTS/vector=%v/%v",
			resultDocIDs(textResults), resultDocIDs(vectorResults))
	}
}

func TestBUG20260726034A02_TombstoneAndReplacementGenerationInvalidateRetrieval(t *testing.T) {
	harness, searcher, corpusUID := bug20260726034A02RevisionFixture(t)
	generationOne := bug20260726034A02Filter(t,
		bug20260726034A02DocumentGenerationRef{
			documentID: "target-doc", generation: 1,
		},
	)
	if _, err := harness.db.ExecContext(harness.ctx,
		`UPDATE kb_semantic_document_bindings
		 SET lifecycle_state='tombstoned',text_state='failed',deleted_at=2,
		     version=version+1,updated_at=2
		 WHERE owner_id='owner-1' AND corpus_uid=? AND document_id='target-doc'
		   AND content_generation=1`, corpusUID); err != nil {
		t.Fatal(err)
	}
	results, err := searcher.TextSearch(t.Context(), "a02token", 5, generationOne)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("BUG-20260726-034-A02: tombstoned generation remained visible: %v",
			resultDocIDs(results))
	}

	if _, err := harness.db.ExecContext(harness.ctx,
		`INSERT INTO kb_semantic_document_generations
		 (owner_id,corpus_uid,document_id,content_generation,created_at)
		 VALUES('owner-1',?,'target-doc',2,3)`, corpusUID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.ExecContext(harness.ctx,
		`UPDATE kb_semantic_document_bindings
		 SET content_generation=2,lifecycle_state='active',text_state='ready',
		     deleted_at=NULL,version=version+1,updated_at=3
		 WHERE owner_id='owner-1' AND corpus_uid=? AND document_id='target-doc'`,
		corpusUID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.ExecContext(harness.ctx,
		`INSERT INTO kb_revision_documents
		 (revision_id,corpus_uid,document_id,content_generation,vector_state,
		  expected_chunks,embedded_chunks,failed_chunks,visible_at,last_error,updated_at)
		 SELECT revision_id,corpus_uid,document_id,2,vector_state,
		        expected_chunks,embedded_chunks,failed_chunks,visible_at,last_error,updated_at
		 FROM kb_revision_documents
		 WHERE corpus_uid=? AND document_id='target-doc' AND content_generation=1`,
		corpusUID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.ExecContext(harness.ctx,
		`INSERT INTO kb_revision_vectors
		 (revision_id,corpus_uid,document_id,content_generation,chunk_id,chunk_index,
		  chunk_content_hash,profile_snapshot_id,profile_config_hash,provider_id,
		  provider_location,model_name,dimension,embedding,created_at)
		 SELECT revision_id,corpus_uid,document_id,2,chunk_id,chunk_index,
		        chunk_content_hash,profile_snapshot_id,profile_config_hash,provider_id,
		        provider_location,model_name,dimension,embedding,created_at
		 FROM kb_revision_vectors
		 WHERE corpus_uid=? AND document_id='target-doc' AND content_generation=1`,
		corpusUID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.ExecContext(harness.ctx,
		`DELETE FROM kb_revision_vectors
		 WHERE corpus_uid=? AND document_id='target-doc' AND content_generation=1`,
		corpusUID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.ExecContext(harness.ctx,
		`DELETE FROM kb_revision_documents
		 WHERE corpus_uid=? AND document_id='target-doc' AND content_generation=1`,
		corpusUID); err != nil {
		t.Fatal(err)
	}
	results, err = searcher.TextSearch(t.Context(), "a02token", 5, generationOne)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("BUG-20260726-034-A02: old generation survived replacement: %v",
			resultDocIDs(results))
	}
	generationTwo := bug20260726034A02Filter(t,
		bug20260726034A02DocumentGenerationRef{
			documentID: "target-doc", generation: 2,
		},
	)
	results, err = searcher.TextSearch(t.Context(), "a02token", 5, generationTwo)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Chunk.DocID != "target-doc" {
		t.Fatalf("replacement generation is not the sole visible target: %v",
			resultDocIDs(results))
	}
}
