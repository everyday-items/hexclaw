package engineadapter

import (
	"context"
	"database/sql"

	"github.com/hexagon-codes/hexclaw/knowledge"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type textbookManifestLifecycleProjector interface {
	ReconcileTextbookManifestLifecycle(
		context.Context,
		*sql.Tx,
		k12storage.TextbookManifestLifecycleEvent,
	) error
}

// TextbookManifestLifecycleAdapter translates the Knowledge lifecycle protocol
// at the adapter boundary while preserving the caller-owned transaction.
type TextbookManifestLifecycleAdapter struct {
	projector textbookManifestLifecycleProjector
}

var _ knowledge.DocumentIngestLifecycleObserver = (*TextbookManifestLifecycleAdapter)(nil)

// NewTextbookManifestLifecycleAdapter binds a K12 manifest projector to the
// Knowledge observer seam without leaking Knowledge types into K12 storage.
func NewTextbookManifestLifecycleAdapter(
	projector textbookManifestLifecycleProjector,
) *TextbookManifestLifecycleAdapter {
	return &TextbookManifestLifecycleAdapter{projector: projector}
}

func (a *TextbookManifestLifecycleAdapter) ReconcileDocumentIngestLifecycle(
	ctx context.Context,
	tx *sql.Tx,
	event knowledge.DocumentIngestLifecycleEvent,
) error {
	return a.projector.ReconcileTextbookManifestLifecycle(
		ctx,
		tx,
		k12storage.TextbookManifestLifecycleEvent{
			OwnerID:            event.OwnerID,
			CorpusUID:          event.CorpusUID,
			DocumentID:         event.DocumentID,
			DocumentGeneration: event.DocumentGeneration,
			At:                 event.At,
		},
	)
}
