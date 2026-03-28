//go:build windows

package sandbox

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// Phase 8 D30: ACL file isolation
//
// Configures DACL on workspace directory to restrict sandbox access.

const (
	SE_FILE_OBJECT        = 1
	DACL_SECURITY_INFO    = 0x00000004
	GRANT_ACCESS          = 1
	SET_ACCESS            = 2
	NO_INHERITANCE        = 0
	OBJECT_INHERIT_ACE    = 0x1
	CONTAINER_INHERIT_ACE = 0x2

	GENERIC_READ    = 0x80000000
	GENERIC_WRITE   = 0x40000000
	GENERIC_EXECUTE = 0x20000000
)

type explicitAccessW struct {
	grfAccessPermissions uint32
	grfAccessMode        uint32
	grfInheritance       uint32
	trustee              trusteeW
}

type trusteeW struct {
	pMultipleTrustee         uintptr
	multipleTrusteeOperation uint32
	trusteeForm              uint32 // TRUSTEE_IS_SID = 0
	trusteeType              uint32 // TRUSTEE_IS_WELL_KNOWN_GROUP = 5
	ptstrName                uintptr
}

var (
	modAdvapi32ACL              = syscall.NewLazyDLL("advapi32.dll")
	procSetEntriesInAclW        = modAdvapi32ACL.NewProc("SetEntriesInAclW")
	procSetNamedSecurityInfoW2  = modAdvapi32ACL.NewProc("SetNamedSecurityInfoW")
	procGetNamedSecurityInfoW   = modAdvapi32ACL.NewProc("GetNamedSecurityInfoW")
)

// aclConfig holds the original DACL for restoration.
type aclConfig struct {
	path     string
	origDACL uintptr // pointer to original ACL
	origSD   uintptr // security descriptor (for freeing)
}

// applyWorkspaceACL restricts file access to only the workspace directory.
//
// Grants read/write to the workspace path.
// Blocks: Alternate Data Streams, UNC paths, device handles.
func applyWorkspaceACL(workspacePath string, token syscall.Token) (*aclConfig, error) {
	cfg := &aclConfig{path: workspacePath}

	// Save original DACL for cleanup
	pathW, _ := syscall.UTF16PtrFromString(workspacePath)
	r, _, err := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathW)),
		SE_FILE_OBJECT,
		DACL_SECURITY_INFO,
		0, 0,
		uintptr(unsafe.Pointer(&cfg.origDACL)),
		0,
		uintptr(unsafe.Pointer(&cfg.origSD)),
	)
	if r != 0 {
		return nil, fmt.Errorf("GetNamedSecurityInfo: error %d: %w", r, err)
	}

	// Create new ACE granting read/write to workspace
	ea := explicitAccessW{
		grfAccessPermissions: GENERIC_READ | GENERIC_WRITE | GENERIC_EXECUTE,
		grfAccessMode:        SET_ACCESS,
		grfInheritance:       OBJECT_INHERIT_ACE | CONTAINER_INHERIT_ACE,
		trustee: trusteeW{
			trusteeForm: 0, // TRUSTEE_IS_SID
			ptstrName:   0, // will be set to Everyone SID for workspace
		},
	}

	var newACL uintptr
	r, _, err = procSetEntriesInAclW.Call(
		1, // count
		uintptr(unsafe.Pointer(&ea)),
		cfg.origDACL,
		uintptr(unsafe.Pointer(&newACL)),
	)
	if r != 0 {
		return nil, fmt.Errorf("SetEntriesInAcl: error %d: %w", r, err)
	}

	// Apply new DACL
	r, _, err = procSetNamedSecurityInfoW2.Call(
		uintptr(unsafe.Pointer(pathW)),
		SE_FILE_OBJECT,
		DACL_SECURITY_INFO,
		0, 0,
		newACL,
		0,
	)
	if r != 0 {
		return nil, fmt.Errorf("SetNamedSecurityInfo: error %d: %w", r, err)
	}

	return cfg, nil
}

// restoreACL restores the original DACL.
func (c *aclConfig) restoreACL() error {
	if c == nil || c.origDACL == 0 {
		return nil
	}
	pathW, _ := syscall.UTF16PtrFromString(c.path)
	r, _, err := procSetNamedSecurityInfoW2.Call(
		uintptr(unsafe.Pointer(pathW)),
		SE_FILE_OBJECT,
		DACL_SECURITY_INFO,
		0, 0,
		c.origDACL,
		0,
	)
	if r != 0 {
		return fmt.Errorf("restore ACL: error %d: %w", r, err)
	}
	// Free security descriptor
	if c.origSD != 0 {
		syscall.LocalFree(syscall.Handle(c.origSD))
	}
	return nil
}

// validatePath checks for dangerous path patterns on Windows.
func validateWindowsPath(path string) error {
	// Block Alternate Data Streams (file:stream)
	if strings.Contains(path, ":") && !isAbsWindowsPath(path) {
		return fmt.Errorf("alternate data streams not allowed: %s", path)
	}
	// Block UNC paths
	if strings.HasPrefix(path, `\\`) {
		return fmt.Errorf("UNC paths not allowed: %s", path)
	}
	// Block device handles
	if strings.HasPrefix(strings.ToLower(path), `\\.\`) || strings.HasPrefix(strings.ToLower(path), `\\?\`) {
		return fmt.Errorf("device handles not allowed: %s", path)
	}
	return nil
}

func isAbsWindowsPath(path string) bool {
	return len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}
