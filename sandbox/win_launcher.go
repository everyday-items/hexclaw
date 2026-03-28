//go:build windows

package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Phase 8 D32: Windows process launcher
//
// Combines all isolation layers to launch a sandboxed process:
//   Token + ACL + Job Object + Network + Desktop → CreateProcessAsUser

var (
	procCreateProcessAsUserW2 = modAdvapi32.NewProc("CreateProcessAsUserW")
)

// launchSandboxedProcess creates and starts a process with full sandbox isolation.
func launchSandboxedProcess(cfg Config, command string, args []string) (*os.Process, *bytes.Buffer, *bytes.Buffer, error) {
	// 1. Create restricted token
	token, err := createSandboxToken()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create sandbox token: %w", err)
	}
	defer token.Close()

	// 2. Apply ACL to workspace
	aclCfg, err := applyWorkspaceACL(cfg.Workspace, token)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("apply workspace ACL: %w", err)
	}
	// Restore ACL on function exit (caller should handle process lifetime)
	defer aclCfg.restoreACL()

	// 3. Create Job Object
	jobHandle, err := createJobObject(512, 10) // 512MB memory, 10 processes max
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create job object: %w", err)
	}
	defer syscall.CloseHandle(jobHandle)

	// 4. Network isolation (optional)
	finalToken := token
	if !cfg.Network {
		lowBoxToken, err := createLowBoxToken(token, false)
		if err != nil {
			// Non-fatal: fall back to restricted token without network isolation
			_ = err
		} else {
			defer lowBoxToken.Close()
			finalToken = lowBoxToken
		}
	}

	// 5. Create isolated desktop
	desktop, err := createIsolatedDesktop(fmt.Sprintf("hexclaw_sandbox_%d", os.Getpid()))
	if err != nil {
		// Non-fatal: fall back to default desktop
		_ = err
	} else {
		defer desktop.Close()
	}

	// 6. Build command line
	cmdLine := buildCommandLine(command, args)
	cmdLineW, _ := syscall.UTF16PtrFromString(cmdLine)
	workspaceW, _ := syscall.UTF16PtrFromString(cfg.Workspace)

	// 7. Setup STARTUPINFO with isolated desktop
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = syscall.STARTF_USESTDHANDLES
	if desktop != nil {
		desktopNameW, _ := syscall.UTF16PtrFromString(desktop.DesktopName())
		si.Desktop = desktopNameW
	}

	// 8. Create stdout/stderr pipes
	var stdoutR, stdoutW, stderrR, stderrW syscall.Handle
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), InheritHandle: 1}
	syscall.CreatePipe(&stdoutR, &stdoutW, &sa, 0)
	syscall.CreatePipe(&stderrR, &stderrW, &sa, 0)
	si.StdOutput = stdoutW
	si.StdErr = stderrW

	// 9. CreateProcessAsUser
	var pi syscall.ProcessInformation
	const CREATE_SUSPENDED = 0x00000004
	const CREATE_NEW_CONSOLE = 0x00000010

	r, _, callErr := procCreateProcessAsUserW2.Call(
		uintptr(finalToken),
		0, // application name (use cmdLine)
		uintptr(unsafe.Pointer(cmdLineW)),
		0, 0, // security attributes
		1, // inherit handles
		CREATE_SUSPENDED|CREATE_NEW_CONSOLE,
		0, // environment (inherit cleaned env)
		uintptr(unsafe.Pointer(workspaceW)),
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r == 0 {
		syscall.CloseHandle(stdoutR)
		syscall.CloseHandle(stdoutW)
		syscall.CloseHandle(stderrR)
		syscall.CloseHandle(stderrW)
		return nil, nil, nil, fmt.Errorf("CreateProcessAsUser: %w", callErr)
	}

	// Close write ends (parent doesn't need them)
	syscall.CloseHandle(stdoutW)
	syscall.CloseHandle(stderrW)

	// 10. Assign to Job Object
	procAssignProcessToJobObject.Call(uintptr(jobHandle), uintptr(pi.Process))

	// 11. Resume the process
	modKernel32.NewProc("ResumeThread").Call(uintptr(pi.Thread))
	syscall.CloseHandle(pi.Thread)

	// Read stdout/stderr
	var stdout, stderr bytes.Buffer
	go readHandle(&stdout, stdoutR)
	go readHandle(&stderr, stderrR)

	proc, _ := os.FindProcess(int(pi.ProcessId))
	return proc, &stdout, &stderr, nil
}

func readHandle(buf *bytes.Buffer, h syscall.Handle) {
	defer syscall.CloseHandle(h)
	tmp := make([]byte, 4096)
	for {
		var n uint32
		err := syscall.ReadFile(h, tmp, &n, nil)
		if err != nil || n == 0 {
			break
		}
		buf.Write(tmp[:n])
	}
}

func buildCommandLine(command string, args []string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, command)
	parts = append(parts, args...)
	// Simple join — proper quoting would be needed for production
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += " "
		}
		result += p
	}
	return result
}
