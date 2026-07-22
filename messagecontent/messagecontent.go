// Package messagecontent defines the versioned canonical content and render
// evidence shared by chat, K12, skills, tools, RAG and automation producers.
//
// It deliberately stores Markdown/TeX source, never rendered HTML. Renderers
// must emit a RenderManifest so a visible result can be traced back to the
// exact canonical source and capability snapshot used for the projection.
package messagecontent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const CurrentContentVersion = "1.0"

type ProducerKind string

const (
	ProducerChat      ProducerKind = "chat"
	ProducerQuickChat ProducerKind = "quick_chat"
	ProducerK12       ProducerKind = "k12"
	ProducerSkill     ProducerKind = "skill"
	ProducerTool      ProducerKind = "tool"
	ProducerRAG       ProducerKind = "rag"
	ProducerReport    ProducerKind = "report"
	ProducerCron      ProducerKind = "cron"
	ProducerWebhook   ProducerKind = "webhook"
	ProducerWorkflow  ProducerKind = "workflow"
)

var surfaceKinds = [...]Surface{
	SurfaceDesktop,
	SurfaceQuickChat,
	SurfaceHistory,
	SurfaceK12,
	SurfaceChannel,
	SurfaceExport,
}

func SurfaceKinds() []Surface {
	result := make([]Surface, len(surfaceKinds))
	copy(result, surfaceKinds[:])
	return result
}

var producerKinds = [...]ProducerKind{
	ProducerChat,
	ProducerQuickChat,
	ProducerK12,
	ProducerSkill,
	ProducerTool,
	ProducerRAG,
	ProducerReport,
	ProducerCron,
	ProducerWebhook,
	ProducerWorkflow,
}

func ProducerKinds() []ProducerKind {
	result := make([]ProducerKind, len(producerKinds))
	copy(result, producerKinds[:])
	return result
}

func (p ProducerKind) valid() bool {
	for _, candidate := range producerKinds {
		if p == candidate {
			return true
		}
	}
	return false
}

func ParseProducerKind(raw string) (ProducerKind, bool) {
	producer := ProducerKind(raw)
	return producer, producer.valid()
}

type AttachmentRef struct {
	AssetID string `json:"asset_id"`
	Name    string `json:"name,omitempty"`
	MIME    string `json:"mime"`
	Digest  string `json:"digest"`
	AltText string `json:"alt_text,omitempty"`
}

type MessageContent struct {
	ContentID      string          `json:"content_id"`
	ContentVersion string          `json:"content_version"`
	ProducerKind   ProducerKind    `json:"producer_kind"`
	Markdown       string          `json:"markdown"`
	SourceDigest   string          `json:"source_digest"`
	Locale         string          `json:"locale"`
	Attachments    []AttachmentRef `json:"attachments,omitempty"`
}

type digestInput struct {
	ContentVersion string          `json:"content_version"`
	ProducerKind   ProducerKind    `json:"producer_kind"`
	Markdown       string          `json:"markdown"`
	Locale         string          `json:"locale"`
	Attachments    []AttachmentRef `json:"attachments,omitempty"`
}

func New(producer ProducerKind, locale, markdown string, attachments []AttachmentRef) (MessageContent, error) {
	content := MessageContent{
		ContentVersion: CurrentContentVersion,
		ProducerKind:   producer,
		Markdown:       markdown,
		Locale:         locale,
		Attachments:    append([]AttachmentRef(nil), attachments...),
	}
	digest, err := content.computeSourceDigest()
	if err != nil {
		return MessageContent{}, err
	}
	content.SourceDigest = digest
	content.ContentID = "content:" + strings.TrimPrefix(digest, "sha256:")
	if err := content.Validate(); err != nil {
		return MessageContent{}, err
	}
	return content, nil
}

func (c MessageContent) Validate() error {
	if c.ContentVersion != CurrentContentVersion {
		return fmt.Errorf("messagecontent: unsupported content_version %q", c.ContentVersion)
	}
	if !c.ProducerKind.valid() {
		return fmt.Errorf("messagecontent: invalid producer_kind %q", c.ProducerKind)
	}
	if strings.TrimSpace(c.Locale) == "" {
		return errors.New("messagecontent: locale is required")
	}
	if strings.TrimSpace(c.Markdown) == "" && len(c.Attachments) == 0 {
		return errors.New("messagecontent: markdown or attachments are required")
	}
	for i, attachment := range c.Attachments {
		if strings.TrimSpace(attachment.AssetID) == "" || strings.TrimSpace(attachment.MIME) == "" || strings.TrimSpace(attachment.Digest) == "" {
			return fmt.Errorf("messagecontent: attachment %d requires asset_id, mime and digest", i)
		}
	}
	want, err := c.computeSourceDigest()
	if err != nil {
		return err
	}
	if c.SourceDigest != want {
		return fmt.Errorf("messagecontent: source_digest mismatch: got %q want %q", c.SourceDigest, want)
	}
	if c.ContentID != "content:"+strings.TrimPrefix(want, "sha256:") {
		return errors.New("messagecontent: content_id does not match source_digest")
	}
	return nil
}

func (c MessageContent) computeSourceDigest() (string, error) {
	payload, err := json.Marshal(digestInput{
		ContentVersion: c.ContentVersion,
		ProducerKind:   c.ProducerKind,
		Markdown:       c.Markdown,
		Locale:         c.Locale,
		Attachments:    c.Attachments,
	})
	if err != nil {
		return "", fmt.Errorf("messagecontent: encode digest input: %w", err)
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type Surface string

const (
	SurfaceDesktop   Surface = "desktop"
	SurfaceQuickChat Surface = "quick_chat"
	SurfaceHistory   Surface = "history"
	SurfaceK12       Surface = "k12"
	SurfaceChannel   Surface = "channel"
	SurfaceExport    Surface = "export"
)

type CapabilitySnapshot struct {
	Markdown    bool `json:"markdown"`
	TeXMath     bool `json:"tex_math"`
	MathML      bool `json:"mathml,omitempty"`
	UnicodeMath bool `json:"unicode_math,omitempty"`
	Attachments bool `json:"attachments,omitempty"`
	MaxRunes    int  `json:"max_runes,omitempty"`
}

type PartKind string

const (
	PartMarkdown PartKind = "markdown"
	PartText     PartKind = "text"
	PartArtifact PartKind = "artifact"
)

type RenderPart struct {
	Kind           PartKind `json:"kind"`
	Text           string   `json:"text,omitempty"`
	ArtifactRef    string   `json:"artifact_ref,omitempty"`
	ArtifactDigest string   `json:"artifact_digest,omitempty"`
	AltText        string   `json:"alt_text,omitempty"`
}

const (
	FallbackMathToReadableText = "math_to_readable_text"
	FallbackParseFailure       = "parse_failure_raw_text"
	FallbackCapabilityMissing  = "capability_missing"
)

type RenderManifest struct {
	RenderID           string             `json:"render_id"`
	ContentID          string             `json:"content_id"`
	Surface            Surface            `json:"surface"`
	CapabilitySnapshot CapabilitySnapshot `json:"capability_snapshot"`
	RendererVersion    string             `json:"renderer_version"`
	SourceDigest       string             `json:"source_digest"`
	Parts              []RenderPart       `json:"parts"`
	FallbackReason     string             `json:"fallback_reason,omitempty"`
	ReceiptRef         string             `json:"receipt_ref,omitempty"`
}

type RenderRequest struct {
	Surface         Surface
	Capabilities    CapabilitySnapshot
	RendererVersion string
	Parts           []RenderPart
	FallbackReason  string
	ReceiptRef      string
}

var rawTeXPattern = regexp.MustCompile(`(?s)(\\(?:frac|sqrt|sum|int|begin|left|right|times|cdot|leq|geq|alpha|beta)\b|\\[,;:!]|\$[^$\n]+\$|\\\(|\\\[)`)

func BuildManifest(content MessageContent, request RenderRequest) (RenderManifest, error) {
	if err := content.Validate(); err != nil {
		return RenderManifest{}, err
	}
	manifest := RenderManifest{
		ContentID:          content.ContentID,
		Surface:            request.Surface,
		CapabilitySnapshot: request.Capabilities,
		RendererVersion:    request.RendererVersion,
		SourceDigest:       content.SourceDigest,
		Parts:              append([]RenderPart(nil), request.Parts...),
		FallbackReason:     request.FallbackReason,
		ReceiptRef:         request.ReceiptRef,
	}
	if err := manifest.validateProjection(content); err != nil {
		return RenderManifest{}, err
	}
	payload, err := json.Marshal(struct {
		ContentID       string             `json:"content_id"`
		Surface         Surface            `json:"surface"`
		Capabilities    CapabilitySnapshot `json:"capabilities"`
		RendererVersion string             `json:"renderer_version"`
		SourceDigest    string             `json:"source_digest"`
		Parts           []RenderPart       `json:"parts"`
		FallbackReason  string             `json:"fallback_reason,omitempty"`
		ReceiptRef      string             `json:"receipt_ref,omitempty"`
	}{manifest.ContentID, manifest.Surface, manifest.CapabilitySnapshot, manifest.RendererVersion, manifest.SourceDigest, manifest.Parts, manifest.FallbackReason, manifest.ReceiptRef})
	if err != nil {
		return RenderManifest{}, fmt.Errorf("messagecontent: encode render manifest: %w", err)
	}
	sum := sha256.Sum256(payload)
	manifest.RenderID = "render:" + hex.EncodeToString(sum[:])
	return manifest, nil
}

func (m RenderManifest) ValidateFor(content MessageContent) error {
	if err := content.Validate(); err != nil {
		return err
	}
	if m.ContentID != content.ContentID || m.SourceDigest != content.SourceDigest {
		return errors.New("messagecontent: render manifest source does not match content")
	}
	if strings.TrimSpace(m.RenderID) == "" {
		return errors.New("messagecontent: render_id is required")
	}
	return m.validateProjection(content)
}

func (m RenderManifest) validateProjection(content MessageContent) error {
	if strings.TrimSpace(string(m.Surface)) == "" || strings.TrimSpace(m.RendererVersion) == "" {
		return errors.New("messagecontent: surface and renderer_version are required")
	}
	if len(m.Parts) == 0 {
		return errors.New("messagecontent: render projection must contain at least one part")
	}
	var visible strings.Builder
	for i, part := range m.Parts {
		switch part.Kind {
		case PartMarkdown:
			if !m.CapabilitySnapshot.Markdown {
				return errors.New("messagecontent: markdown part emitted without markdown capability")
			}
			if strings.TrimSpace(part.Text) == "" {
				return fmt.Errorf("messagecontent: render part %d is empty", i)
			}
			visible.WriteString(part.Text)
		case PartText:
			if strings.TrimSpace(part.Text) == "" {
				return fmt.Errorf("messagecontent: render part %d is empty", i)
			}
			visible.WriteString(part.Text)
		case PartArtifact:
			if !m.CapabilitySnapshot.Attachments {
				return errors.New("messagecontent: artifact part emitted without attachment capability")
			}
			if part.ArtifactRef == "" || part.ArtifactDigest == "" || part.AltText == "" {
				return fmt.Errorf("messagecontent: artifact part %d requires ref, digest and alt text", i)
			}
			visible.WriteString(part.AltText)
		default:
			return fmt.Errorf("messagecontent: invalid render part kind %q", part.Kind)
		}
	}
	if rawTeXPattern.MatchString(content.Markdown) && !m.CapabilitySnapshot.TeXMath {
		if strings.TrimSpace(m.FallbackReason) == "" {
			return errors.New("messagecontent: math fallback reason is required")
		}
		if rawTeXPattern.MatchString(visible.String()) {
			return errors.New("messagecontent: raw LaTeX cannot be reported as a successful fallback")
		}
	}
	return nil
}
