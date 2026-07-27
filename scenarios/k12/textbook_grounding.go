package k12

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
	Edition            string
	Volume             string
	SegmentRefs        []string
	PageRefs           []TextbookGroundingPageRef
}
