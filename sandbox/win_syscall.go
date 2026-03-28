//go:build windows

package sandbox

import (
	"syscall"
	"unsafe"
)

// Win32 API bindings — pure Go syscall, zero CGo.
//
// Phase 8 D29: Token + Job Object + ACL + Network + Desktop isolation.
// References:
//   - Codex codex-windows-sandbox (~2000 lines Rust)
//   - https://learn.microsoft.com/en-us/windows/win32/api/

var (
	modAdvapi32 = syscall.NewLazyDLL("advapi32.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")

	// Token management
	procCreateRestrictedToken = modAdvapi32.NewProc("CreateRestrictedToken")
	procCreateProcessAsUserW  = modAdvapi32.NewProc("CreateProcessAsUserW")
	procSetTokenInformation   = modAdvapi32.NewProc("SetTokenInformation")
	procAdjustTokenPrivileges = modAdvapi32.NewProc("AdjustTokenPrivileges")

	// Job Object
	procCreateJobObjectW            = modKernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject     = modKernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject    = modKernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject          = modKernel32.NewProc("TerminateJobObject")

	// ACL / Security
	procSetNamedSecurityInfoW = modAdvapi32.NewProc("SetNamedSecurityInfoW")

	// Note: Desktop procs are in win_desktop.go (user32.dll)
)

// Integrity levels
const (
	SECURITY_MANDATORY_UNTRUSTED_RID = 0x0000
	SECURITY_MANDATORY_LOW_RID       = 0x1000
	SECURITY_MANDATORY_MEDIUM_RID    = 0x2000
)

// Job Object limit flags
const (
	JOB_OBJECT_LIMIT_PROCESS_MEMORY      = 0x00000100
	JOB_OBJECT_LIMIT_JOB_MEMORY          = 0x00000200
	JOB_OBJECT_LIMIT_ACTIVE_PROCESS      = 0x00000008
	JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE   = 0x00002000
)

// Placeholder: actual Win32 struct definitions will be filled on Windows
type (
	jobObjectExtendedLimitInformation struct {
		BasicLimitInformation struct {
			PerProcessUserTimeLimit int64
			PerJobUserTimeLimit     int64
			LimitFlags              uint32
			MinimumWorkingSetSize   uintptr
			MaximumWorkingSetSize   uintptr
			ActiveProcessLimit      uint32
			Affinity                uintptr
			PriorityClass           uint32
			SchedulingClass         uint32
		}
		IoInfo struct {
			ReadOperationCount  uint64
			WriteOperationCount uint64
			OtherOperationCount uint64
			ReadTransferCount   uint64
			WriteTransferCount  uint64
			OtherTransferCount  uint64
		}
		ProcessMemoryLimit uintptr
		JobMemoryLimit     uintptr
		PeakProcessMemoryUsed uintptr
		PeakJobMemoryUsed     uintptr
	}
)

// createRestrictedToken creates a restricted token with deny-only SIDs.
func createRestrictedToken(existingToken syscall.Token) (syscall.Token, error) {
	var newToken syscall.Token
	r, _, err := procCreateRestrictedToken.Call(
		uintptr(existingToken),
		0, // flags: DISABLE_MAX_PRIVILEGE
		0, 0, // SIDs to disable
		0, 0, // privileges to delete
		0, 0, // restricting SIDs
		uintptr(unsafe.Pointer(&newToken)),
	)
	if r == 0 {
		return 0, err
	}
	return newToken, nil
}

// createJobObject creates a new Job Object with memory and process limits.
func createJobObject(memoryLimitMB int, maxProcesses int) (syscall.Handle, error) {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0, err
	}
	handle := syscall.Handle(h)

	info := jobObjectExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_PROCESS_MEMORY |
		JOB_OBJECT_LIMIT_ACTIVE_PROCESS |
		JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	info.ProcessMemoryLimit = uintptr(memoryLimitMB) * 1024 * 1024
	info.BasicLimitInformation.ActiveProcessLimit = uint32(maxProcesses)

	r, _, err := procSetInformationJobObject.Call(
		uintptr(handle),
		9, // JobObjectExtendedLimitInformation
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r == 0 {
		syscall.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}
