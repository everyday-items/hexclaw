package k12storage

import "time"

// TextbookManifestLifecycleEvent is the K12 storage boundary for one immutable
// Knowledge document generation. Adapters translate upstream lifecycle events
// into this pure DTO.
type TextbookManifestLifecycleEvent struct {
	OwnerID            string
	CorpusUID          string
	DocumentID         string
	DocumentGeneration int64
	At                 time.Time
}
