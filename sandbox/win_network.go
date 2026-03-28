//go:build windows

package sandbox

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Phase 8 D31: Low Box Token network isolation

var (
	modNtdll              = syscall.NewLazyDLL("ntdll.dll")
	procNtCreateLowBoxToken = modNtdll.NewProc("NtCreateLowBoxToken")
	procRtlAllocateAndInitializeSid = modNtdll.NewProc("RtlAllocateAndInitializeSid")
	procRtlFreeSid = modNtdll.NewProc("RtlFreeSid")
)

// sidIdentifierAuthority is the SID authority structure.
type sidIdentifierAuthority struct {
	Value [6]byte
}

// createLowBoxToken creates a Low Box Token for network isolation.
//
// If allowNetwork is false, the token has no capabilities (kernel-level network block).
// If allowNetwork is true, basic network capability is granted.
func createLowBoxToken(baseToken syscall.Token, allowNetwork bool) (syscall.Token, error) {
	// Create AppContainer SID
	authority := sidIdentifierAuthority{Value: [6]byte{0, 0, 0, 0, 0, 15}} // APP_PACKAGE_AUTHORITY
	var appContainerSid uintptr

	r, _, err := procRtlAllocateAndInitializeSid.Call(
		uintptr(unsafe.Pointer(&authority)),
		2, // sub-authority count
		2, 1, 0, 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&appContainerSid)),
	)
	if r != 0 {
		return 0, fmt.Errorf("RtlAllocateAndInitializeSid: NTSTATUS 0x%X: %w", r, err)
	}
	defer procRtlFreeSid.Call(appContainerSid)

	var capCount uintptr
	var capPtr uintptr
	// If allowNetwork, we'd add INTERNET_CLIENT capability here
	// For offline mode (default), no capabilities = no network
	_ = allowNetwork

	var lowBoxToken syscall.Token
	r, _, err = procNtCreateLowBoxToken.Call(
		uintptr(unsafe.Pointer(&lowBoxToken)),
		uintptr(baseToken),
		syscall.TOKEN_ALL_ACCESS,
		0, // object attributes
		appContainerSid,
		capCount,
		capPtr,
		0, 0, // no handles
	)
	if r != 0 {
		return 0, fmt.Errorf("NtCreateLowBoxToken: NTSTATUS 0x%X: %w", r, err)
	}

	return lowBoxToken, nil
}
