//go:build windows

package sandbox

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Phase 8 D31: Alternate Desktop + UIPI
//
// Creates an isolated desktop to prevent Shatter Attack.
// User Interface Privilege Isolation (UIPI) blocks low→high integrity messages.

var (
	modUser32           = syscall.NewLazyDLL("user32.dll")
	procCreateDesktopW  = modUser32.NewProc("CreateDesktopW")
	procCloseDesktop    = modUser32.NewProc("CloseDesktop")
)

const (
	DESKTOP_CREATEWINDOW = 0x0002
	DESKTOP_READOBJECTS  = 0x0001
	DESKTOP_WRITEOBJECTS = 0x0080
	GENERIC_ALL_DESKTOP  = 0x000F01FF
)

// desktopHandle holds a reference to an isolated desktop.
type desktopHandle struct {
	handle uintptr
	name   string
}

// createIsolatedDesktop creates a new desktop for sandboxed processes.
//
// The desktop is separate from the default desktop, preventing:
//   - Shatter Attack (sending WM_* messages to other windows)
//   - Keylogging via SetWindowsHookEx on default desktop
//   - Screen capture of other applications
func createIsolatedDesktop(name string) (*desktopHandle, error) {
	nameW, _ := syscall.UTF16PtrFromString(name)

	h, _, err := procCreateDesktopW.Call(
		uintptr(unsafe.Pointer(nameW)),
		0, // device (reserved)
		0, // devmode (reserved)
		0, // flags
		DESKTOP_CREATEWINDOW|DESKTOP_READOBJECTS|DESKTOP_WRITEOBJECTS, // access
		0, // security attributes
	)
	if h == 0 {
		return nil, fmt.Errorf("CreateDesktop %q: %w", name, err)
	}

	return &desktopHandle{handle: h, name: name}, nil
}

// Close releases the desktop handle.
func (d *desktopHandle) Close() error {
	if d.handle == 0 {
		return nil
	}
	r, _, err := procCloseDesktop.Call(d.handle)
	if r == 0 {
		return fmt.Errorf("CloseDesktop: %w", err)
	}
	d.handle = 0
	return nil
}

// DesktopName returns the name for use in STARTUPINFO.lpDesktop.
func (d *desktopHandle) DesktopName() string {
	return d.name
}
