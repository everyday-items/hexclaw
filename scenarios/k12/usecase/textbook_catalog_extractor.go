package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var ErrTextbookCatalogEvidenceInsufficient = errors.New("textbook catalog evidence insufficient")

type TextbookCatalogExtractor interface {
	Extract(context.Context, k12storage.TextbookCatalogSource) (
		k12storage.TextbookCatalogPublication, error,
	)
}

type TextbookCatalogExtractorFunc func(
	context.Context, k12storage.TextbookCatalogSource,
) (k12storage.TextbookCatalogPublication, error)

func (fn TextbookCatalogExtractorFunc) Extract(
	ctx context.Context,
	source k12storage.TextbookCatalogSource,
) (k12storage.TextbookCatalogPublication, error) {
	return fn(ctx, source)
}

// TextbookCatalogCheckpointExtractor is deliberately local and deterministic.
// It accepts only a complete persisted Knowledge page manifest, exact TOC
// rows, and a standalone printed footer on every mapped body page. It never
// assumes that logical page N is PDF page N and has no model fallback.
type TextbookCatalogCheckpointExtractor struct{}

var (
	textbookVolumePattern       = regexp.MustCompile(`([一二三四五六]年级)\s*(上册|下册)`)
	textbookApprovalYearPattern = regexp.MustCompile(`([12][0-9]{3})\s*年\s*经国家教材委员会专家委员会审核通过`)
	textbookEditionYearPattern  = regexp.MustCompile(`（\s*([12][0-9]{3})\s*年版\s*）`)
	textbookTOCMajorPattern     = regexp.MustCompile(`^\s*([1-9][0-9]*)\.?\s+(.+?)\s+([1-9][0-9]*)\s*$`)
	textbookTOCChinesePattern   = regexp.MustCompile(`^\s*([一二三四五六七八九])\s+(.+?)\s+([1-9][0-9]*)\s*$`)
	textbookTOCChildPattern     = regexp.MustCompile(`^\s{2,}(\S.*?)\s+([1-9][0-9]*)\s*$`)
)

type extractedTextbookUnit struct {
	Ordinal  int
	Title    string
	PageFrom int
	Children []extractedTextbookLesson
}

type extractedTextbookLesson struct {
	Title    string
	PageFrom int
}

type textbookCatalogJSON struct {
	Subject         string                    `json:"subject"`
	TextbookEdition string                    `json:"textbook_edition"`
	TextbookVersion string                    `json:"textbook_version"`
	Title           string                    `json:"title"`
	Volume          string                    `json:"volume"`
	PageMin         int                       `json:"page_min"`
	PageMax         int                       `json:"page_max"`
	Units           []textbookCatalogJSONUnit `json:"units"`
	PageRefs        []textbookCatalogJSONPage `json:"page_refs"`
}

type textbookCatalogJSONUnit struct {
	UnitID   string                      `json:"unit_id"`
	Title    string                      `json:"title"`
	PageFrom int                         `json:"page_from"`
	PageTo   int                         `json:"page_to"`
	Lessons  []textbookCatalogJSONLesson `json:"lessons"`
}

type textbookCatalogJSONLesson struct {
	LessonID string `json:"lesson_id"`
	Title    string `json:"title"`
	PageFrom int    `json:"page_from"`
	PageTo   int    `json:"page_to"`
}

type textbookCatalogJSONPage struct {
	LogicalPage int      `json:"logical_page"`
	PDFPage     int      `json:"pdf_page"`
	SegmentRefs []string `json:"segment_refs"`
}

type printedPageAnchor struct {
	LogicalPage  int
	PDFPage      int
	EvidencePage int
	OffsetFrom   int
	OffsetTo     int
	Digest       string
	Method       string
	Segments     []string
}

func (TextbookCatalogCheckpointExtractor) Extract(
	ctx context.Context,
	source k12storage.TextbookCatalogSource,
) (k12storage.TextbookCatalogPublication, error) {
	fail := func(reason string) (k12storage.TextbookCatalogPublication, error) {
		return k12storage.TextbookCatalogPublication{}, fmt.Errorf(
			"%w: %s", ErrTextbookCatalogEvidenceInsufficient, reason,
		)
	}
	if err := ctx.Err(); err != nil {
		return k12storage.TextbookCatalogPublication{}, err
	}
	if strings.TrimSpace(source.IngestJobID) == "" ||
		!validTextbookSHA256(source.SourcePlanDigest) ||
		!validTextbookSHA256(source.SourceDigest) || len(source.Pages) == 0 {
		return fail("invalid persisted source snapshot")
	}

	allText := strings.Builder{}
	anchors := make([]printedPageAnchor, 0, len(source.Pages))
	tocStart := -1
	for index, page := range source.Pages {
		if err := ctx.Err(); err != nil {
			return k12storage.TextbookCatalogPublication{}, err
		}
		if page.PDFPage != index+1 || strings.TrimSpace(page.Content) == "" ||
			page.SourceOffsetFrom < 0 || page.SourceOffsetTo <= page.SourceOffsetFrom ||
			textbookContentDigest(page.Content) != page.ContentDigest {
			return fail("corrupt or incomplete page checkpoint")
		}
		allText.WriteString(page.Content)
		allText.WriteByte('\n')
		if tocStart < 0 && strings.Contains(removeTextbookWhitespace(page.Content), "目录") {
			tocStart = index
		}
		logicalPage, from, to, ok := trailingPrintedPageAnchor(page.Content)
		if !ok {
			continue
		}
		segments := append([]string(nil), page.SegmentRefs...)
		sort.Strings(segments)
		if len(segments) == 0 || hasEmptyOrDuplicateTextbookString(segments) {
			return fail("printed page has no exact chunk proof")
		}
		anchors = append(anchors, printedPageAnchor{
			LogicalPage: logicalPage, PDFPage: page.PDFPage,
			EvidencePage: page.PDFPage,
			OffsetFrom:   from, OffsetTo: to, Digest: page.ContentDigest,
			Method:   "printed_anchor",
			Segments: segments,
		})
	}
	if tocStart < 0 || len(anchors) < 2 {
		return fail("table of contents or printed footer is missing")
	}
	pageOffset := anchors[0].LogicalPage - anchors[0].PDFPage
	for index, anchor := range anchors {
		if anchor.LogicalPage-anchor.PDFPage != pageOffset {
			return fail("printed page anchors have conflicting offsets")
		}
		if index > 0 {
			previous := anchors[index-1]
			pdfGap := anchor.PDFPage - previous.PDFPage
			if pdfGap < 1 || pdfGap > 2 || anchor.LogicalPage-previous.LogicalPage != pdfGap {
				return fail("printed page anchors are not complete and monotonic")
			}
		}
	}
	explicitByPDFPage := make(map[int]printedPageAnchor, len(anchors))
	for _, anchor := range anchors {
		explicitByPDFPage[anchor.PDFPage] = anchor
	}
	firstAnchorPDF := anchors[0].PDFPage
	lastAnchorPDF := anchors[len(anchors)-1].PDFPage
	completedAnchors := make([]printedPageAnchor, 0, lastAnchorPDF-firstAnchorPDF+1)
	for pdfPage := firstAnchorPDF; pdfPage <= lastAnchorPDF; pdfPage++ {
		if anchor, ok := explicitByPDFPage[pdfPage]; ok {
			completedAnchors = append(completedAnchors, anchor)
			continue
		}
		previous, previousOK := explicitByPDFPage[pdfPage-1]
		next, nextOK := explicitByPDFPage[pdfPage+1]
		if !previousOK || !nextOK ||
			previous.LogicalPage+2 != next.LogicalPage {
			return fail("printed page anchors are not complete and monotonic")
		}
		page := source.Pages[pdfPage-1]
		segments := append([]string(nil), page.SegmentRefs...)
		sort.Strings(segments)
		if len(segments) == 0 || hasEmptyOrDuplicateTextbookString(segments) {
			return fail("interpolated page has no exact chunk proof")
		}
		// 只补齐被前后两个可信页脚夹住的单页；正文和 chunk 仍取该物理页自身事实。
		completedAnchors = append(completedAnchors, printedPageAnchor{
			LogicalPage: pdfPage + pageOffset,
			PDFPage:     pdfPage, EvidencePage: pdfPage,
			OffsetFrom: 0, OffsetTo: len(page.Content), Digest: page.ContentDigest,
			Method: "adjacent_printed_anchors", Segments: segments,
		})
	}
	anchors = completedAnchors

	fullText := allText.String()
	if !strings.Contains(fullText, "人民教育出版社") {
		return fail("publisher evidence is missing")
	}
	volumeMatch := textbookVolumePattern.FindStringSubmatch(fullText)
	if len(volumeMatch) != 3 {
		return fail("grade or volume evidence is missing")
	}
	// PDF text extraction may insert a physical line break inside a semantic
	// phrase (the frozen PEP fixture splits “专家委员会”). Collapse whitespace
	// only for metadata evidence matching so the current approval year is not
	// lost and silently replaced by an older “base edition” year.
	versionEvidence := removeTextbookWhitespace(fullText)
	version := ""
	if match := textbookApprovalYearPattern.FindStringSubmatch(versionEvidence); len(match) == 2 {
		version = match[1]
	} else if match := textbookEditionYearPattern.FindStringSubmatch(versionEvidence); len(match) == 2 {
		version = match[1]
	}
	if version == "" {
		return fail("copyright or edition year evidence is missing")
	}
	title := strings.TrimSpace(source.DocumentTitle)
	if strings.EqualFold(filepath.Ext(title), ".pdf") {
		title = strings.TrimSuffix(title, filepath.Ext(title))
	}
	// 上传名仅作显示标签；教材版本、目录和页码仍取持久页面证据。
	if title == "" {
		return fail("document title is missing")
	}

	if tocStart+1 >= firstAnchorPDF {
		return fail("table of contents is not before textbook body")
	}
	units, err := parseTextbookTOC(source.Pages[tocStart : firstAnchorPDF-1])
	if err != nil {
		return fail(err.Error())
	}
	pageMin, pageMax := anchors[0].LogicalPage, anchors[len(anchors)-1].LogicalPage
	if len(units) == 0 || units[0].PageFrom != pageMin {
		return fail("TOC start page does not match first printed page")
	}
	anchorByLogical := make(map[int]printedPageAnchor, len(anchors))
	for _, anchor := range anchors {
		anchorByLogical[anchor.LogicalPage] = anchor
	}

	catalog := textbookCatalogJSON{
		Subject: "math", TextbookEdition: "人教版", TextbookVersion: version,
		Title: title, Volume: volumeMatch[2], PageMin: pageMin, PageMax: pageMax,
		Units:    make([]textbookCatalogJSONUnit, 0, len(units)),
		PageRefs: make([]textbookCatalogJSONPage, 0, len(anchors)),
	}
	for index, unit := range units {
		if unit.Ordinal != index+1 || unit.PageFrom < pageMin || unit.PageFrom > pageMax {
			return fail("TOC unit sequence or page is invalid")
		}
		if _, ok := anchorByLogical[unit.PageFrom]; !ok {
			return fail("TOC unit page has no printed footer proof")
		}
		pageTo := pageMax
		if index+1 < len(units) {
			if units[index+1].PageFrom <= unit.PageFrom {
				return fail("TOC unit pages are not strictly increasing")
			}
			pageTo = units[index+1].PageFrom - 1
		}
		out := textbookCatalogJSONUnit{
			UnitID: stableTextbookCatalogID("u", source.SourceDigest, unit.Title,
				strconv.Itoa(unit.PageFrom)),
			Title: unit.Title, PageFrom: unit.PageFrom, PageTo: pageTo,
			Lessons: make([]textbookCatalogJSONLesson, 0, len(unit.Children)),
		}
		for lessonIndex, lesson := range unit.Children {
			if lesson.PageFrom < unit.PageFrom || lesson.PageFrom > pageTo {
				return fail("TOC lesson page is outside its unit")
			}
			lessonTo := pageTo
			if lessonIndex+1 < len(unit.Children) {
				if unit.Children[lessonIndex+1].PageFrom <= lesson.PageFrom {
					return fail("TOC lesson pages are not strictly increasing")
				}
				lessonTo = unit.Children[lessonIndex+1].PageFrom - 1
			}
			out.Lessons = append(out.Lessons, textbookCatalogJSONLesson{
				LessonID: stableTextbookCatalogID("l", source.SourceDigest,
					unit.Title, lesson.Title, strconv.Itoa(lesson.PageFrom)),
				Title: lesson.Title, PageFrom: lesson.PageFrom, PageTo: lessonTo,
			})
		}
		catalog.Units = append(catalog.Units, out)
	}

	proofs := make([]k12storage.TextbookCatalogPageProof, 0, len(anchors))
	for _, anchor := range anchors {
		catalog.PageRefs = append(catalog.PageRefs, textbookCatalogJSONPage{
			LogicalPage: anchor.LogicalPage, PDFPage: anchor.PDFPage,
			SegmentRefs: append([]string(nil), anchor.Segments...),
		})
		proofs = append(proofs, k12storage.TextbookCatalogPageProof{
			LogicalPage: anchor.LogicalPage, PDFPage: anchor.PDFPage,
			EvidencePage: anchor.EvidencePage, EvidenceOffsetFrom: anchor.OffsetFrom,
			EvidenceOffsetTo: anchor.OffsetTo, EvidenceDigest: anchor.Digest,
			Method: anchor.Method, SegmentRefs: append([]string(nil), anchor.Segments...),
		})
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		return k12storage.TextbookCatalogPublication{}, err
	}
	return k12storage.TextbookCatalogPublication{CatalogJSON: encoded, PageProofs: proofs}, nil
}

func parseTextbookTOC(pages []k12storage.TextbookCatalogSourcePage) ([]extractedTextbookUnit, error) {
	units := make([]extractedTextbookUnit, 0)
	for _, page := range pages {
		for _, line := range strings.Split(page.Content, "\n") {
			// PDF 文本层可能用全角空格等 Unicode 空白分隔目录字段；只归一分隔符，字段校验仍由正则和页证据完成。
			line = strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return ' '
				}
				return r
			}, line)
			trimmedLeft := strings.TrimLeft(line, " ")
			if strings.HasPrefix(trimmedLeft, "#") {
				line = strings.TrimSpace(strings.TrimLeft(trimmedLeft, "#"))
			}
			majorMatch := textbookTOCMajorPattern.FindStringSubmatch(line)
			ordinal := 0
			if len(majorMatch) != 4 {
				majorMatch = textbookTOCChinesePattern.FindStringSubmatch(line)
				if len(majorMatch) == 4 {
					ordinal = strings.Index("一二三四五六七八九", majorMatch[1])/len("一") + 1
				}
			}
			if len(majorMatch) == 4 {
				if ordinal == 0 {
					ordinal, _ = strconv.Atoi(majorMatch[1])
				}
				pageFrom, _ := strconv.Atoi(majorMatch[3])
				title := strings.TrimSpace(majorMatch[2])
				// OCR 可保留 Markdown 粗体包装；只去掉成对包装，不改目录文字。
				if len(title) >= 4 && strings.HasPrefix(title, "**") && strings.HasSuffix(title, "**") {
					title = strings.TrimSpace(title[2 : len(title)-2])
				}
				if title == "" || ordinal != len(units)+1 {
					return nil, fmt.Errorf("TOC unit sequence is ambiguous")
				}
				units = append(units, extractedTextbookUnit{
					Ordinal: ordinal, Title: title, PageFrom: pageFrom,
				})
				continue
			}
			if match := textbookTOCChildPattern.FindStringSubmatch(line); len(match) == 3 && len(units) > 0 {
				pageFrom, _ := strconv.Atoi(match[2])
				title := strings.TrimSpace(match[1])
				if title != "" && !strings.Contains(removeTextbookWhitespace(title), "目录") {
					last := &units[len(units)-1]
					last.Children = append(last.Children, extractedTextbookLesson{
						Title: title, PageFrom: pageFrom,
					})
				}
			}
		}
	}
	return units, nil
}

func trailingPrintedPageAnchor(content string) (logicalPage, from, to int, ok bool) {
	end := len(content)
	for end > 0 && isTextbookASCIIWhitespace(content[end-1]) {
		end--
	}
	if end == 0 {
		return 0, 0, 0, false
	}
	start := strings.LastIndexByte(content[:end], '\n') + 1
	for start < end && isTextbookASCIIWhitespace(content[start]) {
		start++
	}
	if start == end {
		return 0, 0, 0, false
	}
	for index := start; index < end; index++ {
		if content[index] < '0' || content[index] > '9' {
			return 0, 0, 0, false
		}
	}
	page, err := strconv.Atoi(content[start:end])
	if err != nil || page < 1 {
		return 0, 0, 0, false
	}
	return page, start, end, true
}

func isTextbookASCIIWhitespace(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n', '\f':
		return true
	default:
		return false
	}
}

func normalizeTextbookEvidence(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("·•._-—（）()《》", r) {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
}

func removeTextbookWhitespace(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
}

func textbookContentDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func validTextbookSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func hasEmptyOrDuplicateTextbookString(values []string) bool {
	for index, value := range values {
		if strings.TrimSpace(value) == "" || (index > 0 && value == values[index-1]) {
			return true
		}
	}
	return false
}

func stableTextbookCatalogID(prefix string, values ...string) string {
	encoded, _ := json.Marshal(values)
	digest := sha256.Sum256(encoded)
	return prefix + "-" + hex.EncodeToString(digest[:8])
}
