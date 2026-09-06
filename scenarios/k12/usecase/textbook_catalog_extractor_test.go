package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestTextbookCatalogCheckpointExtractorUsesTOCAndPrintedFooterProof(t *testing.T) {
	source := syntheticTextbookCatalogSource()
	source.DocumentTitle = "五年级下册-数学-冒烟0905.pdf"

	publication, err := (TextbookCatalogCheckpointExtractor{}).Extract(
		context.Background(), source,
	)
	if err != nil {
		t.Fatalf("extract deterministic catalog: %v", err)
	}
	var catalog struct {
		TextbookEdition string `json:"textbook_edition"`
		TextbookVersion string `json:"textbook_version"`
		Title           string `json:"title"`
		Volume          string `json:"volume"`
		PageMin         int    `json:"page_min"`
		PageMax         int    `json:"page_max"`
		Units           []struct {
			Title    string `json:"title"`
			PageFrom int    `json:"page_from"`
			PageTo   int    `json:"page_to"`
		} `json:"units"`
		PageRefs []struct {
			LogicalPage int `json:"logical_page"`
			PDFPage     int `json:"pdf_page"`
		} `json:"page_refs"`
	}
	if err := json.Unmarshal(publication.CatalogJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.TextbookEdition != "人教版" || catalog.TextbookVersion != "2022" ||
		catalog.Title != "五年级下册-数学-冒烟0905" || catalog.Volume != "下册" {
		t.Fatalf("catalog metadata was not derived from exact evidence: %+v", catalog)
	}
	if catalog.PageMin != 1 || catalog.PageMax != 3 || len(catalog.Units) != 2 {
		t.Fatalf("catalog range/units=%d..%d %+v", catalog.PageMin, catalog.PageMax, catalog.Units)
	}
	if len(catalog.PageRefs) != 3 || catalog.PageRefs[0].LogicalPage != 1 ||
		catalog.PageRefs[0].PDFPage != 3 || catalog.PageRefs[2].PDFPage != 5 {
		t.Fatalf("logical->physical map=%+v; must use footer evidence, not logical=PDF", catalog.PageRefs)
	}
	if len(publication.PageProofs) != 3 {
		t.Fatalf("page proofs=%+v want 3", publication.PageProofs)
	}
	firstProof := publication.PageProofs[0]
	if firstProof.EvidencePage != 3 || firstProof.Method != "printed_anchor" ||
		strings.TrimSpace(source.Pages[2].Content[firstProof.EvidenceOffsetFrom:firstProof.EvidenceOffsetTo]) != "1" {
		t.Fatalf("page proof does not bind the persisted footer span: %+v", publication.PageProofs)
	}
}

func TestTextbookCatalogCheckpointExtractorPrefersCurrentApprovalYearAcrossPDFLineBreaks(t *testing.T) {
	source := syntheticTextbookCatalogSource()
	source.Pages[0].Content = strings.Replace(
		source.Pages[0].Content,
		"2022年经国家教材委员会专家委员会审核通过",
		"《义务教育教科书数学五年级下册》（2014 年版）基础上修订，\n"+
			"2022 年经国家教材委员会专家\n委员会审核通过",
		1,
	)
	source.Pages[0].ContentDigest = testTextbookContentDigest(source.Pages[0].Content)

	publication, err := (TextbookCatalogCheckpointExtractor{}).Extract(
		context.Background(), source,
	)
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		TextbookVersion string `json:"textbook_version"`
	}
	if err := json.Unmarshal(publication.CatalogJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.TextbookVersion != "2022" {
		t.Fatalf("current approval year=%q, want 2022 rather than superseded base edition 2014",
			catalog.TextbookVersion)
	}
}

func TestTextbookCatalogCheckpointExtractorAcceptsUnicodeTOCWhitespace(t *testing.T) {
	source := syntheticTextbookCatalogSource()
	source.Pages[1].Content = "目 录\n1. **观察物体（三）**　1\n2. **因数和倍数**　2\n"
	source.Pages[1].ContentDigest = testTextbookContentDigest(source.Pages[1].Content)
	publication, err := (TextbookCatalogCheckpointExtractor{}).Extract(
		context.Background(), source,
	)
	if err != nil {
		t.Fatalf("extract catalog with full-width TOC whitespace: %v", err)
	}
	var catalog struct {
		Units []struct {
			Title    string `json:"title"`
			PageFrom int    `json:"page_from"`
		} `json:"units"`
	}
	if err := json.Unmarshal(publication.CatalogJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Units) != 2 || catalog.Units[0].Title != "观察物体（三）" ||
		catalog.Units[0].PageFrom != 1 || catalog.Units[1].PageFrom != 2 {
		t.Fatalf("unicode TOC units=%+v", catalog.Units)
	}
}

func TestTextbookCatalogCheckpointExtractorFailsClosedOnMissingVersionOrPage(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*k12storage.TextbookCatalogSource)
	}{
		{
			name: "no provable edition year",
			mutate: func(source *k12storage.TextbookCatalogSource) {
				source.Pages[0].Content = strings.ReplaceAll(
					source.Pages[0].Content,
					"2022年经国家教材委员会专家委员会审核通过",
					"经国家教材委员会专家委员会审核通过",
				)
				source.Pages[0].ContentDigest = testTextbookContentDigest(source.Pages[0].Content)
			},
		},
		{
			name: "non contiguous printed footer",
			mutate: func(source *k12storage.TextbookCatalogSource) {
				source.Pages[3].Content = strings.ReplaceAll(source.Pages[3].Content, "\n2\n", "\n20\n")
				source.Pages[3].ContentDigest = testTextbookContentDigest(source.Pages[3].Content)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := syntheticTextbookCatalogSource()
			tt.mutate(&source)
			publication, err := (TextbookCatalogCheckpointExtractor{}).Extract(
				context.Background(), source,
			)
			if !errors.Is(err, ErrTextbookCatalogEvidenceInsufficient) {
				t.Fatalf("error=%v want evidence-insufficient", err)
			}
			if len(publication.CatalogJSON) != 0 || len(publication.PageProofs) != 0 {
				t.Fatalf("fail-closed extractor returned partial proposal: %+v", publication)
			}
		})
	}
}

func syntheticTextbookCatalogSource() k12storage.TextbookCatalogSource {
	contents := []string{
		"义务教育教科书\n数学 五年级 下册\n人民教育出版社\n2022年经国家教材委员会专家委员会审核通过\n",
		"目 录\n1 观察物体（三） 1\n2 因数和倍数 2\n",
		"1 观察物体（三）\n正文\n1\n",
		"2 因数和倍数\n正文\n2\n",
		"练习\n正文\n3\n",
	}
	pages := make([]k12storage.TextbookCatalogSourcePage, 0, len(contents))
	offset := int64(0)
	for index, content := range contents {
		page := k12storage.TextbookCatalogSourcePage{
			PDFPage:          index + 1,
			Content:          content,
			ContentDigest:    testTextbookContentDigest(content),
			SourceOffsetFrom: offset,
			SourceOffsetTo:   offset + int64(len(content)),
		}
		if index >= 2 {
			page.SegmentRefs = []string{"chunk-page-" + string(rune('1'+index-2))}
		}
		pages = append(pages, page)
		offset += int64(len(content))
	}
	return k12storage.TextbookCatalogSource{
		IngestJobID:      "ingest-proof-1",
		SourcePlanDigest: strings.Repeat("b", 64),
		DocumentTitle:    "义务教育教科书·数学五年级下册.pdf",
		SourceDigest:     strings.Repeat("a", 64),
		Pages:            pages,
	}
}

func testTextbookContentDigest(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}
