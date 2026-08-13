package render

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const approvedReferenceDOCXSHA256 = "bac3f1f4de7f145d966baeef39ce3a6f6d377c6137a475e602f629fcc7d6c2b2"

func TestReferenceDOCXReleaseBoundary(t *testing.T) {
	pathname := filepath.Join("assets", "reference.docx")
	data, err := os.ReadFile(pathname)
	if err != nil {
		t.Fatalf("reference DOCX must be readable: %v", err)
	}
	digest := sha256.Sum256(data)
	if actual := hex.EncodeToString(digest[:]); actual != approvedReferenceDOCXSHA256 {
		t.Fatal("reference DOCX must remain byte-identical to the approved asset")
	}

	reader, err := zip.OpenReader(pathname)
	if err != nil {
		t.Fatalf("reference DOCX must be a valid ZIP package: %v", err)
	}
	defer reader.Close()

	parts := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		parts[file.Name] = file
	}
	for _, required := range []string{
		"[Content_Types].xml",
		"_rels/.rels",
		"docProps/core.xml",
		"word/document.xml",
		"word/styles.xml",
	} {
		if parts[required] == nil {
			t.Errorf("reference DOCX is missing required part %s", required)
		}
	}

	t.Run("known Pandoc relationship shape", func(t *testing.T) {
		externalRelationships := 0
		for name, file := range parts {
			if !strings.HasSuffix(name, ".rels") {
				continue
			}
			data := readDOCXPart(t, file)
			decoder := xml.NewDecoder(strings.NewReader(string(data)))
			for {
				token, decodeErr := decoder.Token()
				if decodeErr == io.EOF {
					break
				}
				if decodeErr != nil {
					t.Fatalf("relationship part %s must be valid XML: %v", name, decodeErr)
				}
				start, ok := token.(xml.StartElement)
				if !ok || start.Name.Local != "Relationship" {
					continue
				}
				for _, attribute := range start.Attr {
					if attribute.Name.Local == "TargetMode" && attribute.Value == "External" {
						externalRelationships++
					}
				}
			}
		}
		if externalRelationships == 0 {
			t.Fatal("reference DOCX must retain its approved external relationship shape")
		}
	})

	t.Run("known Pandoc metadata shape", func(t *testing.T) {
		core := parts["docProps/core.xml"]
		if core == nil {
			t.Skip("required-part assertion already reports missing core properties")
		}
		knownFields := map[string]bool{"title": false, "creator": false}
		decoder := xml.NewDecoder(strings.NewReader(string(readDOCXPart(t, core))))
		for {
			token, decodeErr := decoder.Token()
			if decodeErr == io.EOF {
				break
			}
			if decodeErr != nil {
				t.Fatalf("core properties must be valid XML: %v", decodeErr)
			}
			start, ok := token.(xml.StartElement)
			if !ok {
				continue
			}
			if _, known := knownFields[start.Name.Local]; !known {
				continue
			}
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				t.Fatalf("core property %s must be readable: %v", start.Name.Local, err)
			}
			knownFields[start.Name.Local] = strings.TrimSpace(value) != ""
		}
		for name, present := range knownFields {
			if !present {
				t.Errorf("reference DOCX must retain the approved %s metadata shape", name)
			}
		}
	})

	t.Run("document styles and section properties remain readable", func(t *testing.T) {
		if parts["docProps/custom.xml"] == nil {
			t.Fatal("reference DOCX must retain the approved custom metadata part")
		}
		for _, name := range []string{"word/document.xml", "word/styles.xml", "docProps/custom.xml"} {
			decoder := xml.NewDecoder(strings.NewReader(string(readDOCXPart(t, parts[name]))))
			sectionProperties := false
			for {
				token, decodeErr := decoder.Token()
				if decodeErr == io.EOF {
					break
				}
				if decodeErr != nil {
					t.Fatalf("story part %s must be valid XML: %v", name, decodeErr)
				}
				if start, ok := token.(xml.StartElement); ok && start.Name.Local == "sectPr" {
					sectionProperties = true
				}
			}
			if name == "word/document.xml" && !sectionProperties {
				t.Fatal("reference DOCX must retain section properties")
			}
		}
	})
}

func readDOCXPart(t *testing.T, file *zip.File) []byte {
	t.Helper()
	reader, err := file.Open()
	if err != nil {
		t.Fatalf("DOCX part %s must open: %v", file.Name, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("DOCX part %s must be readable: %v", file.Name, err)
	}
	return data
}
