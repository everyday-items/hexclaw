package api

import "testing"

func TestDD042AttachmentStagingUsesExact200MiBBoundary(t *testing.T) {
	const approvedSingleFileBytes int64 = 200 << 20
	if maxStagedAttachmentBytes != approvedSingleFileBytes {
		t.Fatalf("maxStagedAttachmentBytes = %d, want exact DD-042 boundary %d", maxStagedAttachmentBytes, approvedSingleFileBytes)
	}
}

func TestDD042AttachmentStagingPreservesAggregateGuard(t *testing.T) {
	const approvedAggregateBytes int64 = 512 << 20
	if maxStagedAttachmentTotalBytes != approvedAggregateBytes {
		t.Fatalf("maxStagedAttachmentTotalBytes = %d, want %d", maxStagedAttachmentTotalBytes, approvedAggregateBytes)
	}
}
