//go:build windows

package sandbox

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Additional Job Object UI restriction flags not in win_syscall.go
const (
	jOB_OBJECT_UILIMIT_HANDLES          = 0x00000001
	jOB_OBJECT_UILIMIT_READCLIPBOARD    = 0x00000002
	jOB_OBJECT_UILIMIT_WRITECLIPBOARD   = 0x00000004
	jOB_OBJECT_UILIMIT_SYSTEMPARAMETERS = 0x00000008
	jOB_OBJECT_UILIMIT_DISPLAYSETTINGS  = 0x00000010
	jOB_OBJECT_UILIMIT_GLOBALATOMS      = 0x00000020
	jOB_OBJECT_UILIMIT_DESKTOP          = 0x00000040
	jOB_OBJECT_UILIMIT_EXITWINDOWS      = 0x00000080

	jobObjectBasicUIRestrictionsClass = 4
)

type jobobjectBasicUIRestrictions2 struct {
	UIRestrictionsClass uint32
}

// createSandboxJobObject creates a fully configured Job Object with memory, process,
// and UI restrictions for sandbox isolation. Wraps the basic createJobObject from
// win_syscall.go with additional hardening.
func createSandboxJobObject(memoryMB int, maxProcesses int) (syscall.Handle, error) {
	job, err := createJobObject(memoryMB, maxProcesses)
	if err != nil {
		return 0, err
	}

	if err := setJobUIRestrictions(job); err != nil {
		syscall.CloseHandle(job)
		return 0, err
	}

	return job, nil
}

// setJobUIRestrictions blocks clipboard access, global hooks, atom table,
// desktop creation, display settings changes, and inter-process handle access.
func setJobUIRestrictions(job syscall.Handle) error {
	restrictions := jobobjectBasicUIRestrictions2{
		UIRestrictionsClass: jOB_OBJECT_UILIMIT_DESKTOP |
			jOB_OBJECT_UILIMIT_DISPLAYSETTINGS |
			jOB_OBJECT_UILIMIT_EXITWINDOWS |
			jOB_OBJECT_UILIMIT_GLOBALATOMS |
			jOB_OBJECT_UILIMIT_HANDLES |
			jOB_OBJECT_UILIMIT_READCLIPBOARD |
			jOB_OBJECT_UILIMIT_SYSTEMPARAMETERS |
			jOB_OBJECT_UILIMIT_WRITECLIPBOARD,
	}

	r, _, err := procSetInformationJobObject.Call(
		uintptr(job),
		uintptr(jobObjectBasicUIRestrictionsClass),
		uintptr(unsafe.Pointer(&restrictions)),
		unsafe.Sizeof(restrictions),
	)
	if r == 0 {
		return fmt.Errorf("SetInformationJobObject (UI restrictions): %w", err)
	}
	return nil
}

// assignProcessToJob assigns a process handle to the Job Object.
func assignProcessToJob(job syscall.Handle, process syscall.Handle) error {
	r, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(process))
	if r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// terminateJob terminates all processes in the Job Object.
func terminateJob(job syscall.Handle, exitCode uint32) error {
	r, _, err := procTerminateJobObject.Call(uintptr(job), uintptr(exitCode))
	if r == 0 {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	return nil
}
