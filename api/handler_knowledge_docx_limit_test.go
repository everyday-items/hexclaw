package api

import (
	"bytes"
	"errors"
	"testing"
)

func TestReadDOCXXMLLimitedRejectsInsteadOfSilentlyTruncating(t *testing.T) {
	const limit = int64(16)

	got, err := readDOCXXMLLimited(bytes.NewReader(bytes.Repeat([]byte{'x'}, int(limit)+1)), limit)
	if !errors.Is(err, errDOCXXMLTooLarge) {
		t.Fatalf("error = %v, want errDOCXXMLTooLarge", err)
	}
	if got != nil {
		t.Fatalf("oversized DOCX XML must not return truncated bytes: %q", got)
	}
}

func TestReadDOCXXMLLimitedAcceptsContentAtLimit(t *testing.T) {
	const limit = int64(16)
	want := bytes.Repeat([]byte{'x'}, int(limit))

	got, err := readDOCXXMLLimited(bytes.NewReader(want), limit)
	if err != nil {
		t.Fatalf("read at exact limit: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content changed: got %q want %q", got, want)
	}
}
