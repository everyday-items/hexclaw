package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var ErrHexbakCreativeWorkOCR = errors.New("hexbak creative-work OCR evidence invalid")

type creativeWorkOCRKey struct {
	jobID   string
	version int
}

// creativeWorkOCRReferences extracts the confirmed OCR snapshots promoted into
// canonical CreativeWorkVersion records. These are the exact evidence objects a
// v4 archive must carry: no missing reference and no unattached workflow job.
func creativeWorkOCRReferences(recs []*records.AgentRecord) ([]k12.CreativeWorkOCRArchiveEvidence, error) {
	byKey := map[creativeWorkOCRKey]k12.CreativeWorkOCRArchiveEvidence{}
	for _, rec := range recs {
		if rec == nil || rec.Collection != k12.CollectionCreativeWork {
			continue
		}
		fields, err := k12.ParseCreativeWorkFields(rec.Fields)
		if err != nil {
			return nil, fmt.Errorf("%w: parse work %q: %v", ErrHexbakCreativeWorkOCR, rec.RecordID, err)
		}
		for i, version := range fields.Versions {
			hasOCR := version.OCRJobID != "" || version.OCRRaw != "" || version.OCRVersion != 0 ||
				version.OCRConfirmedDigest != "" || version.ContentConfirmedAt != 0
			if !hasOCR {
				continue
			}
			content := strings.TrimSpace(version.ContentMarkdown)
			if fields.WorkType != k12.WorkTypeWriting || strings.TrimSpace(version.OCRJobID) == "" ||
				version.OCRVersion <= 0 || strings.TrimSpace(version.OCRConfirmedDigest) == "" ||
				version.ContentConfirmedAt <= 0 || content == "" || content != version.ContentMarkdown ||
				strings.TrimSpace(version.SourceAssetID) == "" {
				return nil, fmt.Errorf("%w: work %q version %d has incomplete confirmation", ErrHexbakCreativeWorkOCR, rec.RecordID, i+1)
			}
			if digestString(content) != version.OCRConfirmedDigest {
				return nil, fmt.Errorf("%w: work %q version %d content digest mismatch", ErrHexbakCreativeWorkOCR, rec.RecordID, i+1)
			}
			entry := k12.CreativeWorkOCRArchiveEvidence{
				JobID: version.OCRJobID, AgentName: rec.AgentName,
				SourceAssetID: version.SourceAssetID, OCRRaw: version.OCRRaw,
				Version: version.OCRVersion, ContentMarkdown: content,
				ContentDigest: version.OCRConfirmedDigest, ConfirmedAt: version.ContentConfirmedAt,
			}
			key := creativeWorkOCRKey{jobID: entry.JobID, version: entry.Version}
			if version.StructuredFeedback != nil {
				found := false
				for _, evidenceRef := range version.StructuredFeedback.EvidenceRefs {
					if !strings.HasPrefix(evidenceRef, "ocr-confirmed:") {
						continue
					}
					parsedKey, digest, ok := parseCreativeWorkOCREvidenceRef(evidenceRef)
					if !ok || parsedKey != key || digest != entry.ContentDigest {
						return nil, fmt.Errorf("%w: work %q version %d has stale/malformed feedback OCR ref", ErrHexbakCreativeWorkOCR, rec.RecordID, i+1)
					}
					found = true
				}
				if !found {
					return nil, fmt.Errorf("%w: work %q version %d feedback lacks confirmed OCR ref", ErrHexbakCreativeWorkOCR, rec.RecordID, i+1)
				}
			}
			if prior, ok := byKey[key]; ok {
				if prior.AgentName != entry.AgentName || prior.SourceAssetID != entry.SourceAssetID ||
					prior.OCRRaw != entry.OCRRaw || prior.ContentMarkdown != entry.ContentMarkdown ||
					prior.ContentDigest != entry.ContentDigest || prior.ConfirmedAt != entry.ConfirmedAt {
					return nil, fmt.Errorf("%w: conflicting snapshots for %s v%d", ErrHexbakCreativeWorkOCR, entry.JobID, entry.Version)
				}
				continue
			}
			byKey[key] = entry
		}
	}
	out := make([]k12.CreativeWorkOCRArchiveEvidence, 0, len(byKey))
	for _, item := range byKey {
		out = append(out, item)
	}
	sortCreativeWorkOCREvidence(out)
	return out, nil
}

func sortCreativeWorkOCREvidence(items []k12.CreativeWorkOCRArchiveEvidence) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].JobID != items[j].JobID {
			return items[i].JobID < items[j].JobID
		}
		return items[i].Version < items[j].Version
	})
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// PackHexbakCreativeWorkOCR loads only the confirmed versions named by
// canonical records and verifies the duplicated snapshot before sealing it.
func PackHexbakCreativeWorkOCR(
	ctx context.Context,
	store *k12storage.Store,
	agentName string,
	recs []*records.AgentRecord,
) ([]k12.CreativeWorkOCRArchiveEvidence, error) {
	if store == nil {
		refs, err := creativeWorkOCRReferences(recs)
		if err != nil || len(refs) == 0 {
			return nil, err
		}
		return nil, fmt.Errorf("%w: K12 store unavailable", ErrHexbakCreativeWorkOCR)
	}
	return PackHexbakCreativeWorkOCRWithResolver(ctx, agentName, recs, store.GetCreativeWorkOCRArchiveEvidence)
}

// CreativeWorkOCREvidenceResolver lets restore-as read the same canonical
// evidence from its already-open SQLite snapshot transaction.
type CreativeWorkOCREvidenceResolver func(
	ctx context.Context, agentName, jobID string, version int,
) (k12.CreativeWorkOCRArchiveEvidence, error)

func PackHexbakCreativeWorkOCRWithResolver(
	ctx context.Context,
	agentName string,
	recs []*records.AgentRecord,
	resolve CreativeWorkOCREvidenceResolver,
) ([]k12.CreativeWorkOCRArchiveEvidence, error) {
	refs, err := creativeWorkOCRReferences(recs)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if resolve == nil {
		return nil, fmt.Errorf("%w: evidence resolver unavailable", ErrHexbakCreativeWorkOCR)
	}
	out := make([]k12.CreativeWorkOCRArchiveEvidence, 0, len(refs))
	for _, ref := range refs {
		if ref.AgentName != agentName {
			return nil, fmt.Errorf("%w: work evidence belongs to %q", ErrHexbakCreativeWorkOCR, ref.AgentName)
		}
		item, err := resolve(ctx, agentName, ref.JobID, ref.Version)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve %s v%d: %v", ErrHexbakCreativeWorkOCR, ref.JobID, ref.Version, err)
		}
		if item.SourceAssetID != ref.SourceAssetID || item.OCRRaw != ref.OCRRaw ||
			item.ContentMarkdown != ref.ContentMarkdown || item.ContentDigest != ref.ContentDigest ||
			item.ConfirmedAt != ref.ConfirmedAt {
			return nil, fmt.Errorf("%w: ledger differs from work snapshot %s v%d", ErrHexbakCreativeWorkOCR, ref.JobID, ref.Version)
		}
		out = append(out, item)
	}
	sortCreativeWorkOCREvidence(out)
	return out, nil
}

// ValidateHexbakCreativeWorkOCR enforces the v4 exact-set contract even when
// an attacker recomputes the outer checksum after deleting or changing rows.
func ValidateHexbakCreativeWorkOCR(bak *Hexbak) error {
	if bak == nil {
		return fmt.Errorf("%w: nil archive", ErrHexbakCreativeWorkOCR)
	}
	if bak.Version < 4 {
		if len(bak.CreativeWorkOCR) != 0 {
			return fmt.Errorf("%w: v%d OCR ledger is not checksum-covered", ErrHexbakCreativeWorkOCR, bak.Version)
		}
		return nil
	}
	refs, err := creativeWorkOCRReferences(bak.Records)
	if err != nil {
		return err
	}
	if len(refs) != len(bak.CreativeWorkOCR) {
		return fmt.Errorf("%w: referenced=%d packed=%d", ErrHexbakCreativeWorkOCR, len(refs), len(bak.CreativeWorkOCR))
	}
	refByKey := make(map[creativeWorkOCRKey]k12.CreativeWorkOCRArchiveEvidence, len(refs))
	for _, ref := range refs {
		refByKey[creativeWorkOCRKey{ref.JobID, ref.Version}] = ref
	}
	assetDigest := make(map[string]string, len(bak.Assets))
	for _, item := range bak.Assets {
		assetDigest[item.AssetID] = item.SHA256
	}
	seen := make(map[creativeWorkOCRKey]struct{}, len(bak.CreativeWorkOCR))
	type stableJob struct {
		owner, requestID, assetID, sourceDigest, raw string
	}
	jobs := map[string]stableJob{}
	for i, item := range bak.CreativeWorkOCR {
		key := creativeWorkOCRKey{item.JobID, item.Version}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: duplicate %s v%d", ErrHexbakCreativeWorkOCR, item.JobID, item.Version)
		}
		seen[key] = struct{}{}
		ref, ok := refByKey[key]
		if !ok {
			return fmt.Errorf("%w: unreferenced %s v%d", ErrHexbakCreativeWorkOCR, item.JobID, item.Version)
		}
		if strings.TrimSpace(item.JobID) == "" || item.AgentName != bak.AgentName ||
			strings.TrimSpace(item.RequestID) == "" || item.Version <= 0 || item.ConfirmedAt <= 0 ||
			strings.TrimSpace(item.SourceAssetID) == "" || strings.TrimSpace(item.SourceDigest) == "" ||
			strings.TrimSpace(item.ContentMarkdown) == "" || item.ContentMarkdown != strings.TrimSpace(item.ContentMarkdown) ||
			digestString(item.ContentMarkdown) != item.ContentDigest || item.AttemptCount < 0 {
			return fmt.Errorf("%w: evidence[%d] fields invalid", ErrHexbakCreativeWorkOCR, i)
		}
		owner, _, parseErr := assetstore.Parse(item.SourceAssetID)
		if parseErr != nil || owner != bak.AgentName || assetDigest[item.SourceAssetID] != item.SourceDigest {
			return fmt.Errorf("%w: evidence[%d] source asset/digest invalid", ErrHexbakCreativeWorkOCR, i)
		}
		if item.AgentName != ref.AgentName || item.SourceAssetID != ref.SourceAssetID ||
			item.OCRRaw != ref.OCRRaw || item.ContentMarkdown != ref.ContentMarkdown ||
			item.ContentDigest != ref.ContentDigest || item.ConfirmedAt != ref.ConfirmedAt {
			return fmt.Errorf("%w: evidence[%d] differs from canonical work snapshot", ErrHexbakCreativeWorkOCR, i)
		}
		stable := stableJob{item.AgentName, item.RequestID, item.SourceAssetID, item.SourceDigest, item.OCRRaw}
		if prior, exists := jobs[item.JobID]; exists && prior != stable {
			return fmt.Errorf("%w: job %s identity/raw differs across versions", ErrHexbakCreativeWorkOCR, item.JobID)
		}
		jobs[item.JobID] = stable
	}
	return nil
}

// materializeLegacyCreativeWorkOCR upgrades signed v3 inline snapshots into
// the explicit v4 ledger. The source asset bytes/digest are already protected
// by the v3 checksum, so no model output is recreated or guessed.
func materializeLegacyCreativeWorkOCR(bak *Hexbak) ([]k12.CreativeWorkOCRArchiveEvidence, error) {
	refs, err := creativeWorkOCRReferences(bak.Records)
	if err != nil || len(refs) == 0 {
		return refs, err
	}
	if bak.Version < 3 {
		return nil, fmt.Errorf("%w: v%d does not carry signed asset bytes", ErrHexbakCreativeWorkOCR, bak.Version)
	}
	digests := make(map[string]string, len(bak.Assets))
	for _, item := range bak.Assets {
		digests[item.AssetID] = item.SHA256
	}
	for i := range refs {
		digest := digests[refs[i].SourceAssetID]
		if digest == "" {
			return nil, fmt.Errorf("%w: legacy source asset %q is not packed", ErrHexbakCreativeWorkOCR, refs[i].SourceAssetID)
		}
		refs[i].SourceDigest = digest
		refs[i].RequestID = archiveOCRRequestID(refs[i].AgentName, refs[i].JobID)
		refs[i].JobCreatedAt = refs[i].ConfirmedAt
		refs[i].JobLastUpdatedAt = refs[i].ConfirmedAt
	}
	return refs, nil
}

func materializeHexbakForRestore(source *Hexbak) (*Hexbak, error) {
	if source == nil || source.Version >= 4 {
		return cloneHexbak(source), nil
	}
	evidence, err := materializeLegacyCreativeWorkOCR(source)
	if err != nil {
		return nil, err
	}
	if len(evidence) == 0 {
		return cloneHexbak(source), nil
	}
	out := cloneHexbak(source)
	out.Version = HexbakVersion
	out.ArchiveID = ""
	out.CreativeWorkOCR = evidence
	if err := SealHexbak(out); err != nil {
		return nil, err
	}
	if err := VerifyHexbak(out); err != nil {
		return nil, err
	}
	return out, nil
}

func archiveOCRRequestID(agentName, jobID string) string {
	sum := sha256.Sum256([]byte(agentName + "\x00" + jobID))
	return "hexbak-ocr-" + hex.EncodeToString(sum[:16])
}

func migratedCreativeWorkOCRJobID(targetAgent, sourceJobID string) string {
	sum := sha256.Sum256([]byte(targetAgent + "\x00" + sourceJobID))
	return "cwocr-" + hex.EncodeToString(sum[:16])
}

func migrateCreativeWorkOCREvidence(
	source []k12.CreativeWorkOCRArchiveEvidence,
	targetAgent string,
	assetIDs map[string]string,
	jobIDs map[string]string,
) ([]k12.CreativeWorkOCRArchiveEvidence, error) {
	out := make([]k12.CreativeWorkOCRArchiveEvidence, len(source))
	for i, item := range source {
		assetID, ok := assetIDs[item.SourceAssetID]
		if !ok {
			return nil, fmt.Errorf("%w: source asset %q has no target mapping", ErrHexbakCreativeWorkOCR, item.SourceAssetID)
		}
		jobID := jobIDs[item.JobID]
		if jobID == "" {
			return nil, fmt.Errorf("%w: source job %q has no target mapping", ErrHexbakCreativeWorkOCR, item.JobID)
		}
		item.JobID = jobID
		item.AgentName = targetAgent
		item.RequestID = archiveOCRRequestID(targetAgent, jobID)
		item.SourceAssetID = assetID
		out[i] = item
	}
	sortCreativeWorkOCREvidence(out)
	return out, nil
}

func rewriteCreativeWorkOCRJobRefs(
	recs []*records.AgentRecord,
	jobIDs map[string]string,
) error {
	for _, rec := range recs {
		if rec == nil || rec.Collection != k12.CollectionCreativeWork {
			continue
		}
		fields, err := k12.ParseCreativeWorkFields(rec.Fields)
		if err != nil {
			return fmt.Errorf("%w: parse migrated work %q: %v", ErrHexbakCreativeWorkOCR, rec.RecordID, err)
		}
		changed := false
		for i := range fields.Versions {
			version := &fields.Versions[i]
			if version.OCRJobID == "" {
				continue
			}
			oldJobID := version.OCRJobID
			newJobID := jobIDs[oldJobID]
			if newJobID == "" {
				return fmt.Errorf("%w: work %q names unmapped job %q", ErrHexbakCreativeWorkOCR, rec.RecordID, oldJobID)
			}
			version.OCRJobID = newJobID
			changed = true
			if version.StructuredFeedback == nil {
				continue
			}
			oldPrefix := "ocr-confirmed:" + oldJobID + ":"
			for n, ref := range version.StructuredFeedback.EvidenceRefs {
				if strings.HasPrefix(ref, oldPrefix) {
					version.StructuredFeedback.EvidenceRefs[n] = "ocr-confirmed:" + newJobID + ":" + strings.TrimPrefix(ref, oldPrefix)
					continue
				}
				if strings.HasPrefix(ref, "asset-ref:sha256:") && version.SourceAssetID != "" {
					version.StructuredFeedback.EvidenceRefs[n] = "asset-ref:sha256:" + digestString(version.SourceAssetID)
				}
			}
		}
		if changed {
			encoded, err := json.Marshal(fields)
			if err != nil {
				return fmt.Errorf("%w: marshal migrated work %q: %v", ErrHexbakCreativeWorkOCR, rec.RecordID, err)
			}
			rec.Fields = string(encoded)
		}
	}
	return nil
}

func creativeWorkOCRAssetMapping(source, migrated *Hexbak) map[string]string {
	targetByDigest := make(map[string]string, len(migrated.Assets))
	for _, item := range migrated.Assets {
		targetByDigest[item.SHA256] = item.AssetID
	}
	out := make(map[string]string, len(source.Assets))
	for _, item := range source.Assets {
		if target := targetByDigest[item.SHA256]; target != "" {
			out[item.AssetID] = target
		}
	}
	return out
}

func creativeWorkOCRJobMapping(items []k12.CreativeWorkOCRArchiveEvidence, targetAgent string) map[string]string {
	out := map[string]string{}
	for _, item := range items {
		out[item.JobID] = migratedCreativeWorkOCRJobID(targetAgent, item.JobID)
	}
	return out
}

// parseCreativeWorkOCREvidenceRef is intentionally strict; it is used by
// restore tests and validation callers that need to dereference WorkFeedback.
func parseCreativeWorkOCREvidenceRef(ref string) (creativeWorkOCRKey, string, bool) {
	const prefix = "ocr-confirmed:"
	if !strings.HasPrefix(ref, prefix) {
		return creativeWorkOCRKey{}, "", false
	}
	rest := strings.TrimPrefix(ref, prefix)
	digestAt := strings.LastIndex(rest, ":sha256:")
	if digestAt <= 0 {
		return creativeWorkOCRKey{}, "", false
	}
	identity := rest[:digestAt]
	versionAt := strings.LastIndex(identity, ":v")
	if versionAt <= 0 {
		return creativeWorkOCRKey{}, "", false
	}
	version, err := strconv.Atoi(identity[versionAt+2:])
	digest := rest[digestAt+len(":sha256:"):]
	if err != nil || version <= 0 || digest == "" {
		return creativeWorkOCRKey{}, "", false
	}
	return creativeWorkOCRKey{identity[:versionAt], version}, digest, true
}
