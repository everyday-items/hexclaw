package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPDFTextExtractionPreservesLayoutForFooterEvidence(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "pdftotext")
	const script = `#!/bin/sh
has_layout=0
for arg in "$@"; do
  if [ "$arg" = "-layout" ]; then has_layout=1; fi
done
if [ "$has_layout" -eq 1 ]; then
  printf '正文\n\n2\f'
else
  printf '正文\n2\n后文\f'
fi
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEXCLAW_PDFTOTEXT", fake)
	pdf := filepath.Join(tmp, "fixture.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pages, warning, err := extractPDFTextPagesFromPath(context.Background(), pdf, 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" || len(pages) != 1 || !strings.HasSuffix(pages[0], "2") {
		t.Fatalf("layout-preserving page text=%q warning=%q", pages, warning)
	}
}
