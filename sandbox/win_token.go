//go:build windows

package sandbox

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Phase 8 D29: Restricted Token management
//
// Creates tokens with minimal privileges for sandboxed process execution.
// Aligns with Codex codex-windows-sandbox Restricted Token approach.

const (
	DISABLE_MAX_PRIVILEGE = 0x1
	SANDBOX_INERT         = 0x2

	TokenIntegrityLevel   = 25 // TOKEN_INFORMATION_CLASS
	SE_GROUP_INTEGRITY    = 0x00000020

	WinUntrustedLabelSid  = 66 // WELL_KNOWN_SID_TYPE
)

// tokenMandatoryLabel removed — inlined in setTokenIntegrityLevel

// createSandboxToken creates a restricted token with:
//   - All privileges removed (DISABLE_MAX_PRIVILEGE)
//   - Integrity Level set to Untrusted (0x0000)
//   - SANDBOX_INERT flag set
func createSandboxToken() (syscall.Token, error) {
	// Get current process token
	var processToken syscall.Token
	process, _ := syscall.GetCurrentProcess()
	err := syscall.OpenProcessToken(process, syscall.TOKEN_ALL_ACCESS, &processToken)
	if err != nil {
		return 0, fmt.Errorf("open process token: %w", err)
	}
	defer processToken.Close()

	// Create restricted token — strip all privileges
	var restrictedToken syscall.Token
	r, _, callErr := procCreateRestrictedToken.Call(
		uintptr(processToken),
		DISABLE_MAX_PRIVILEGE|SANDBOX_INERT,
		0, 0, // no SIDs to disable
		0, 0, // no privileges to delete (DISABLE_MAX_PRIVILEGE handles it)
		0, 0, // no restricting SIDs
		uintptr(unsafe.Pointer(&restrictedToken)),
	)
	if r == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", callErr)
	}

	// Set integrity level to Untrusted
	if err := setTokenIntegrityLevel(restrictedToken, SECURITY_MANDATORY_UNTRUSTED_RID); err != nil {
		restrictedToken.Close()
		return 0, fmt.Errorf("set integrity level: %w", err)
	}

	return restrictedToken, nil
}

// setTokenIntegrityLevel sets the token's mandatory integrity level.
func setTokenIntegrityLevel(token syscall.Token, level uint32) error {
	// Create SID for the integrity level using Ntdll
	authority := sidIdentifierAuthority{Value: [6]byte{0, 0, 0, 0, 0, 16}} // MANDATORY_LABEL_AUTHORITY
	var sid uintptr

	r, _, err := procRtlAllocateAndInitializeSid.Call(
		uintptr(unsafe.Pointer(&authority)),
		1,              // sub-authority count
		uintptr(level), // RID
		0, 0, 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&sid)),
	)
	if r != 0 {
		return fmt.Errorf("allocate integrity SID: NTSTATUS 0x%X: %w", r, err)
	}
	defer procRtlFreeSid.Call(sid)

	type sidAndAttrs struct {
		Sid        uintptr
		Attributes uint32
	}
	type tml struct {
		Label sidAndAttrs
	}
	info := tml{Label: sidAndAttrs{Sid: sid, Attributes: SE_GROUP_INTEGRITY}}

	r2, _, callErr := procSetTokenInformation.Call(
		uintptr(token),
		uintptr(TokenIntegrityLevel),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r2 == 0 {
		return fmt.Errorf("SetTokenInformation: %w", callErr)
	}
	return nil
}
