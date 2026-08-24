package k12

import (
	"strings"
	"testing"
)

func annotatedFinalArtifactFixture() GradingFinalArtifact {
	artifact := GradingFinalArtifact{
		ArtifactID:                "grading-final-annotated",
		AgentName:                 "mingming",
		JobID:                     "grading-job-annotated",
		StructureVersion:          GradingFinalArtifactStructureVersion,
		CoverageStatus:            GradingFinalArtifactCoverageComplete,
		TotalCount:                1,
		PublishedCount:            1,
		OrderedCurrentDigestsJSON: `["assessment-digest"]`,
		CanonicalMarkdown:         "# 批改结果",
		SummaryInvocationID:       "summary-annotated",
		AnnotatedAssetOwnerScope:  "guardian-1",
		AnnotatedAssetID:          "asset://mingming/" + strings.Repeat("a", 64) + ".png",
		AnnotatedMIME:             "image/png",
		AnnotatedDigest:           strings.Repeat("a", 64),
		OriginalSourceDigest:      strings.Repeat("b", 64),
		CreatedAt:                 100,
		UpdatedAt:                 100,
	}
	artifact.ArtifactDigest = ComputeGradingFinalArtifactDigest(artifact)
	return artifact
}

func TestGradingFinalArtifactAnnotatedAssetIdentityIsCompleteAndDigestBound(t *testing.T) {
	valid := annotatedFinalArtifactFixture()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid annotated final artifact: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*GradingFinalArtifact)
	}{
		{"missing owner scope", func(v *GradingFinalArtifact) { v.AnnotatedAssetOwnerScope = "" }},
		{"missing asset identity", func(v *GradingFinalArtifact) { v.AnnotatedAssetID = "" }},
		{"cross agent identity", func(v *GradingFinalArtifact) {
			v.AnnotatedAssetID = "asset://lele/" + strings.Repeat("a", 64) + ".png"
		}},
		{"MIME drift", func(v *GradingFinalArtifact) { v.AnnotatedMIME = "image/jpeg" }},
		{"bytes digest drift", func(v *GradingFinalArtifact) { v.AnnotatedDigest = strings.Repeat("c", 64) }},
		{"source digest drift", func(v *GradingFinalArtifact) { v.OriginalSourceDigest = strings.Repeat("d", 64) }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			changed := valid
			tc.mutate(&changed)
			if err := changed.Validate(); err == nil {
				t.Fatal("annotated asset mutation must fail closed")
			}
		})
	}
}

func TestGradingFinalArtifactLegacyTextOnlyArtifactRemainsValid(t *testing.T) {
	artifact := annotatedFinalArtifactFixture()
	artifact.AnnotatedAssetOwnerScope = ""
	artifact.AnnotatedAssetID = ""
	artifact.AnnotatedMIME = ""
	artifact.AnnotatedDigest = ""
	artifact.OriginalSourceDigest = ""
	artifact.ArtifactDigest = strings.Repeat("e", 64)
	if err := artifact.Validate(); err != nil {
		t.Fatalf("legacy artifact without an annotated image must remain readable: %v", err)
	}
}
