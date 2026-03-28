//go:build windows

package sandbox

// Phase 8 D33: Sandbox policy modes

// SandboxMode defines the isolation level.
type SandboxMode string

const (
	// ModeReadOnly allows read-only access to workspace.
	ModeReadOnly SandboxMode = "readonly"
	// ModeWorkspaceWrite allows read/write only to workspace directory.
	ModeWorkspaceWrite SandboxMode = "workspace-write"
	// ModeFullAccess applies Token/IL/Job limits but no filesystem restrictions.
	ModeFullAccess SandboxMode = "full-access"
)

// NetworkMode defines network access level.
type NetworkMode string

const (
	NetworkOffline NetworkMode = "offline" // Low Box Token without INTERNET_CLIENT
	NetworkOnline  NetworkMode = "online"  // Low Box Token with INTERNET_CLIENT
)

// WindowsSandboxPolicy combines all sandbox settings.
type WindowsSandboxPolicy struct {
	Mode        SandboxMode `yaml:"mode"`         // filesystem isolation level
	Network     NetworkMode `yaml:"network"`       // network access level
	MemoryMB    int         `yaml:"memory_mb"`     // memory limit in MB
	MaxProcs    int         `yaml:"max_processes"` // max child processes
	UseDesktop  bool        `yaml:"use_desktop"`   // create alternate desktop
}

// DefaultWindowsPolicy returns the recommended secure defaults.
func DefaultWindowsPolicy() WindowsSandboxPolicy {
	return WindowsSandboxPolicy{
		Mode:       ModeWorkspaceWrite,
		Network:    NetworkOffline,
		MemoryMB:   512,
		MaxProcs:   10,
		UseDesktop: true,
	}
}

// applyPolicy applies the policy to the sandbox configuration.
func (p *WindowsSandboxPolicy) applyPolicy(cfg *Config) {
	switch p.Mode {
	case ModeReadOnly:
		// ACL: deny all writes, even in workspace
		// Token: already restricted
	case ModeWorkspaceWrite:
		// ACL: allow read/write only in workspace (default)
	case ModeFullAccess:
		// No ACL restrictions — only Token/IL/Job limits
	}

	cfg.Network = p.Network == NetworkOnline
}
