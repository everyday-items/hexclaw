package engineadapter

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestAccumulationMetadataAdapterReturnsControlledFactsAndModelProvenance(t *testing.T) {
	tests := []struct {
		name                 string
		content              string
		response             string
		wantSource           string
		wantSourceProvenance bool
	}{
		{
			name:     "source unavailable",
			content:  "apple",
			response: `{"subject":"英语","entry_type":"词汇积累","source":""}`,
		},
		{
			name:                 "source derived from content",
			content:              "《静夜思》：床前明月光",
			response:             `{"subject":"语文","entry_type":"古诗积累","source":"《静夜思》"}`,
			wantSource:           "《静夜思》",
			wantSourceProvenance: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			adapter := NewAccumulationMetadataAdapter(func(
				_ context.Context, content string,
			) (string, error) {
				calls++
				if content != tc.content {
					t.Fatalf("generator content=%q, want %q", content, tc.content)
				}
				return tc.response, nil
			})
			got, err := adapter.DeriveAccumulationMetadata(
				context.Background(), tc.content,
			)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || got.Source != tc.wantSource {
				t.Fatalf("calls=%d metadata=%+v", calls, got)
			}
			for name, provenance := range map[string]k12.DerivationProvenance{
				"subject":    got.SubjectProvenance,
				"entry_type": got.EntryTypeProvenance,
			} {
				if provenance.Method != "model" ||
					provenance.Policy != "accumulation-metadata" ||
					provenance.Version != "1" {
					t.Fatalf("%s provenance=%+v", name, provenance)
				}
			}
			if (got.SourceProvenance != nil) != tc.wantSourceProvenance {
				t.Fatalf("source provenance=%+v", got.SourceProvenance)
			}
			if got.SourceProvenance != nil &&
				(got.SourceProvenance.Method != "model" ||
					got.SourceProvenance.Policy != "accumulation-metadata" ||
					got.SourceProvenance.Version != "1") {
				t.Fatalf("source provenance=%+v", got.SourceProvenance)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("derived metadata invalid: %v", err)
			}
		})
	}
}

func TestAccumulationMetadataAdapterRejectsUntrustedResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
	}{
		{
			name:     "unknown field",
			response: `{"subject":"英语","entry_type":"词汇积累","source":"","mastery":"mastered"}`,
		},
		{
			name:     "invalid taxonomy",
			response: `{"subject":"英语","entry_type":"grammar","source":""}`,
		},
		{
			name:     "trailing object",
			response: `{"subject":"英语","entry_type":"词汇积累","source":""}{}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := NewAccumulationMetadataAdapter(func(
				context.Context, string,
			) (string, error) {
				return tc.response, nil
			})
			if _, err := adapter.DeriveAccumulationMetadata(
				context.Background(), "apple",
			); err == nil {
				t.Fatal("untrusted response was accepted")
			}
		})
	}
}

func TestAccumulationMetadataAdapterRejectsMissingGeneratorAndPreservesProviderError(t *testing.T) {
	if _, err := NewAccumulationMetadataAdapter(nil).DeriveAccumulationMetadata(
		context.Background(), "apple",
	); err == nil {
		t.Fatal("missing generator was accepted")
	}
	want := errors.New("provider unavailable")
	adapter := NewAccumulationMetadataAdapter(func(
		context.Context, string,
	) (string, error) {
		return "", want
	})
	if _, err := adapter.DeriveAccumulationMetadata(
		context.Background(), "apple",
	); !errors.Is(err, want) {
		t.Fatalf("provider error identity lost: %v", err)
	}
}
