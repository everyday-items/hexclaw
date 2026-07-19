// Package renderledger generates the current producer × surface × payload
// evidence matrix from production registries. It deliberately owns no frozen
// counts: adding or removing a production registry member changes the exact
// set and causes ValidateCurrent to fail until every generated cell exists.
package renderledger

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

type PayloadClass struct {
	Name     string
	Markdown string
}

type SurfaceProfile struct {
	Surface         messagecontent.Surface
	RendererVersion string
	Capabilities    messagecontent.CapabilitySnapshot
}

type Row struct {
	Producer        string `json:"producer"`
	Surface         string `json:"surface"`
	PayloadClass    string `json:"payload_class"`
	Allowed         bool   `json:"allowed"`
	SourceDigest    string `json:"source_digest"`
	RenderID        string `json:"render_id"`
	RendererVersion string `json:"renderer_version"`
	Strategy        string `json:"strategy"`
	FallbackReason  string `json:"fallback_reason,omitempty"`
}

func ProductionProducers() []messagecontent.ProducerKind {
	return messagecontent.ProducerKinds()
}

func ProductionPayloadClasses() []PayloadClass {
	return []PayloadClass{
		{Name: "markdown", Markdown: "## 辅导结论\n\n- 保留 **证据**\n- 可复制正文"},
		{Name: "inline_math", Markdown: `答案是 $\frac{3}{4} \times 8 = 6$。`},
		{Name: "block_math", Markdown: "推导：\n\\[x^2 + 2x + 1 = (x+1)^2\\]"},
		{Name: "mixed_long", Markdown: "## 混排\n\n> 依据\n\n`x` 与 $\\alpha + \\beta$\n\n```text\ncopy-safe\n```\n\n\\[\\sum x_i \\geq 1\\]"},
	}
}

func ProductionSurfaces() []SurfaceProfile {
	profiles := make([]SurfaceProfile, 0, len(messagecontent.SurfaceKinds()))
	for _, surface := range messagecontent.SurfaceKinds() {
		profile := SurfaceProfile{
			Surface:         surface,
			RendererVersion: string(surface) + "-markdown-v1",
			Capabilities: messagecontent.CapabilitySnapshot{
				Markdown: true,
				TeXMath:  true,
				MathML:   true,
			},
		}
		if surface == messagecontent.SurfaceChannel {
			profile.RendererVersion = "channel-markdown-readable-math-v1"
			profile.Capabilities.TeXMath = false
			profile.Capabilities.MathML = false
			profile.Capabilities.UnicodeMath = true
		}
		profiles = append(profiles, profile)
	}
	return profiles
}

func Build() ([]Row, error) {
	producers := ProductionProducers()
	surfaces := ProductionSurfaces()
	payloads := ProductionPayloadClasses()
	rows := make([]Row, 0, len(producers)*len(surfaces)*len(payloads))
	for _, producer := range producers {
		for _, surface := range surfaces {
			for _, payload := range payloads {
				content, err := messagecontent.New(producer, "zh-CN", payload.Markdown, nil)
				if err != nil {
					return nil, fmt.Errorf("%s/%s/%s content: %w", producer, surface.Surface, payload.Name, err)
				}
				visible := payload.Markdown
				fallback := ""
				strategy := "native_markdown_tex"
				if !surface.Capabilities.TeXMath {
					visible = adapter.NormalizeMathText(visible)
					if visible != payload.Markdown {
						fallback = messagecontent.FallbackMathToReadableText
						strategy = "markdown_unicode_math_fallback"
					} else {
						strategy = "native_markdown"
					}
				}
				manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
					Surface:         surface.Surface,
					RendererVersion: surface.RendererVersion,
					Capabilities:    surface.Capabilities,
					Parts:           []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: visible}},
					FallbackReason:  fallback,
				})
				if err != nil {
					return nil, fmt.Errorf("%s/%s/%s render: %w", producer, surface.Surface, payload.Name, err)
				}
				rows = append(rows, Row{
					Producer:        string(producer),
					Surface:         string(surface.Surface),
					PayloadClass:    payload.Name,
					Allowed:         true,
					SourceDigest:    content.SourceDigest,
					RenderID:        manifest.RenderID,
					RendererVersion: manifest.RendererVersion,
					Strategy:        strategy,
					FallbackReason:  manifest.FallbackReason,
				})
			}
		}
	}
	return rows, nil
}

func ValidateCurrent(rows []Row) error {
	expected := make(map[string]struct{})
	for _, producer := range ProductionProducers() {
		for _, surface := range ProductionSurfaces() {
			for _, payload := range ProductionPayloadClasses() {
				expected[cellKey(string(producer), string(surface.Surface), payload.Name)] = struct{}{}
			}
		}
	}
	actual := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		key := cellKey(row.Producer, row.Surface, row.PayloadClass)
		if _, duplicate := actual[key]; duplicate {
			return fmt.Errorf("render ledger duplicate cell %s", key)
		}
		actual[key] = struct{}{}
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("render ledger unexpected cell %s", key)
		}
		if !row.Allowed || strings.TrimSpace(row.SourceDigest) == "" || strings.TrimSpace(row.RenderID) == "" || strings.TrimSpace(row.RendererVersion) == "" {
			return fmt.Errorf("render ledger incomplete allowed cell %s", key)
		}
	}
	missing := make([]string, 0)
	for key := range expected {
		if _, ok := actual[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("render ledger missing cells: %s", strings.Join(missing, ", "))
	}
	return nil
}

func cellKey(producer, surface, payload string) string {
	return producer + "|" + surface + "|" + payload
}
