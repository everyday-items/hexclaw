package usecase

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var ErrHexbakProblemAttempt = errors.New("hexbak Problem/Attempt ledger invalid")

var legacyVirtualPageID = regexp.MustCompile(`^page-[0-9a-f]{20}$`)

// PackHexbakProblemAttempts exports every V19 canonical submission owned by the
// Tutor. Operational GradingJob runtime files are not reconstructed here: stable
// Submission/Problem/Attempt IDs remain the durable identity boundary.
func PackHexbakProblemAttempts(
	ctx context.Context,
	store *k12storage.Store,
	agentName string,
) ([]k12.ProblemAttemptSnapshot, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: K12 store unavailable", ErrHexbakProblemAttempt)
	}
	snapshots, err := store.ExportProblemAttemptSnapshots(ctx, agentName)
	if err != nil {
		return nil, fmt.Errorf("%w: export: %v", ErrHexbakProblemAttempt, err)
	}
	return snapshots, nil
}

// ReferencedHexbakProblemAssetIDs returns the exact page-image set named by a
// Problem/Attempt ledger. A page must be a canonical owner-scoped asset:// ID.
func ReferencedHexbakProblemAssetIDs(
	agentName string,
	snapshots []k12.ProblemAttemptSnapshot,
) ([]string, error) {
	if err := k12storage.ValidateProblemAttemptArchive(agentName, snapshots); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHexbakProblemAttempt, err)
	}
	seen := make(map[string]struct{})
	for i, snapshot := range snapshots {
		for j, problem := range snapshot.Problems {
			if legacyVirtualPageID.MatchString(problem.PageAssetID) {
				// Webhook text submissions and pre-v5 photo facts used a stable page
				// identity without a Blob. Preserve that signed fact, but never invent
				// bytes or include it in the content manifest.
				continue
			}
			owner, _, err := assetstore.Parse(problem.PageAssetID)
			if err != nil || owner != agentName {
				return nil, fmt.Errorf("%w: problem_attempts[%d].problems[%d] page asset owner/header 不一致", ErrHexbakProblemAttempt, i, j)
			}
			seen[problem.PageAssetID] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// PackHexbakProblemAttemptAssets reads every V19 page image and verifies its
// content-address identity before the archive checksum is sealed.
func PackHexbakProblemAttemptAssets(
	agentName string,
	snapshots []k12.ProblemAttemptSnapshot,
) ([]HexbakAsset, error) {
	refs, err := ReferencedHexbakProblemAssetIDs(agentName, snapshots)
	if err != nil {
		return nil, err
	}
	return packHexbakAssetIDs(agentName, refs)
}

// ValidateHexbakProblemAttempts rejects v5-only facts in historical archives
// and validates owner, graph, immutable IDs, durable timestamps and page assets.
func ValidateHexbakProblemAttempts(bak *Hexbak) error {
	if bak == nil {
		return fmt.Errorf("%w: nil archive", ErrHexbakProblemAttempt)
	}
	if bak.Version < 5 {
		if len(bak.ProblemAttempts) != 0 {
			return fmt.Errorf("%w: v%d ledger is not checksum-covered", ErrHexbakProblemAttempt, bak.Version)
		}
		return nil
	}
	refs, err := ReferencedHexbakProblemAssetIDs(bak.AgentName, bak.ProblemAttempts)
	if err != nil {
		return err
	}
	assets := make(map[string]struct{}, len(bak.Assets))
	for _, item := range bak.Assets {
		assets[item.AssetID] = struct{}{}
	}
	for _, id := range refs {
		if _, ok := assets[id]; !ok {
			return fmt.Errorf("%w: page asset %q 未打包", ErrHexbakProblemAttempt, id)
		}
	}
	return nil
}

// migrateHexbakProblemAttempts rewrites only owner-scoped identity. V19 IDs are
// intentionally stable: photo SubmissionID is a SHA-1 content check used during
// restart, while Problem/Attempt composite keys already include agent_name.
func migrateHexbakProblemAttempts(
	source []k12.ProblemAttemptSnapshot,
	targetAgent string,
	assetIDs map[string]string,
) ([]k12.ProblemAttemptSnapshot, error) {
	out := cloneProblemAttemptSnapshots(source)
	for i := range out {
		for j := range out[i].Problems {
			problem := &out[i].Problems[j]
			if !legacyVirtualPageID.MatchString(problem.PageAssetID) {
				targetAssetID := assetIDs[problem.PageAssetID]
				if targetAssetID == "" {
					return nil, fmt.Errorf("%w: page asset %q has no target mapping", ErrHexbakProblemAttempt, problem.PageAssetID)
				}
				problem.PageAssetID = targetAssetID
			}
			problem.AgentName = targetAgent
		}
		for j := range out[i].Attempts {
			out[i].Attempts[j].AgentName = targetAgent
		}
	}
	if err := k12storage.ValidateProblemAttemptArchive(targetAgent, out); err != nil {
		return nil, fmt.Errorf("%w: migrated ledger: %v", ErrHexbakProblemAttempt, err)
	}
	return out, nil
}

func cloneProblemAttemptSnapshots(source []k12.ProblemAttemptSnapshot) []k12.ProblemAttemptSnapshot {
	out := make([]k12.ProblemAttemptSnapshot, len(source))
	for i, snapshot := range source {
		out[i].Problems = make([]k12.Problem, len(snapshot.Problems))
		for j, problem := range snapshot.Problems {
			out[i].Problems[j] = problem
			out[i].Problems[j].ConceptIDs = append([]string(nil), problem.ConceptIDs...)
			out[i].Problems[j].ConfirmationReasons = append([]string(nil), problem.ConfirmationReasons...)
			if problem.TranscriptionConfidence != nil {
				confidence := *problem.TranscriptionConfidence
				out[i].Problems[j].TranscriptionConfidence = &confidence
			}
		}
		out[i].Attempts = make([]k12.Attempt, len(snapshot.Attempts))
		for j, attempt := range snapshot.Attempts {
			out[i].Attempts[j] = attempt
			if attempt.BBox != nil {
				box := *attempt.BBox
				out[i].Attempts[j].BBox = &box
			}
		}
	}
	return out
}
