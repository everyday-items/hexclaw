package k12

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// TextbookGroundingPageRef freezes one verified logical/PDF page mapping and
// the exact Knowledge segment references that are allowed to support it.
type TextbookGroundingPageRef struct {
	LogicalPage int
	PDFPage     int
	SegmentRefs []string
}

// TextbookGroundingScope is the durable read projection of one active A02
// binding. It contains only server-owned manifest facts and is copied into a
// request snapshot before any Knowledge query begins.
type TextbookGroundingScope struct {
	TextbookBindingID  string
	TextbookManifestID string
	DocumentID         string
	DocumentGeneration int64
	SourceDigest       string
	Edition            string
	Volume             string
	SegmentRefs        []string
	PageRefs           []TextbookGroundingPageRef
}

// ValidateTextbookGroundingScope rejects any incomplete, normalized-by-guess,
// or non-exact manifest projection. In particular, the global segment
// whitelist must be exactly the union of the per-page whitelists: callers may
// neither silently drop malformed rows nor introduce a segment that has no
// verified page mapping.
func ValidateTextbookGroundingScope(scope TextbookGroundingScope) error {
	for name, value := range map[string]string{
		"binding_id":  scope.TextbookBindingID,
		"manifest_id": scope.TextbookManifestID,
		"document_id": scope.DocumentID,
		"edition":     scope.Edition,
		"volume":      scope.Volume,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("invalid textbook grounding %s", name)
		}
	}
	if scope.DocumentGeneration <= 0 {
		return fmt.Errorf("invalid textbook grounding document generation")
	}
	if !validGroundingSHA256(scope.SourceDigest) {
		return fmt.Errorf("invalid textbook grounding source digest")
	}
	if len(scope.SegmentRefs) == 0 || len(scope.PageRefs) == 0 {
		return fmt.Errorf("incomplete textbook grounding exact set")
	}

	globalSegments := make(map[string]struct{}, len(scope.SegmentRefs))
	for _, segmentRef := range scope.SegmentRefs {
		if segmentRef == "" || segmentRef != strings.TrimSpace(segmentRef) {
			return fmt.Errorf("invalid textbook grounding segment reference")
		}
		if _, duplicate := globalSegments[segmentRef]; duplicate {
			return fmt.Errorf("duplicate textbook grounding segment reference")
		}
		globalSegments[segmentRef] = struct{}{}
	}

	pageSegments := make(map[string]struct{}, len(scope.SegmentRefs))
	logicalPages := make(map[int]struct{}, len(scope.PageRefs))
	physicalPages := make(map[int]struct{}, len(scope.PageRefs))
	for _, pageRef := range scope.PageRefs {
		if pageRef.LogicalPage < 1 || pageRef.PDFPage < 1 || len(pageRef.SegmentRefs) == 0 {
			return fmt.Errorf("invalid textbook grounding page reference")
		}
		if _, duplicate := logicalPages[pageRef.LogicalPage]; duplicate {
			return fmt.Errorf("duplicate textbook grounding logical page")
		}
		if _, duplicate := physicalPages[pageRef.PDFPage]; duplicate {
			return fmt.Errorf("duplicate textbook grounding physical page")
		}
		logicalPages[pageRef.LogicalPage] = struct{}{}
		physicalPages[pageRef.PDFPage] = struct{}{}

		segmentsOnPage := make(map[string]struct{}, len(pageRef.SegmentRefs))
		for _, segmentRef := range pageRef.SegmentRefs {
			if segmentRef == "" || segmentRef != strings.TrimSpace(segmentRef) {
				return fmt.Errorf("invalid textbook grounding page segment reference")
			}
			if _, duplicate := segmentsOnPage[segmentRef]; duplicate {
				return fmt.Errorf("duplicate textbook grounding page segment reference")
			}
			segmentsOnPage[segmentRef] = struct{}{}
			pageSegments[segmentRef] = struct{}{}
		}
	}
	if len(globalSegments) != len(pageSegments) {
		return fmt.Errorf("textbook grounding segment exact set differs from page mappings")
	}
	for segmentRef := range globalSegments {
		if _, exists := pageSegments[segmentRef]; !exists {
			return fmt.Errorf("textbook grounding segment %q has no page mapping", segmentRef)
		}
	}
	return nil
}

func validGroundingSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}
