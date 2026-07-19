package eval

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type QualityGateMode string

const (
	QualityGateSmoke  QualityGateMode = "smoke"
	QualityGateFormal QualityGateMode = "formal"
)

type FormalGateThresholds struct {
	MinRealPhotoSamples      int     `json:"min_real_photo_samples"`
	MinCoverageCI95Lower     float64 `json:"min_coverage_ci95_lower"`
	MaxWeightedRiskCI95Upper float64 `json:"max_weighted_risk_ci95_upper"`
}

type SubjectGateEvidence struct {
	Subject               string               `json:"subject"`
	RealPhotoSamples      int                  `json:"real_photo_samples"`
	Coverage              float64              `json:"coverage"`
	CoverageCI95Lower     float64              `json:"coverage_ci95_lower"`
	WeightedRisk          float64              `json:"weighted_risk"`
	WeightedRiskCI95Upper float64              `json:"weighted_risk_ci95_upper"`
	Thresholds            FormalGateThresholds `json:"thresholds"`
}

type QualityGate struct {
	Mode     QualityGateMode       `json:"mode"`
	Passed   bool                  `json:"passed"`
	Subjects []SubjectGateEvidence `json:"subjects"`
}

type ReportSignature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"value"`
}

var officialFormalThresholds = FormalGateThresholds{
	MinRealPhotoSamples:      100,
	MinCoverageCI95Lower:     MinJudgmentCoverage,
	MaxWeightedRiskCI95Upper: MaxWeightedRisk,
}

func canonicalUnsignedReport(r Report) ([]byte, error) {
	r.ReportID = ""
	r.Signature = nil
	r.Suites = append([]SuiteResult(nil), r.Suites...)
	sort.Slice(r.Suites, func(i, j int) bool { return r.Suites[i].SuiteNo < r.Suites[j].SuiteNo })
	if r.QualityGate != nil {
		gate := *r.QualityGate
		gate.Subjects = append([]SubjectGateEvidence(nil), gate.Subjects...)
		sort.Slice(gate.Subjects, func(i, j int) bool { return gate.Subjects[i].Subject < gate.Subjects[j].Subject })
		r.QualityGate = &gate
	}
	return json.Marshal(r)
}

func SignReport(r Report, keyID string, privateKey ed25519.PrivateKey) (Report, error) {
	if strings.TrimSpace(keyID) == "" || len(privateKey) != ed25519.PrivateKeySize {
		return Report{}, fmt.Errorf("eval: invalid report signing key")
	}
	id, err := ComputeReportID(r)
	if err != nil {
		return Report{}, err
	}
	r.ReportID = id
	signature := ed25519.Sign(privateKey, []byte(id))
	r.Signature = &ReportSignature{
		Algorithm: "Ed25519", KeyID: keyID, Value: base64.RawStdEncoding.EncodeToString(signature),
	}
	return r, nil
}

func (r Report) verifySignature(publicKeys map[string]ed25519.PublicKey) error {
	if r.Signature == nil || r.Signature.Algorithm != "Ed25519" || strings.TrimSpace(r.Signature.KeyID) == "" {
		return fmt.Errorf("formal report missing Ed25519 signature")
	}
	key := publicKeys[r.Signature.KeyID]
	if len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("formal report signing key %q is not trusted", r.Signature.KeyID)
	}
	wantID, err := ComputeReportID(r)
	if err != nil {
		return err
	}
	if r.ReportID != wantID {
		return fmt.Errorf("formal report content digest mismatch: got %q want %q", r.ReportID, wantID)
	}
	signature, err := base64.RawStdEncoding.DecodeString(r.Signature.Value)
	if err != nil || !ed25519.Verify(key, []byte(r.ReportID), signature) {
		return fmt.Errorf("formal report signature verification failed")
	}
	return nil
}

func (r Report) PassesFormalQualityGate(requiredSubjects []string, publicKeys map[string]ed25519.PublicKey) (bool, []string) {
	var reasons []string
	if r.Split != "holdout" {
		reasons = append(reasons, "正式门只接受 blind holdout 报告")
	}
	if r.CaseLimitPerSuite != 0 {
		reasons = append(reasons, "正式门不得使用截断 case_limit")
	}
	if strings.TrimSpace(r.Provider) == "" || strings.TrimSpace(r.Model) == "" {
		reasons = append(reasons, "正式报告缺 provider/model snapshot")
	}
	if len(r.HoldoutManifestSHA) != 64 {
		reasons = append(reasons, "正式报告缺 64 位 holdout manifest sha256")
	} else if _, err := hex.DecodeString(r.HoldoutManifestSHA); err != nil {
		reasons = append(reasons, "holdout manifest sha256 非法")
	}
	if err := r.verifySignature(publicKeys); err != nil {
		reasons = append(reasons, err.Error())
	}
	if r.QualityGate == nil {
		return false, append(reasons, "报告缺 quality_gate")
	}
	gate := r.QualityGate
	if gate.Mode != QualityGateFormal {
		reasons = append(reasons, fmt.Sprintf("%s 报告只能作 smoke，不能翻正式门", gate.Mode))
	}
	if !gate.Passed {
		reasons = append(reasons, "报告未声明正式门通过")
	}
	want := make(map[string]bool, len(requiredSubjects))
	for _, subject := range requiredSubjects {
		if strings.TrimSpace(subject) == "" || want[subject] {
			reasons = append(reasons, "required subject exact-set 非法")
			continue
		}
		want[subject] = true
	}
	seen := make(map[string]bool, len(gate.Subjects))
	for _, subject := range gate.Subjects {
		if !want[subject.Subject] {
			reasons = append(reasons, fmt.Sprintf("正式门包含未请求学科 %q", subject.Subject))
		}
		if seen[subject.Subject] {
			reasons = append(reasons, fmt.Sprintf("正式门学科重复 %q", subject.Subject))
			continue
		}
		seen[subject.Subject] = true
		if subject.Thresholds != officialFormalThresholds {
			reasons = append(reasons, fmt.Sprintf("学科 %s 门槛不是批准的 100/90%%/2%%", subject.Subject))
		}
		if subject.RealPhotoSamples < officialFormalThresholds.MinRealPhotoSamples {
			reasons = append(reasons, fmt.Sprintf("学科 %s 真实照片 n=%d < 100", subject.Subject, subject.RealPhotoSamples))
		}
		if subject.Coverage < 0 || subject.Coverage > 1 || subject.CoverageCI95Lower < 0 ||
			subject.CoverageCI95Lower > subject.Coverage || subject.CoverageCI95Lower < officialFormalThresholds.MinCoverageCI95Lower {
			reasons = append(reasons, fmt.Sprintf("学科 %s coverage 95%% 下界未达门", subject.Subject))
		}
		if subject.WeightedRisk < 0 || subject.WeightedRiskCI95Upper < subject.WeightedRisk ||
			subject.WeightedRiskCI95Upper > officialFormalThresholds.MaxWeightedRiskCI95Upper {
			reasons = append(reasons, fmt.Sprintf("学科 %s weighted risk 95%% 上界未达门", subject.Subject))
		}
	}
	for subject := range want {
		if !seen[subject] {
			reasons = append(reasons, fmt.Sprintf("正式门缺学科 %q", subject))
		}
	}
	return len(reasons) == 0, reasons
}

// LoadFormalQualityGate is the release-gate loader. It strictly decodes the
// stored report, verifies its content-addressed ID and Ed25519 signature, then
// evaluates the approved per-subject sample and 95% CI thresholds.
func LoadFormalQualityGate(path string, requiredSubjects []string, publicKeys map[string]ed25519.PublicKey) (Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var report Report
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return Report{}, fmt.Errorf("解析正式 eval 报告: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("包含多余 JSON 值")
		}
		return Report{}, fmt.Errorf("解析正式 eval 报告: %w", err)
	}
	if ok, reasons := report.PassesFormalQualityGate(requiredSubjects, publicKeys); !ok {
		return Report{}, fmt.Errorf("正式 eval 门未通过: %s", strings.Join(reasons, "; "))
	}
	return report, nil
}
