package k12storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

var (
	ErrPageAssetNotFound = errors.New("page asset not found in owner scope")
	ErrPageAssetConflict = errors.New("page asset immutable identity or state conflict")
	ErrPageAssetInvalid  = errors.New("invalid page asset metadata")
)

type PageAssetStorageState string

const (
	PageAssetStorageStaging PageAssetStorageState = "staging"
	PageAssetStorageReady   PageAssetStorageState = "ready"
	PageAssetStorageFailed  PageAssetStorageState = "failed"
	PageAssetStorageCorrupt PageAssetStorageState = "corrupt"
)

type PageAssetOrientationPolicy string

const (
	// PageAssetOrientationUnverified is the honest default until an image
	// decoder has inspected and normalized orientation metadata. Decoding width
	// and height alone is not evidence that EXIF orientation was normalized.
	PageAssetOrientationUnverified PageAssetOrientationPolicy = "unverified"
	PageAssetOrientationVerified   PageAssetOrientationPolicy = "verified"
)

// PageAssetMetadata is the owner-scoped durable identity of one immutable,
// content-addressed source image. Mutable storage lifecycle fields are kept on
// the same row, but the V72 trigger prevents all identity metadata from being
// rewritten after PreparePageAsset commits.
type PageAssetMetadata struct {
	OwnerScope               string
	AgentName                string
	PageAssetID              string
	ContentDigest            string
	MediaType                string
	SizeBytes                int64
	PixelWidth               int
	PixelHeight              int
	OrientationPolicy        PageAssetOrientationPolicy
	OrientationPolicyVersion string
	TransformChainJSON       string
	StorageState             PageAssetStorageState
	ReadyAt                  int64
	LastError                string
	CreatedAt                int64
	UpdatedAt                int64
}

const pageAssetColumns = `owner_scope,agent_name,page_asset_id,content_digest,media_type,
    size_bytes,pixel_width,pixel_height,orientation_policy,orientation_policy_version,
    transform_chain_json,
    storage_state,ready_at,last_error,created_at,updated_at`

func scanPageAsset(row rowScanner) (PageAssetMetadata, error) {
	var asset PageAssetMetadata
	var orientationPolicy, storageState string
	err := row.Scan(
		&asset.OwnerScope,
		&asset.AgentName,
		&asset.PageAssetID,
		&asset.ContentDigest,
		&asset.MediaType,
		&asset.SizeBytes,
		&asset.PixelWidth,
		&asset.PixelHeight,
		&orientationPolicy,
		&asset.OrientationPolicyVersion,
		&asset.TransformChainJSON,
		&storageState,
		&asset.ReadyAt,
		&asset.LastError,
		&asset.CreatedAt,
		&asset.UpdatedAt,
	)
	asset.OrientationPolicy = PageAssetOrientationPolicy(orientationPolicy)
	asset.StorageState = PageAssetStorageState(storageState)
	return asset, err
}

func canonicalPageAssetTransformChain(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "[]"
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", fmt.Errorf("%w: transform_chain_json is not JSON", ErrPageAssetInvalid)
	}
	if _, ok := value.([]any); !ok {
		return "", fmt.Errorf("%w: transform_chain_json must be an array", ErrPageAssetInvalid)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: canonicalize transform_chain_json: %v", ErrPageAssetInvalid, err)
	}
	return string(canonical), nil
}

func normalizePageAssetIdentity(input PageAssetMetadata) (PageAssetMetadata, error) {
	asset := input
	asset.OwnerScope = strings.TrimSpace(asset.OwnerScope)
	asset.AgentName = strings.TrimSpace(asset.AgentName)
	asset.PageAssetID = strings.TrimSpace(asset.PageAssetID)
	asset.ContentDigest = strings.TrimSpace(asset.ContentDigest)
	asset.MediaType = strings.ToLower(strings.TrimSpace(asset.MediaType))
	asset.OrientationPolicy = PageAssetOrientationPolicy(
		strings.ToLower(strings.TrimSpace(string(asset.OrientationPolicy))),
	)
	asset.OrientationPolicyVersion = strings.TrimSpace(asset.OrientationPolicyVersion)
	if asset.OrientationPolicy == "" {
		asset.OrientationPolicy = PageAssetOrientationUnverified
	}
	if asset.OwnerScope == "" || asset.AgentName == "" || asset.PageAssetID == "" {
		return PageAssetMetadata{}, fmt.Errorf("%w: owner_scope, agent and page_asset_id are required", ErrPageAssetInvalid)
	}
	if len(asset.ContentDigest) != 64 ||
		asset.ContentDigest != strings.ToLower(asset.ContentDigest) {
		return PageAssetMetadata{}, fmt.Errorf("%w: content digest must be lowercase sha256 hex", ErrPageAssetInvalid)
	}
	decodedDigest, err := hex.DecodeString(asset.ContentDigest)
	if err != nil || len(decodedDigest) != 32 {
		return PageAssetMetadata{}, fmt.Errorf("%w: content digest must be lowercase sha256 hex", ErrPageAssetInvalid)
	}
	extension := ""
	switch asset.MediaType {
	case "image/png":
		extension = "png"
	case "image/jpeg":
		extension = "jpg"
	case "image/gif":
		extension = "gif"
	case "image/webp":
		extension = "webp"
	default:
		return PageAssetMetadata{}, fmt.Errorf("%w: unsupported image media type %q", ErrPageAssetInvalid, asset.MediaType)
	}
	if asset.SizeBytes <= 0 || asset.PixelWidth <= 0 || asset.PixelHeight <= 0 {
		return PageAssetMetadata{}, fmt.Errorf("%w: positive size and decoded pixel dimensions are required", ErrPageAssetInvalid)
	}
	if asset.OrientationPolicy != PageAssetOrientationUnverified &&
		asset.OrientationPolicy != PageAssetOrientationVerified {
		return PageAssetMetadata{}, fmt.Errorf("%w: unsupported orientation policy %q", ErrPageAssetInvalid, asset.OrientationPolicy)
	}
	if asset.OrientationPolicyVersion == "" {
		if asset.OrientationPolicy == PageAssetOrientationVerified {
			return PageAssetMetadata{}, fmt.Errorf("%w: verified orientation policy requires an explicit version", ErrPageAssetInvalid)
		}
		asset.OrientationPolicyVersion = "unverified-v1"
	}
	if asset.OrientationPolicy == PageAssetOrientationVerified &&
		strings.HasPrefix(strings.ToLower(asset.OrientationPolicyVersion), "unverified-") {
		return PageAssetMetadata{}, fmt.Errorf("%w: verified orientation policy cannot use an unverified version", ErrPageAssetInvalid)
	}
	assetOwner, file, err := assetstore.Parse(asset.PageAssetID)
	if err != nil || assetOwner != asset.AgentName ||
		file != asset.ContentDigest+"."+extension {
		return PageAssetMetadata{}, fmt.Errorf("%w: page_asset_id does not bind agent, digest and media type", ErrPageAssetInvalid)
	}
	asset.TransformChainJSON, err = canonicalPageAssetTransformChain(asset.TransformChainJSON)
	if err != nil {
		return PageAssetMetadata{}, err
	}
	// Prepare always creates staging. Mutable lifecycle fields supplied by a
	// caller never authorize a state transition or timestamp rewrite.
	asset.StorageState = PageAssetStorageStaging
	asset.ReadyAt = 0
	asset.LastError = ""
	asset.CreatedAt = 0
	asset.UpdatedAt = 0
	return asset, nil
}

func pageAssetIdentityEqual(a, b PageAssetMetadata) bool {
	return a.OwnerScope == b.OwnerScope &&
		a.AgentName == b.AgentName &&
		a.PageAssetID == b.PageAssetID &&
		a.ContentDigest == b.ContentDigest &&
		a.MediaType == b.MediaType &&
		a.SizeBytes == b.SizeBytes &&
		a.PixelWidth == b.PixelWidth &&
		a.PixelHeight == b.PixelHeight &&
		a.OrientationPolicy == b.OrientationPolicy &&
		a.OrientationPolicyVersion == b.OrientationPolicyVersion &&
		a.TransformChainJSON == b.TransformChainJSON
}

func (s *Store) getPageAsset(
	ctx context.Context,
	ownerScope, agentName, pageAssetID string,
) (PageAssetMetadata, error) {
	asset, err := scanPageAsset(s.db.QueryRowContext(ctx, `SELECT `+pageAssetColumns+`
        FROM k12_page_assets
        WHERE owner_scope=? AND agent_name=? AND page_asset_id=?`,
		ownerScope,
		agentName,
		pageAssetID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PageAssetMetadata{}, ErrPageAssetNotFound
	}
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: get PageAsset: %w", err)
	}
	return asset, nil
}

func (s *Store) getPageAssetByAgentIdentity(
	ctx context.Context,
	agentName, pageAssetID string,
) (PageAssetMetadata, error) {
	asset, err := scanPageAsset(s.db.QueryRowContext(ctx, `SELECT `+pageAssetColumns+`
        FROM k12_page_assets
        WHERE agent_name=? AND page_asset_id=?`,
		agentName,
		pageAssetID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PageAssetMetadata{}, ErrPageAssetNotFound
	}
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: get PageAsset identity: %w", err)
	}
	return asset, nil
}

// PreparePageAsset freezes one immutable metadata identity in staging state.
// Exact concurrent/repeated identities return the already durable row with
// created=false; any metadata or owner drift under the same PageAsset identity
// fails closed.
func (s *Store) PreparePageAsset(
	ctx context.Context,
	input PageAssetMetadata,
) (PageAssetMetadata, bool, error) {
	ownerScope, agentName, pageAssetID, err := normalizePageAssetScope(
		input.OwnerScope,
		input.AgentName,
		input.PageAssetID,
	)
	if err != nil {
		return PageAssetMetadata{}, false, err
	}
	stored, err := s.getPageAsset(ctx, ownerScope, agentName, pageAssetID)
	if err == nil {
		asset, normalizeErr := normalizePageAssetIdentity(input)
		if normalizeErr == nil && pageAssetIdentityEqual(stored, asset) {
			return stored, false, nil
		}
		return PageAssetMetadata{}, false, fmt.Errorf(
			"%w: immutable PageAsset metadata differs from the durable identity",
			ErrPageAssetConflict,
		)
	}
	if !errors.Is(err, ErrPageAssetNotFound) {
		return PageAssetMetadata{}, false, err
	}
	if stored, err := s.getPageAssetByAgentIdentity(ctx, agentName, pageAssetID); err == nil {
		asset, normalizeErr := normalizePageAssetIdentity(input)
		if normalizeErr == nil && pageAssetIdentityEqual(stored, asset) {
			return stored, false, nil
		}
		return PageAssetMetadata{}, false, fmt.Errorf(
			"%w: agent/page asset identity is already bound",
			ErrPageAssetConflict,
		)
	} else if !errors.Is(err, ErrPageAssetNotFound) {
		return PageAssetMetadata{}, false, err
	}

	asset, err := normalizePageAssetIdentity(input)
	if err != nil {
		return PageAssetMetadata{}, false, err
	}
	if err := ensureAgentRegistered(ctx, s.db, asset.AgentName); err != nil {
		return PageAssetMetadata{}, false, err
	}
	now := nowUnix()
	if now <= 0 {
		now = 1
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO k12_page_assets (`+pageAssetColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,'staging',0,'',?,?)
		ON CONFLICT(owner_scope,page_asset_id) DO NOTHING`,
		asset.OwnerScope,
		asset.AgentName,
		asset.PageAssetID,
		asset.ContentDigest,
		asset.MediaType,
		asset.SizeBytes,
		asset.PixelWidth,
		asset.PixelHeight,
		asset.OrientationPolicy,
		asset.OrientationPolicyVersion,
		asset.TransformChainJSON,
		now,
		now,
	)
	if err != nil {
		// UNIQUE(agent_name,page_asset_id) is the cross-owner rebinding gate.
		// Resolve it from durable state before classifying. A concurrent exact
		// insert is an idempotent success; an owner or metadata drift conflicts.
		// Unrelated database failures must retain their original error.
		if stored, getErr := s.getPageAssetByAgentIdentity(
			ctx,
			asset.AgentName,
			asset.PageAssetID,
		); getErr == nil {
			if pageAssetIdentityEqual(stored, asset) {
				return stored, false, nil
			}
			return PageAssetMetadata{}, false, fmt.Errorf(
				"%w: agent/page asset identity is already bound",
				ErrPageAssetConflict,
			)
		}
		return PageAssetMetadata{}, false, fmt.Errorf("k12storage: prepare PageAsset: %w", err)
	}
	rows, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return PageAssetMetadata{}, false, fmt.Errorf("k12storage: prepare PageAsset rows: %w", rowsErr)
	}
	stored, err = s.getPageAsset(ctx, asset.OwnerScope, asset.AgentName, asset.PageAssetID)
	if err != nil {
		return PageAssetMetadata{}, false, err
	}
	if !pageAssetIdentityEqual(stored, asset) {
		return PageAssetMetadata{}, false, fmt.Errorf(
			"%w: owner=%s agent=%s asset=%s",
			ErrPageAssetConflict,
			asset.OwnerScope,
			asset.AgentName,
			asset.PageAssetID,
		)
	}
	return stored, rows == 1, nil
}

func normalizePageAssetScope(
	ownerScope, agentName, pageAssetID string,
) (string, string, string, error) {
	ownerScope = strings.TrimSpace(ownerScope)
	agentName = strings.TrimSpace(agentName)
	pageAssetID = strings.TrimSpace(pageAssetID)
	if ownerScope == "" || agentName == "" || pageAssetID == "" {
		return "", "", "", fmt.Errorf("%w: owner_scope, agent and page_asset_id are required", ErrPageAssetInvalid)
	}
	return ownerScope, agentName, pageAssetID, nil
}

// GetReadyPageAsset is the only consumer read. Non-ready and cross-owner rows
// are deliberately indistinguishable from missing identities.
func (s *Store) GetReadyPageAsset(
	ctx context.Context,
	ownerScope, agentName, pageAssetID string,
) (PageAssetMetadata, error) {
	ownerScope = strings.TrimSpace(ownerScope)
	agentName = strings.TrimSpace(agentName)
	pageAssetID = strings.TrimSpace(pageAssetID)
	if ownerScope == "" || agentName == "" || pageAssetID == "" {
		return PageAssetMetadata{}, ErrPageAssetNotFound
	}
	asset, err := scanPageAsset(s.db.QueryRowContext(ctx, `SELECT `+pageAssetColumns+`
        FROM k12_page_assets
        WHERE owner_scope=? AND agent_name=? AND page_asset_id=? AND storage_state='ready'`,
		ownerScope,
		agentName,
		pageAssetID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PageAssetMetadata{}, ErrPageAssetNotFound
	}
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: get ready PageAsset: %w", err)
	}
	return asset, nil
}

// MarkPageAssetReady performs staging -> ready CAS. Replaying an already-ready
// identity returns its frozen row without changing ready_at.
func (s *Store) MarkPageAssetReady(
	ctx context.Context,
	ownerScope, agentName, pageAssetID string,
) (PageAssetMetadata, error) {
	ownerScope, agentName, pageAssetID, err := normalizePageAssetScope(
		ownerScope,
		agentName,
		pageAssetID,
	)
	if err != nil {
		return PageAssetMetadata{}, err
	}
	now := nowUnix()
	if now <= 0 {
		now = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE k12_page_assets
		SET storage_state='ready',
		    ready_at=MAX(updated_at+1,?),last_error='',
		    updated_at=MAX(updated_at+1,?)
        WHERE owner_scope=? AND agent_name=? AND page_asset_id=?
          AND storage_state='staging'`,
		now,
		now,
		ownerScope,
		agentName,
		pageAssetID,
	)
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: mark PageAsset ready: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: mark PageAsset ready rows: %w", err)
	}
	stored, err := s.getPageAsset(ctx, ownerScope, agentName, pageAssetID)
	if err != nil {
		return PageAssetMetadata{}, err
	}
	if rows == 1 || stored.StorageState == PageAssetStorageReady {
		return stored, nil
	}
	return PageAssetMetadata{}, fmt.Errorf(
		"%w: cannot mark %s PageAsset ready",
		ErrPageAssetConflict,
		stored.StorageState,
	)
}

func (s *Store) markPageAssetFailureState(
	ctx context.Context,
	ownerScope, agentName, pageAssetID, detail string,
	target PageAssetStorageState,
) (PageAssetMetadata, error) {
	ownerScope, agentName, pageAssetID, err := normalizePageAssetScope(
		ownerScope,
		agentName,
		pageAssetID,
	)
	if err != nil {
		return PageAssetMetadata{}, err
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return PageAssetMetadata{}, fmt.Errorf("%w: failure detail is required", ErrPageAssetInvalid)
	}
	allowedSource := "storage_state='staging'"
	if target == PageAssetStorageCorrupt {
		allowedSource = "storage_state IN ('staging','ready')"
	}
	now := nowUnix()
	if now <= 0 {
		now = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE k12_page_assets
		SET storage_state=?,last_error=?,updated_at=MAX(updated_at+1,?)
        WHERE owner_scope=? AND agent_name=? AND page_asset_id=? AND `+allowedSource,
		target,
		detail,
		now,
		ownerScope,
		agentName,
		pageAssetID,
	)
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: mark PageAsset %s: %w", target, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: mark PageAsset %s rows: %w", target, err)
	}
	stored, err := s.getPageAsset(ctx, ownerScope, agentName, pageAssetID)
	if err != nil {
		return PageAssetMetadata{}, err
	}
	if rows == 1 || (stored.StorageState == target && stored.LastError == detail) {
		return stored, nil
	}
	return PageAssetMetadata{}, fmt.Errorf(
		"%w: cannot mark %s PageAsset %s or rewrite failure detail",
		ErrPageAssetConflict,
		stored.StorageState,
		target,
	)
}

func (s *Store) MarkPageAssetFailed(
	ctx context.Context,
	ownerScope, agentName, pageAssetID, detail string,
) (PageAssetMetadata, error) {
	return s.markPageAssetFailureState(
		ctx,
		ownerScope,
		agentName,
		pageAssetID,
		detail,
		PageAssetStorageFailed,
	)
}

func (s *Store) MarkPageAssetCorrupt(
	ctx context.Context,
	ownerScope, agentName, pageAssetID, detail string,
) (PageAssetMetadata, error) {
	return s.markPageAssetFailureState(
		ctx,
		ownerScope,
		agentName,
		pageAssetID,
		detail,
		PageAssetStorageCorrupt,
	)
}

// RetryPageAssetStaging performs the only automatic recovery transition:
// failed -> staging. Staging and ready are exact idempotent replays; corrupt
// bytes require explicit replacement/reconciliation and are never retried.
func (s *Store) RetryPageAssetStaging(
	ctx context.Context,
	ownerScope, agentName, pageAssetID string,
) (PageAssetMetadata, error) {
	ownerScope, agentName, pageAssetID, err := normalizePageAssetScope(
		ownerScope,
		agentName,
		pageAssetID,
	)
	if err != nil {
		return PageAssetMetadata{}, err
	}
	now := nowUnix()
	if now <= 0 {
		now = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE k12_page_assets
		SET storage_state='staging',ready_at=0,last_error='',
		    updated_at=MAX(updated_at+1,?)
		WHERE owner_scope=? AND agent_name=? AND page_asset_id=?
		  AND storage_state='failed'`,
		now,
		ownerScope,
		agentName,
		pageAssetID,
	)
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: retry PageAsset staging: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: retry PageAsset staging rows: %w", err)
	}
	stored, err := s.getPageAsset(ctx, ownerScope, agentName, pageAssetID)
	if err != nil {
		return PageAssetMetadata{}, err
	}
	if rows == 1 || stored.StorageState == PageAssetStorageStaging ||
		stored.StorageState == PageAssetStorageReady {
		return stored, nil
	}
	return PageAssetMetadata{}, fmt.Errorf(
		"%w: cannot automatically retry %s PageAsset",
		ErrPageAssetConflict,
		stored.StorageState,
	)
}

// RepairCorruptPageAssetStaging 只供已逐字段重验身份一致的上传执行专用 CAS。
func (s *Store) RepairCorruptPageAssetStaging(
	ctx context.Context,
	ownerScope, agentName, pageAssetID string,
) (PageAssetMetadata, error) {
	ownerScope, agentName, pageAssetID, err := normalizePageAssetScope(
		ownerScope,
		agentName,
		pageAssetID,
	)
	if err != nil {
		return PageAssetMetadata{}, err
	}
	now := nowUnix()
	if now <= 0 {
		now = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE k12_page_assets
		SET storage_state='staging',ready_at=0,last_error='',
		    updated_at=MAX(updated_at+1,?)
		WHERE owner_scope=? AND agent_name=? AND page_asset_id=?
		  AND storage_state='corrupt'`,
		now,
		ownerScope,
		agentName,
		pageAssetID,
	)
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: repair corrupt PageAsset staging: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return PageAssetMetadata{}, fmt.Errorf("k12storage: repair corrupt PageAsset staging rows: %w", err)
	}
	stored, err := s.getPageAsset(ctx, ownerScope, agentName, pageAssetID)
	if err != nil {
		return PageAssetMetadata{}, err
	}
	if rows == 1 {
		return stored, nil
	}
	return PageAssetMetadata{}, fmt.Errorf(
		"%w: cannot repair %s PageAsset",
		ErrPageAssetConflict,
		stored.StorageState,
	)
}
