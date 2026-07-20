package knowledge

import (
	"errors"
	"testing"
)

func TestEmbeddingSelectionValidateStrictTaggedUnion(t *testing.T) {
	tests := []struct {
		name      string
		selection EmbeddingSelection
		wantErr   bool
	}{
		{name: "auto", selection: EmbeddingSelection{Kind: EmbeddingSelectionAuto}},
		{name: "fixed profile", selection: EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "local-bge-m3"}},
		{name: "disabled", selection: EmbeddingSelection{Kind: EmbeddingSelectionDisabled}},
		{name: "auto cannot carry profile", selection: EmbeddingSelection{Kind: EmbeddingSelectionAuto, ProfileID: "unexpected"}, wantErr: true},
		{name: "profile requires id", selection: EmbeddingSelection{Kind: EmbeddingSelectionProfile}, wantErr: true},
		{name: "disabled cannot carry profile", selection: EmbeddingSelection{Kind: EmbeddingSelectionDisabled, ProfileID: "unexpected"}, wantErr: true},
		{name: "unknown kind", selection: EmbeddingSelection{Kind: "cloud"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.selection.Validate()
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidSelection) {
					t.Fatalf("Validate() error = %v, want ErrInvalidSelection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}
}
