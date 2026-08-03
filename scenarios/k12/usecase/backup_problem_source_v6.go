package usecase

import (
	"errors"
	"fmt"
	"sort"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var ErrHexbakProblemSource = errors.New("hexbak problem-source archive invalid")

// ErrHexbakProblemSourceLiveWork is returned by restore-as before any write
// when the source closure still contains work that could enter provider I/O.
var ErrHexbakProblemSourceLiveWork = errors.New("hexbak problem-source archive contains live work")

func problemSourceArchiveEmpty(source k12storage.ProblemSourceArchiveV6) bool {
	return source.IsEmpty()
}

func PackHexbakProblemSourceAssets(
	agentName string,
	source *k12storage.ProblemSourceArchiveV6,
) ([]HexbakAsset, error) {
	if source == nil {
		return nil, nil
	}
	ids := make([]string, 0, len(source.PageAssets))
	seen := make(map[string]struct{}, len(source.PageAssets))
	for _, asset := range source.PageAssets {
		if asset.AgentName != agentName {
			return nil, fmt.Errorf("%w: PageAsset owner/header mismatch", ErrHexbakProblemSource)
		}
		if _, duplicate := seen[asset.PageAssetID]; duplicate {
			continue
		}
		seen[asset.PageAssetID] = struct{}{}
		ids = append(ids, asset.PageAssetID)
	}
	sort.Strings(ids)
	return packHexbakAssetIDs(agentName, ids)
}

func ValidateHexbakProblemSource(bak *Hexbak) error {
	if bak == nil {
		return fmt.Errorf("%w: nil archive", ErrHexbakProblemSource)
	}
	if bak.Version < 6 {
		if bak.ProblemSource != nil {
			return fmt.Errorf("%w: v%d source ledger is not checksum-covered", ErrHexbakProblemSource, bak.Version)
		}
		return nil
	}
	if bak.ProblemSource == nil {
		return nil
	}
	if err := k12storage.ValidateProblemSourceArchiveV6(bak.AgentName, *bak.ProblemSource); err != nil {
		return fmt.Errorf("%w: %v", ErrHexbakProblemSource, err)
	}
	packed := make(map[string]HexbakAsset, len(bak.Assets))
	for _, item := range bak.Assets {
		packed[item.AssetID] = item
	}
	for _, metadata := range bak.ProblemSource.PageAssets {
		item, ok := packed[metadata.PageAssetID]
		if !ok {
			return fmt.Errorf("%w: source PageAsset %q bytes are missing", ErrHexbakProblemSource, metadata.PageAssetID)
		}
		inspection, err := assetstore.Inspect(bak.AgentName, item.Data)
		if err != nil {
			return fmt.Errorf("%w: inspect source PageAsset %q: %v", ErrHexbakProblemSource, metadata.PageAssetID, err)
		}
		expectedWidth, expectedHeight := inspection.PixelWidth, inspection.PixelHeight
		if metadata.OrientationPolicy == string(k12storage.PageAssetOrientationVerified) {
			orientation, err := inspectPageAssetOrientation(inspection, item.Data)
			if err != nil {
				return fmt.Errorf("%w: inspect source PageAsset orientation %q: %v", ErrHexbakProblemSource, metadata.PageAssetID, err)
			}
			expectedWidth, expectedHeight = orientation.PixelWidth, orientation.PixelHeight
			if string(orientation.Policy) != metadata.OrientationPolicy ||
				orientation.PolicyVersion != metadata.OrientationPolicyVersion ||
				orientation.TransformChainJSON != metadata.TransformChainJSON {
				return fmt.Errorf("%w: source PageAsset %q orientation metadata/bytes mismatch", ErrHexbakProblemSource, metadata.PageAssetID)
			}
		}
		if inspection.AssetID != metadata.PageAssetID ||
			inspection.SHA256 != metadata.ContentDigest ||
			inspection.MediaType != metadata.MediaType ||
			inspection.SizeBytes != metadata.SizeBytes ||
			expectedWidth != metadata.PixelWidth ||
			expectedHeight != metadata.PixelHeight {
			return fmt.Errorf("%w: source PageAsset %q metadata/bytes mismatch", ErrHexbakProblemSource, metadata.PageAssetID)
		}
	}
	return nil
}
