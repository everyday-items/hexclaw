package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

const (
	maxStagedAttachmentBytes      = 20 << 20
	maxStagedAttachmentEntries    = 256
	maxStagedAttachmentTotalBytes = 512 << 20
	stagedAttachmentTTL           = 15 * time.Minute
)

var (
	errAttachmentNotFound    = errors.New("attachment not found")
	errAttachmentConflict    = errors.New("attachment idempotency conflict")
	errAttachmentTooLarge    = errors.New("attachment exceeds size limit")
	errAttachmentCapacity    = errors.New("attachment staging capacity exceeded")
	errAttachmentInvalid     = errors.New("invalid attachment")
	attachmentIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// AttachmentReceipt is the non-secret, renderer-safe identity of a staged
// binary. The Sidecar owns all metadata; chat requests send attachment_id only.
type AttachmentReceipt struct {
	AttachmentID string    `json:"attachment_id"`
	Digest       string    `json:"digest"`
	Size         int64     `json:"size"`
	MediaType    string    `json:"media_type"`
	DisplayName  string    `json:"display_name"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type stagedAttachment struct {
	receipt        AttachmentReceipt
	ownerID        string
	idempotencyKey string
	path           string
}

type attachmentStagingStore struct {
	mu          sync.Mutex
	dir         string
	entries     map[string]stagedAttachment
	idempotency map[string]string
	totalBytes  int64
	now         func() time.Time
	closed      bool
	active      sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
}

func newAttachmentStagingStore() *attachmentStagingStore {
	return &attachmentStagingStore{
		entries: make(map[string]stagedAttachment), idempotency: make(map[string]string), now: time.Now,
	}
}

// beginStage serializes Add with Close's transition to closed. This is the
// required WaitGroup discipline: after Close observes closed, no Add can race
// with its Wait and the staging directory remains alive until every in-flight
// stream has drained.
func (s *attachmentStagingStore) beginStage() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("attachment staging is closed")
	}
	if s.dir == "" {
		dir, err := os.MkdirTemp("", "hexclaw-attachment-staging-*")
		if err != nil {
			return "", err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		s.dir = dir
	}
	s.active.Add(1)
	return s.dir, nil
}

func newAttachmentID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "att_v1_" + hex.EncodeToString(random), nil
}

func normalizeAttachmentDisplayName(name string) (string, error) {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "" || name == "." || len(name) > 255 || strings.ContainsAny(name, "\x00\r\n") {
		return "", errAttachmentInvalid
	}
	return name, nil
}

func normalizeAttachmentMediaType(claimed string, prefix []byte) (string, error) {
	claimed, _, _ = mime.ParseMediaType(strings.TrimSpace(claimed))
	claimed = strings.ToLower(claimed)
	detected := strings.ToLower(http.DetectContentType(prefix))
	if !strings.HasPrefix(detected, "image/") || (claimed != "" && claimed != detected) {
		return "", fmt.Errorf("%w: only accurately typed images are supported", errAttachmentInvalid)
	}
	return detected, nil
}

func (s *attachmentStagingStore) Stage(
	ctx context.Context,
	ownerID, idempotencyKey, displayName, claimedMediaType string,
	reader io.Reader,
) (AttachmentReceipt, bool, error) {
	ownerID = strings.TrimSpace(ownerID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if ownerID == "" || !attachmentIdempotencyKey.MatchString(idempotencyKey) || reader == nil {
		return AttachmentReceipt{}, false, errAttachmentInvalid
	}
	displayName, err := normalizeAttachmentDisplayName(displayName)
	if err != nil {
		return AttachmentReceipt{}, false, err
	}
	dir, err := s.beginStage()
	if err != nil {
		return AttachmentReceipt{}, false, err
	}
	defer s.active.Done()
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return AttachmentReceipt{}, false, err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	hash := sha256.New()
	limited := io.LimitReader(reader, maxStagedAttachmentBytes+1)
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), limited)
	if copyErr != nil {
		return AttachmentReceipt{}, false, copyErr
	}
	if err := ctx.Err(); err != nil {
		return AttachmentReceipt{}, false, err
	}
	if written == 0 {
		return AttachmentReceipt{}, false, errAttachmentInvalid
	}
	if written > maxStagedAttachmentBytes {
		return AttachmentReceipt{}, false, errAttachmentTooLarge
	}
	if err := tmp.Sync(); err != nil {
		return AttachmentReceipt{}, false, err
	}
	if err := tmp.Close(); err != nil {
		return AttachmentReceipt{}, false, err
	}
	prefix := make([]byte, min(int64(512), written))
	file, err := os.Open(tmpPath)
	if err != nil {
		return AttachmentReceipt{}, false, err
	}
	_, readErr := io.ReadFull(file, prefix)
	_ = file.Close()
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return AttachmentReceipt{}, false, readErr
	}
	mediaType, err := normalizeAttachmentMediaType(claimedMediaType, prefix)
	if err != nil {
		return AttachmentReceipt{}, false, err
	}
	now := s.now().UTC()
	receipt := AttachmentReceipt{
		Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), Size: written,
		MediaType: mediaType, DisplayName: displayName, ExpiresAt: now.Add(stagedAttachmentTTL),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return AttachmentReceipt{}, false, errors.New("attachment staging is closed")
	}
	s.pruneExpiredLocked(now)
	idempotencyScope := ownerID + "\x00" + idempotencyKey
	if existingID := s.idempotency[idempotencyScope]; existingID != "" {
		existing, ok := s.entries[existingID]
		if ok && existing.receipt.Digest == receipt.Digest && existing.receipt.Size == receipt.Size &&
			existing.receipt.MediaType == receipt.MediaType && existing.receipt.DisplayName == receipt.DisplayName {
			return existing.receipt, true, nil
		}
		return AttachmentReceipt{}, false, errAttachmentConflict
	}
	if len(s.entries) >= maxStagedAttachmentEntries || s.totalBytes+written > maxStagedAttachmentTotalBytes {
		return AttachmentReceipt{}, false, errAttachmentCapacity
	}
	id, err := newAttachmentID()
	if err != nil {
		return AttachmentReceipt{}, false, err
	}
	finalPath := filepath.Join(dir, id+".bin")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return AttachmentReceipt{}, false, err
	}
	committed = true
	receipt.AttachmentID = id
	s.entries[id] = stagedAttachment{
		receipt: receipt, ownerID: ownerID, idempotencyKey: idempotencyScope, path: finalPath,
	}
	s.idempotency[idempotencyScope] = id
	s.totalBytes += written
	return receipt, false, nil
}

func (s *attachmentStagingStore) Resolve(ownerID, attachmentID string) (adapter.Attachment, error) {
	now := s.now().UTC()
	s.mu.Lock()
	s.pruneExpiredLocked(now)
	entry, ok := s.entries[strings.TrimSpace(attachmentID)]
	if !ok || entry.ownerID != strings.TrimSpace(ownerID) {
		s.mu.Unlock()
		return adapter.Attachment{}, errAttachmentNotFound
	}
	s.mu.Unlock()
	raw, err := os.ReadFile(entry.path)
	if err != nil || int64(len(raw)) != entry.receipt.Size {
		return adapter.Attachment{}, errAttachmentNotFound
	}
	digest := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(digest[:]) != entry.receipt.Digest {
		return adapter.Attachment{}, errAttachmentNotFound
	}
	return adapter.Attachment{
		Type: "image", Name: entry.receipt.DisplayName, Mime: entry.receipt.MediaType,
		Data: base64.StdEncoding.EncodeToString(raw),
	}, nil
}

// Discard removes a newly staged receipt when the enclosing HTTP request is
// rejected after its file part was consumed (for example, an unexpected
// second multipart part). Ownership prevents one request from deleting
// another principal's staged object.
func (s *attachmentStagingStore) Discard(ownerID, attachmentID string) bool {
	ownerID = strings.TrimSpace(ownerID)
	attachmentID = strings.TrimSpace(attachmentID)
	s.mu.Lock()
	entry, ok := s.entries[attachmentID]
	if !ok || entry.ownerID != ownerID {
		s.mu.Unlock()
		return false
	}
	delete(s.entries, attachmentID)
	delete(s.idempotency, entry.idempotencyKey)
	s.totalBytes -= entry.receipt.Size
	s.mu.Unlock()
	_ = os.Remove(entry.path)
	return true
}

func (s *attachmentStagingStore) pruneExpiredLocked(now time.Time) {
	for id, entry := range s.entries {
		if now.Before(entry.receipt.ExpiresAt) {
			continue
		}
		delete(s.entries, id)
		delete(s.idempotency, entry.idempotencyKey)
		s.totalBytes -= entry.receipt.Size
		_ = os.Remove(entry.path)
	}
}

func (s *attachmentStagingStore) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		dir := s.dir
		s.mu.Unlock()

		// Server shutdown has already stopped accepting HTTP work. Wait for any
		// request that entered Stage before the closed transition so RemoveAll
		// cannot race its file creation, hashing, or final validation.
		s.active.Wait()

		s.mu.Lock()
		s.entries = make(map[string]stagedAttachment)
		s.idempotency = make(map[string]string)
		s.totalBytes = 0
		s.mu.Unlock()
		if dir != "" {
			s.closeErr = os.RemoveAll(dir)
		}
	})
	return s.closeErr
}

func (s *Server) handleStageAttachment(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !attachmentIdempotencyKey.MatchString(idempotencyKey) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid Idempotency-Key is required"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxStagedAttachmentBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart/form-data is required"})
		return
	}
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" || part.FileName() == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "exactly one file part is required"})
		return
	}
	receipt, replay, err := s.attachmentStaging.Stage(
		r.Context(), httpPrincipalFromRequest(r).userID, idempotencyKey,
		part.FileName(), part.Header.Get("Content-Type"), part,
	)
	_ = part.Close()
	if err == nil {
		if extra, nextErr := reader.NextPart(); nextErr != io.EOF || extra != nil {
			if extra != nil {
				_ = extra.Close()
			}
			if !replay {
				s.attachmentStaging.Discard(httpPrincipalFromRequest(r).userID, receipt.AttachmentID)
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "exactly one file part is required"})
			return
		}
		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		writeJSON(w, status, receipt)
		return
	}
	switch {
	case errors.Is(err, errAttachmentConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "idempotency key conflicts with another attachment"})
	case errors.Is(err, errAttachmentTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "attachment exceeds size limit"})
	case errors.Is(err, errAttachmentCapacity):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "attachment staging is full"})
	case errors.Is(err, errAttachmentInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "attachment staging failed"})
	}
}

// ResolveStagedAttachments resolves only opaque IDs owned by the authenticated
// principal. Legacy URL/data attachments remain temporarily decode-compatible
// while Desktop consumers migrate, but an ID object must contain no metadata.
func (s *Server) ResolveStagedAttachments(ctx context.Context, ownerID string, attachments []adapter.Attachment) ([]adapter.Attachment, error) {
	if len(attachments) > adapter.MaxAttachments {
		return nil, fmt.Errorf("too many attachments")
	}
	resolved := make([]adapter.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.TrimSpace(attachment.ID) == "" {
			resolved = append(resolved, attachment)
			continue
		}
		if attachment.Type != "" || attachment.Name != "" || attachment.Mime != "" || attachment.Data != "" || attachment.URL != "" {
			return nil, errAttachmentInvalid
		}
		value, err := s.attachmentStaging.Resolve(ownerID, attachment.ID)
		if err != nil {
			return nil, errAttachmentNotFound
		}
		resolved = append(resolved, value)
	}
	return resolved, nil
}
