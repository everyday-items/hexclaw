package mcp

// ToolAnnotation represents MCP tool annotations (2025 standard).
//
// These hint at the tool's behavior, used for:
//   - Auto-classification into safe/sensitive/dangerous
//   - Frontend display (risk badges)
//   - Caching decisions (readOnly → cacheable)
type ToolAnnotation struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// RiskLevel derives a risk classification from annotations.
//
// Rules:
//   - destructiveHint=true → "dangerous"
//   - openWorldHint=true → "sensitive"
//   - readOnlyHint=true → "safe"
//   - no annotations → "sensitive" (conservative default)
func (a *ToolAnnotation) RiskLevel() string {
	if a == nil {
		return "sensitive"
	}
	if a.DestructiveHint != nil && *a.DestructiveHint {
		return "dangerous"
	}
	if a.OpenWorldHint != nil && *a.OpenWorldHint {
		return "sensitive"
	}
	if a.ReadOnlyHint != nil && *a.ReadOnlyHint {
		return "safe"
	}
	return "sensitive"
}

// IsCacheable returns true if the tool is safe to cache (readOnly + idempotent).
func (a *ToolAnnotation) IsCacheable() bool {
	if a == nil {
		return false
	}
	readOnly := a.ReadOnlyHint != nil && *a.ReadOnlyHint
	idempotent := a.IdempotentHint != nil && *a.IdempotentHint
	return readOnly || idempotent
}
