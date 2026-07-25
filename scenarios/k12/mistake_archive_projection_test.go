package k12

import "testing"

func TestMistakeRestorableRequiresACompleteUnrestoredSnapshot(t *testing.T) {
	valid := MistakeFields{
		ArchivedReason:     MistakeArchivedReasonManual,
		ArchivedAt:         1000,
		ArchiveCommandID:   "archive-1",
		ArchivedFromStatus: StatusRetried,
		LastArchive: &MistakeArchiveSnapshot{
			Reason:           MistakeArchivedReasonManual,
			ArchivedAt:       1000,
			ArchiveCommandID: "archive-1",
			FromStatus:       StatusRetried,
		},
	}
	if !MistakeRestorable(StatusArchived, valid) {
		t.Fatal("complete archived snapshot must be restorable")
	}

	legacy := MistakeFields{}
	if MistakeRestorable(StatusArchived, legacy) {
		t.Fatal("legacy archived row without snapshot must not be restorable")
	}

	if MistakeRestorable(StatusRetried, valid) {
		t.Fatal("active record must not be restorable")
	}

	mismatched := valid
	mismatched.ArchiveCommandID = "other-command"
	if MistakeRestorable(StatusArchived, mismatched) {
		t.Fatal("mismatched current fields and snapshot must not be restorable")
	}
}
