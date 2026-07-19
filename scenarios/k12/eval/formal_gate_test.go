package eval

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func formalGateReport(t *testing.T) (Report, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := Report{
		Split: "holdout", Provider: "provider", Model: "model",
		HoldoutManifestSHA: strings.Repeat("a", 64), GeneratedAt: "2026-07-19T00:00:00Z",
		QualityGate: &QualityGate{
			Mode: QualityGateFormal, Passed: true,
			Subjects: []SubjectGateEvidence{{
				Subject: "数学", RealPhotoSamples: 100,
				Coverage: 0.96, CoverageCI95Lower: 0.91,
				WeightedRisk: 0.01, WeightedRiskCI95Upper: 0.019,
				Thresholds: FormalGateThresholds{MinRealPhotoSamples: 100, MinCoverageCI95Lower: 0.90, MaxWeightedRiskCI95Upper: 0.02},
			}},
		},
	}
	signed, err := SignReport(r, "release-key-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	return signed, pub, priv
}

func TestFormalQualityGateRequires100PerSubjectCIAndSignature(t *testing.T) {
	r, pub, _ := formalGateReport(t)
	if ok, reasons := r.PassesFormalQualityGate([]string{"数学"}, map[string]ed25519.PublicKey{"release-key-1": pub}); !ok {
		t.Fatalf("valid formal gate rejected: %v", reasons)
	}

	r.QualityGate.Subjects[0].RealPhotoSamples = 99
	if ok, _ := r.PassesFormalQualityGate([]string{"数学"}, map[string]ed25519.PublicKey{"release-key-1": pub}); ok {
		t.Fatal("99 real photos must never pass the formal gate")
	}
}

func TestSmokeReportCanNeverClaimPassed(t *testing.T) {
	r, pub, priv := formalGateReport(t)
	r.QualityGate.Mode = QualityGateSmoke
	r.QualityGate.Passed = true
	r, err := SignReport(r, "release-key-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	if ok, reasons := r.PassesFormalQualityGate([]string{"数学"}, map[string]ed25519.PublicKey{"release-key-1": pub}); ok || len(reasons) == 0 {
		t.Fatal("smoke report must fail closed even when it writes passed=true")
	}
}

func TestFormalQualityGateRejectsTamperedSignedReport(t *testing.T) {
	r, pub, _ := formalGateReport(t)
	r.QualityGate.Subjects[0].CoverageCI95Lower = 0.99
	if ok, _ := r.PassesFormalQualityGate([]string{"数学"}, map[string]ed25519.PublicKey{"release-key-1": pub}); ok {
		t.Fatal("tampering after signature must be rejected")
	}
}

func TestLoadFormalQualityGateStrictlyVerifiesStoredReport(t *testing.T) {
	r, pub, _ := formalGateReport(t)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "formal.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFormalQualityGate(path, []string{"数学"}, map[string]ed25519.PublicKey{"release-key-1": pub})
	if err != nil || loaded.ReportID != r.ReportID {
		t.Fatalf("load verified formal report: id=%q err=%v", loaded.ReportID, err)
	}
}
