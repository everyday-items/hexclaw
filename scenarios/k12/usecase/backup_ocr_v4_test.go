package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

func TestHexbakCurrentVersionIsV5ForProblemAttemptAndConfirmedCreativeWorkOCREvidence(t *testing.T) {
	if HexbakVersion != 5 {
		t.Fatalf("HexbakVersion=%d want 5 so Problem/Attempt and confirmed OCR are checksum-covered", HexbakVersion)
	}
}

func TestHexbakV4CreativeWorkOCREvidenceIsChecksumCoveredAndExactSet(t *testing.T) {
	valid := v4CreativeWorkOCRArchive(t, "mingming")
	if err := VerifyHexbak(valid); err != nil {
		t.Fatalf("valid v4 OCR evidence must verify: %v", err)
	}

	t.Run("checksum covers evidence", func(t *testing.T) {
		bak := cloneOCRArchiveForTest(t, valid)
		bak.CreativeWorkOCR[0].OCRRaw = "被篡改的 OCR raw"
		if err := VerifyHexbak(bak); !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("tampered signed OCR evidence err=%v want checksum mismatch", err)
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*Hexbak)
	}{
		{name: "missing ledger entry", mutate: func(b *Hexbak) { b.CreativeWorkOCR = nil }},
		{name: "canonical differs from work snapshot", mutate: func(b *Hexbak) {
			b.CreativeWorkOCR[0].ContentMarkdown = "另一份确认稿"
			b.CreativeWorkOCR[0].ContentDigest = digestText("另一份确认稿")
		}},
		{name: "cross owner evidence", mutate: func(b *Hexbak) {
			b.CreativeWorkOCR[0].AgentName = "another-child"
		}},
		{name: "work feedback points stale digest", mutate: func(b *Hexbak) {
			fields, err := k12.ParseCreativeWorkFields(b.Records[0].Fields)
			if err != nil {
				t.Fatal(err)
			}
			fields.Versions[0].StructuredFeedback.EvidenceRefs[0] =
				"ocr-confirmed:cwocr-source:v1:sha256:" + strings.Repeat("0", 64)
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			b.Records[0].Fields = string(raw)
		}},
		{name: "extra unreferenced evidence", mutate: func(b *Hexbak) {
			extra := b.CreativeWorkOCR[0]
			extra.Version = 2
			extra.ContentMarkdown = "未被作品引用的确认稿"
			extra.ContentDigest = digestText(extra.ContentMarkdown)
			b.CreativeWorkOCR = append(b.CreativeWorkOCR, extra)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bak := cloneOCRArchiveForTest(t, valid)
			tc.mutate(bak)
			if err := SealHexbak(bak); err != nil {
				t.Fatal(err)
			}
			if err := VerifyHexbak(bak); err == nil {
				t.Fatal("semantically invalid OCR evidence passed after attacker recomputed checksum")
			}
		})
	}
}

func TestMigrateHexbakOwnerUpgradesV3InlineOCREvidenceAndRewritesResolvableJobRef(t *testing.T) {
	source := v4CreativeWorkOCRArchive(t, "source-child")
	source.Version = 3
	source.ArchiveID = ""
	source.CreativeWorkOCR = nil
	if err := SealHexbak(source); err != nil {
		t.Fatal(err)
	}
	if err := VerifyHexbak(source); err != nil {
		t.Fatalf("legacy signed v3 must remain readable: %v", err)
	}

	migrated, err := MigrateHexbakOwner(source, "target-child")
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != 5 || len(migrated.CreativeWorkOCR) != 1 {
		t.Fatalf("migrated v3 archive=%+v want one current-version confirmed OCR evidence", migrated)
	}
	fields, err := k12.ParseCreativeWorkFields(migrated.Records[0].Fields)
	if err != nil {
		t.Fatal(err)
	}
	version := fields.Versions[0]
	evidence := migrated.CreativeWorkOCR[0]
	if evidence.AgentName != "target-child" || evidence.JobID == "cwocr-source" ||
		version.OCRJobID != evidence.JobID || version.SourceAssetID != evidence.SourceAssetID {
		t.Fatalf("owner/job/asset refs were not migrated together: version=%+v evidence=%+v", version, evidence)
	}
	if version.StructuredFeedback == nil || !containsExact(
		version.StructuredFeedback.EvidenceRefs,
		"ocr-confirmed:"+evidence.JobID+":v1:sha256:"+evidence.ContentDigest,
	) {
		t.Fatalf("existing WorkFeedback OCR ref was not rewritten: %+v", version.StructuredFeedback)
	}
	if !containsExact(
		version.StructuredFeedback.EvidenceRefs,
		"asset-ref:sha256:"+digestText(version.SourceAssetID),
	) {
		t.Fatalf("existing WorkFeedback asset ref was not rewritten: %+v", version.StructuredFeedback.EvidenceRefs)
	}
	if err := VerifyHexbak(migrated); err != nil {
		t.Fatalf("upgraded current archive must verify: %v", err)
	}
	sourceFields, _ := k12.ParseCreativeWorkFields(source.Records[0].Fields)
	if sourceFields.Versions[0].OCRJobID != "cwocr-source" || len(source.CreativeWorkOCR) != 0 {
		t.Fatal("source v3 archive was mutated")
	}
}

func TestBackupV4PacksOnlyConfirmedOCREvidenceReferencedByCreativeWork(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	d, store := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	ctx := context.Background()
	image := []byte("\x89PNG\r\n\x1a\nbackup-ocr-v4")
	assetID, err := assetstore.Save("mingming", image)
	if err != nil {
		t.Fatal(err)
	}

	confirmed, _, err := store.CreateCreativeWorkOCRJob(ctx, "mingming", "confirmed-request", assetID, digestBytes(image), 10)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err = store.MarkCreativeWorkOCRProcessing(ctx, "mingming", confirmed.JobID, 11)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err = store.MarkCreativeWorkOCRAwaiting(ctx, "mingming", confirmed.JobID, "OCR 原文", 12)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err = store.ConfirmCreativeWorkOCR(ctx, "mingming", confirmed.JobID, "家长确认稿", digestText("家长确认稿"), 13)
	if err != nil {
		t.Fatal(err)
	}
	// A confirmed but unreferenced draft and a pending job are operational
	// leftovers, not canonical archive evidence.
	if _, _, err := store.CreateCreativeWorkOCRJob(ctx, "mingming", "pending-request", assetID, digestBytes(image), 20); err != nil {
		t.Fatal(err)
	}
	unreferenced, _, err := store.CreateCreativeWorkOCRJob(ctx, "mingming", "unreferenced-request", assetID, digestBytes(image), 21)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkCreativeWorkOCRProcessing(ctx, "mingming", unreferenced.JobID, 22); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkCreativeWorkOCRAwaiting(ctx, "mingming", unreferenced.JobID, "unused raw", 23); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmCreativeWorkOCR(ctx, "mingming", unreferenced.JobID, "unused confirmed", digestText("unused confirmed"), 24); err != nil {
		t.Fatal(err)
	}

	work, err := k12.NewCreativeWorkRecord("mingming", "session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "春天", Task: "写景",
		Versions: []k12.CreativeWorkVersion{{
			VersionID: "v1", SourceAssetID: assetID, ContentMarkdown: confirmed.ConfirmedContent,
			OCRJobID: confirmed.JobID, OCRRaw: confirmed.OCRRaw, OCRVersion: confirmed.ConfirmedVersion,
			OCRConfirmedDigest: confirmed.ConfirmedDigest, ContentConfirmedAt: confirmed.ConfirmedAt,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, work); err != nil {
		t.Fatal(err)
	}

	bak, err := d.Backup(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(bak.CreativeWorkOCR) != 1 || bak.CreativeWorkOCR[0].JobID != confirmed.JobID {
		t.Fatalf("packed OCR evidence=%+v want only referenced confirmed version", bak.CreativeWorkOCR)
	}
	if err := VerifyHexbak(bak); err != nil {
		t.Fatalf("backup v4 must verify: %v", err)
	}
}

func v4CreativeWorkOCRArchive(t *testing.T, agent string) *Hexbak {
	t.Helper()
	image := []byte("\x89PNG\r\n\x1a\ncreative-work-ocr-v4")
	assetID, mime, sourceDigest, err := assetstore.Describe(agent, image)
	if err != nil {
		t.Fatal(err)
	}
	content := "春天的校园\n柳枝像绿色丝带。"
	contentDigest := digestText(content)
	version := k12.CreativeWorkVersion{
		VersionID: "v1", SourceAssetID: assetID, ContentMarkdown: content,
		OCRJobID: "cwocr-source", OCRRaw: "春天的校园\n柳枝象绿色丝带。",
		OCRVersion: 1, OCRConfirmedDigest: contentDigest, ContentConfirmedAt: 101,
		Feedback: "这句话的比喻很清楚；建议补充柳枝随风移动的细节。",
	}
	structured := buildStructuredWorkFeedback(
		k12.WorkTypeWriting, version, version.Feedback, k12.FeedbackSourceAI,
		"writing-feedback@1.0.0/embedded",
	)
	version.StructuredFeedback = &structured
	version.FeedbackSource = k12.FeedbackSourceAI
	version.FeedbackSkill = "writing-feedback@1.0.0/embedded"
	rec, err := k12.NewCreativeWorkRecord(agent, "session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "春天", Task: "写景",
		Versions: []k12.CreativeWorkVersion{version},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordID = "work-ocr-v4"
	rec.DedupeKey = "work-ocr-v4"
	rec.SchemaVersion = 1
	rec.Version = 1
	rec.Tags = "[]"
	rec.CreatedAt = 100
	rec.UpdatedAt = 101
	bak := &Hexbak{
		Version: 4, AgentName: agent, ExportedAt: 200,
		Records: []*records.AgentRecord{rec},
		Assets: []HexbakAsset{{
			AssetID: assetID, OwnerAgent: agent, SHA256: sourceDigest, MIME: mime, Data: image,
		}},
		CreativeWorkOCR: []k12.CreativeWorkOCRArchiveEvidence{{
			JobID: "cwocr-source", AgentName: agent, RequestID: "request-source",
			SourceAssetID: assetID, SourceDigest: sourceDigest,
			OCRRaw: "春天的校园\n柳枝象绿色丝带。", Version: 1,
			ContentMarkdown: content, ContentDigest: contentDigest, ConfirmedAt: 101,
			AttemptCount: 1, JobCreatedAt: 90, JobLastUpdatedAt: 101,
		}},
	}
	if err := SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	return bak
}

func digestText(value string) string { return digestBytes([]byte(value)) }

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func cloneOCRArchiveForTest(t *testing.T, source *Hexbak) *Hexbak {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone Hexbak
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
