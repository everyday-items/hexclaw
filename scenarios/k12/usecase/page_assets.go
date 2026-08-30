package usecase

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var ErrPageAssetIntegrity = errors.New("PageAsset bytes do not match ready metadata")

const pageAssetOrientationPolicyVersion = "source-pixel-exif-v1"

// DefaultLocalOwnerScope is the server-owned account boundary used by the
// single-user loopback Desktop composition. Network clients never supply it.
const DefaultLocalOwnerScope = "desktop-user"

// ReadyPageAsset is the only asset value exposed to K12 application services.
// Metadata and bytes are verified together; callers cannot observe a ready row
// independently from the content-addressed file it attests.
type ReadyPageAsset struct {
	Metadata k12storage.PageAssetMetadata
	Data     []byte
}

// PageAssetGateway is the owner-scoped application boundary shared by HTTP,
// IM, ImageTask creation and source-action processing.
type PageAssetGateway interface {
	Persist(context.Context, string, string, []byte) (ReadyPageAsset, error)
	OpenReady(context.Context, string, string, string) (ReadyPageAsset, error)
}

// PageAssetRepository coordinates the SQLite staging/ready ledger with the
// content-addressed filesystem. A file is never deleted as rollback: identical
// bytes may be shared by concurrent/replayed uploads, and a staging row can be
// reconciled safely after a crash.
type PageAssetRepository struct {
	Records *k12storage.Store

	Inspect func(string, []byte) (assetstore.AssetInspection, error)
	Ensure  func(string, []byte) (string, bool, error)
	Read    func(string, string) ([]byte, string, error)
}

func (r *PageAssetRepository) inspect(
	agentName string,
	data []byte,
) (assetstore.AssetInspection, error) {
	if r != nil && r.Inspect != nil {
		return r.Inspect(agentName, data)
	}
	return assetstore.Inspect(agentName, data)
}

func (r *PageAssetRepository) ensure(
	agentName string,
	data []byte,
) (string, bool, error) {
	if r != nil && r.Ensure != nil {
		return r.Ensure(agentName, data)
	}
	return assetstore.Ensure(agentName, data)
}

func (r *PageAssetRepository) read(
	agentName string,
	file string,
) ([]byte, string, error) {
	if r != nil && r.Read != nil {
		return r.Read(agentName, file)
	}
	return assetstore.Read(agentName, file)
}

type pageAssetOrientationFacts struct {
	Policy             k12storage.PageAssetOrientationPolicy
	PolicyVersion      string
	TransformChainJSON string
	PixelWidth         int
	PixelHeight        int
}

func inspectPageAssetOrientation(
	inspection assetstore.AssetInspection,
	data []byte,
) (pageAssetOrientationFacts, error) {
	orientation := 1
	var err error
	if inspection.MediaType == "image/jpeg" {
		orientation, err = jpegEXIFOrientation(data)
		if err != nil {
			return pageAssetOrientationFacts{}, fmt.Errorf("PageAsset EXIF orientation is invalid: %w", err)
		}
	}
	width, height := inspection.PixelWidth, inspection.PixelHeight
	if orientation >= 5 && orientation <= 8 {
		width, height = height, width
	}
	chain := []map[string]any{}
	if orientation != 1 {
		chain = append(chain, map[string]any{
			"operation":   "exif_orientation",
			"orientation": orientation,
		})
	}
	raw, err := json.Marshal(chain)
	if err != nil {
		return pageAssetOrientationFacts{}, err
	}
	return pageAssetOrientationFacts{
		Policy:             k12storage.PageAssetOrientationVerified,
		PolicyVersion:      pageAssetOrientationPolicyVersion,
		TransformChainJSON: string(raw),
		PixelWidth:         width,
		PixelHeight:        height,
	}, nil
}

// jpegEXIFOrientation returns the TIFF orientation (1..8). A JPEG without an
// EXIF orientation tag is already source orientation 1. Malformed EXIF is
// rejected instead of silently claiming that raw and displayed pixels align.
func jpegEXIFOrientation(data []byte) (int, error) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, errors.New("missing JPEG SOI")
	}
	orientation := 0
	for offset := 2; offset < len(data); {
		if data[offset] != 0xff {
			return 0, fmt.Errorf("invalid JPEG marker at byte %d", offset)
		}
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset >= len(data) {
			return 0, errors.New("truncated JPEG marker")
		}
		marker := data[offset]
		offset++
		switch marker {
		case 0xd9, 0xda: // EOI / start of entropy-coded scan.
			if orientation == 0 {
				return 1, nil
			}
			return orientation, nil
		case 0x01, 0xd0, 0xd1, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7:
			continue
		}
		if offset+2 > len(data) {
			return 0, errors.New("truncated JPEG segment length")
		}
		segmentLength := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		if segmentLength < 2 || offset+segmentLength > len(data) {
			return 0, errors.New("invalid JPEG segment length")
		}
		payload := data[offset+2 : offset+segmentLength]
		offset += segmentLength
		if marker != 0xe1 || len(payload) < 6 || string(payload[:6]) != "Exif\x00\x00" {
			continue
		}
		value, err := tiffOrientation(payload[6:])
		if err != nil {
			return 0, err
		}
		if value == 0 {
			continue
		}
		if orientation != 0 && orientation != value {
			return 0, errors.New("conflicting EXIF orientation tags")
		}
		orientation = value
	}
	if orientation == 0 {
		return 1, nil
	}
	return orientation, nil
}

func tiffOrientation(tiff []byte) (int, error) {
	if len(tiff) < 8 {
		return 0, errors.New("truncated EXIF TIFF header")
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, errors.New("invalid EXIF byte order")
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 0, errors.New("invalid EXIF TIFF magic")
	}
	ifdOffset := uint64(order.Uint32(tiff[4:8]))
	if ifdOffset > uint64(len(tiff)-2) {
		return 0, errors.New("EXIF IFD offset outside payload")
	}
	entryCount := uint64(order.Uint16(tiff[ifdOffset : ifdOffset+2]))
	entriesStart := ifdOffset + 2
	if entryCount > (uint64(len(tiff))-entriesStart)/12 {
		return 0, errors.New("truncated EXIF IFD entries")
	}
	for index := uint64(0); index < entryCount; index++ {
		start := entriesStart + index*12
		entry := tiff[start : start+12]
		if order.Uint16(entry[0:2]) != 0x0112 {
			continue
		}
		if order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) != 1 {
			return 0, errors.New("EXIF orientation has invalid type or count")
		}
		value := int(order.Uint16(entry[8:10]))
		if value < 1 || value > 8 {
			return 0, fmt.Errorf("EXIF orientation %d outside 1..8", value)
		}
		return value, nil
	}
	return 0, nil
}

func pageAssetMetadataFromInspection(
	ownerScope, agentName string,
	inspection assetstore.AssetInspection,
	orientation pageAssetOrientationFacts,
) k12storage.PageAssetMetadata {
	return k12storage.PageAssetMetadata{
		OwnerScope:               strings.TrimSpace(ownerScope),
		AgentName:                strings.TrimSpace(agentName),
		PageAssetID:              inspection.AssetID,
		ContentDigest:            inspection.SHA256,
		MediaType:                inspection.MediaType,
		SizeBytes:                inspection.SizeBytes,
		PixelWidth:               orientation.PixelWidth,
		PixelHeight:              orientation.PixelHeight,
		OrientationPolicy:        orientation.Policy,
		OrientationPolicyVersion: orientation.PolicyVersion,
		TransformChainJSON:       orientation.TransformChainJSON,
	}
}

func (r *PageAssetRepository) Persist(
	ctx context.Context,
	ownerScope, agentName string,
	data []byte,
) (ReadyPageAsset, error) {
	if r == nil || r.Records == nil {
		return ReadyPageAsset{}, errors.New("PageAsset repository is unavailable")
	}
	ownerScope = strings.TrimSpace(ownerScope)
	agentName = strings.TrimSpace(agentName)
	inspection, err := r.inspect(agentName, data)
	if err != nil {
		return ReadyPageAsset{}, err
	}
	orientation, err := inspectPageAssetOrientation(inspection, data)
	if err != nil {
		return ReadyPageAsset{}, err
	}
	identity := pageAssetMetadataFromInspection(
		ownerScope,
		agentName,
		inspection,
		orientation,
	)
	stored, _, err := r.Records.PreparePageAsset(ctx, identity)
	if err != nil {
		return ReadyPageAsset{}, err
	}
	switch stored.StorageState {
	case k12storage.PageAssetStorageReady:
		return r.OpenReady(ctx, ownerScope, agentName, stored.PageAssetID)
	case k12storage.PageAssetStorageFailed:
		stored, err = r.Records.RetryPageAssetStaging(
			ctx,
			ownerScope,
			agentName,
			stored.PageAssetID,
		)
		if err != nil {
			return ReadyPageAsset{}, err
		}
	case k12storage.PageAssetStorageCorrupt:
		stored, err = r.Records.RepairCorruptPageAssetStaging(
			ctx,
			ownerScope,
			agentName,
			stored.PageAssetID,
		)
		if err != nil {
			return ReadyPageAsset{}, err
		}
	}
	if stored.StorageState != k12storage.PageAssetStorageStaging {
		return ReadyPageAsset{}, fmt.Errorf(
			"%w: unexpected PageAsset state %s",
			k12storage.ErrPageAssetConflict,
			stored.StorageState,
		)
	}
	assetID, _, err := r.ensure(agentName, data)
	if err != nil {
		_, markErr := r.Records.MarkPageAssetFailed(
			context.WithoutCancel(ctx),
			ownerScope,
			agentName,
			stored.PageAssetID,
			err.Error(),
		)
		return ReadyPageAsset{}, errors.Join(err, markErr)
	}
	if assetID != stored.PageAssetID {
		identityErr := fmt.Errorf("%w: persisted asset identity drift", ErrPageAssetIntegrity)
		_, markErr := r.Records.MarkPageAssetFailed(
			context.WithoutCancel(ctx),
			ownerScope,
			agentName,
			stored.PageAssetID,
			identityErr.Error(),
		)
		return ReadyPageAsset{}, errors.Join(identityErr, markErr)
	}
	if _, err := r.Records.MarkPageAssetReady(
		ctx,
		ownerScope,
		agentName,
		stored.PageAssetID,
	); err != nil {
		// Keep the staging row and immutable file for restart reconciliation.
		return ReadyPageAsset{}, err
	}
	return r.OpenReady(ctx, ownerScope, agentName, stored.PageAssetID)
}

func (r *PageAssetRepository) OpenReady(
	ctx context.Context,
	ownerScope, agentName, pageAssetID string,
) (ReadyPageAsset, error) {
	if r == nil || r.Records == nil {
		return ReadyPageAsset{}, errors.New("PageAsset repository is unavailable")
	}
	ownerScope = strings.TrimSpace(ownerScope)
	agentName = strings.TrimSpace(agentName)
	pageAssetID = strings.TrimSpace(pageAssetID)
	metadata, err := r.Records.GetReadyPageAsset(
		ctx,
		ownerScope,
		agentName,
		pageAssetID,
	)
	if err != nil {
		return ReadyPageAsset{}, err
	}
	assetAgent, file, err := assetstore.Parse(pageAssetID)
	if err != nil || assetAgent != agentName {
		return ReadyPageAsset{}, r.markCorrupt(
			ctx,
			metadata,
			"ready PageAsset identity cannot be parsed",
		)
	}
	data, mediaType, err := r.read(agentName, file)
	if err != nil {
		return ReadyPageAsset{}, r.markCorrupt(
			ctx,
			metadata,
			"ready PageAsset file is missing or unreadable",
		)
	}
	inspection, err := r.inspect(agentName, data)
	if err != nil {
		return ReadyPageAsset{}, r.markCorrupt(
			ctx,
			metadata,
			"ready PageAsset bytes are not a valid image",
		)
	}
	orientation, err := inspectPageAssetOrientation(inspection, data)
	if err != nil {
		return ReadyPageAsset{}, r.markCorrupt(
			ctx,
			metadata,
			"ready PageAsset orientation is invalid",
		)
	}
	actual := pageAssetMetadataFromInspection(
		ownerScope,
		agentName,
		inspection,
		orientation,
	)
	if mediaType != metadata.MediaType ||
		actual.PageAssetID != metadata.PageAssetID ||
		actual.ContentDigest != metadata.ContentDigest ||
		actual.MediaType != metadata.MediaType ||
		actual.SizeBytes != metadata.SizeBytes ||
		actual.PixelWidth != metadata.PixelWidth ||
		actual.PixelHeight != metadata.PixelHeight ||
		actual.OrientationPolicy != metadata.OrientationPolicy ||
		actual.OrientationPolicyVersion != metadata.OrientationPolicyVersion ||
		actual.TransformChainJSON != metadata.TransformChainJSON {
		return ReadyPageAsset{}, r.markCorrupt(
			ctx,
			metadata,
			"ready PageAsset metadata does not match immutable bytes",
		)
	}
	return ReadyPageAsset{
		Metadata: metadata,
		Data:     append([]byte(nil), data...),
	}, nil
}

func (r *PageAssetRepository) markCorrupt(
	ctx context.Context,
	metadata k12storage.PageAssetMetadata,
	detail string,
) error {
	_, markErr := r.Records.MarkPageAssetCorrupt(
		context.WithoutCancel(ctx),
		metadata.OwnerScope,
		metadata.AgentName,
		metadata.PageAssetID,
		detail,
	)
	return errors.Join(ErrPageAssetIntegrity, markErr)
}
