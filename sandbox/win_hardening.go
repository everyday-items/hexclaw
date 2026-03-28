//go:build windows

package sandbox

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// Phase 8 D34: Security hardening + escape prevention

// Job Object UI restrictions
const (
	JOB_OBJECT_UILIMIT_DESKTOP          = 0x00000040
	JOB_OBJECT_UILIMIT_GLOBALATOMS      = 0x00000020
	JOB_OBJECT_UILIMIT_HANDLES          = 0x00000001
	JOB_OBJECT_UILIMIT_READCLIPBOARD    = 0x00000002
	JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS = 0x00000008
	JOB_OBJECT_UILIMIT_WRITECLIPBOARD   = 0x00000004
	JOB_OBJECT_UILIMIT_EXITWINDOWS      = 0x00000080
	JOB_OBJECT_UILIMIT_DISPLAYSETTINGS  = 0x00000010
)

type jobObjectBasicUIRestrictions struct {
	UIRestrictionsClass uint32
}

// hardenJobObject applies UI restrictions to the Job Object.
//
// Blocks:
//   - Clipboard read/write (data exfiltration)
//   - Global atom table (IPC attack vector)
//   - Desktop creation (escape to new desktop)
//   - System parameter changes
//   - Display settings modification
func hardenJobObject(jobHandle syscall.Handle) error {
	restrictions := jobObjectBasicUIRestrictions{
		UIRestrictionsClass: JOB_OBJECT_UILIMIT_READCLIPBOARD |
			JOB_OBJECT_UILIMIT_WRITECLIPBOARD |
			JOB_OBJECT_UILIMIT_GLOBALATOMS |
			JOB_OBJECT_UILIMIT_DESKTOP |
			JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS |
			JOB_OBJECT_UILIMIT_DISPLAYSETTINGS |
			JOB_OBJECT_UILIMIT_EXITWINDOWS,
	}

	r, _, err := procSetInformationJobObject.Call(
		uintptr(jobHandle),
		7, // JobObjectBasicUIRestrictions
		uintptr(unsafe.Pointer(&restrictions)),
		unsafe.Sizeof(restrictions),
	)
	if r == 0 {
		return err
	}
	return nil
}

// cleanWindowsEnv returns a sanitized environment for sandboxed processes.
//
// Strips dangerous variables:
//   - COMSPEC (cmd.exe path — prevents shell escape)
//   - PSModulePath (PowerShell module injection)
//   - PROCESSOR_ARCHITECTURE (info leak)
//   - USERNAME, USERDOMAIN (identity leak)
//   - APPDATA, LOCALAPPDATA (out-of-workspace access)
func cleanWindowsEnv(workspace string) []string {
	dangerousVars := map[string]bool{
		"COMSPEC":                true,
		"PSMODULEPATH":           true,
		"PROCESSOR_ARCHITECTURE": true,
		"USERNAME":               true,
		"USERDOMAIN":             true,
		"APPDATA":                true,
		"LOCALAPPDATA":           true,
		"USERPROFILE":            true,
		"HOMEPATH":               true,
		"HOMEDRIVE":              true,
	}

	// Keep only safe variables
	var clean []string
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToUpper(parts[0])
		if dangerousVars[key] {
			continue
		}
		// Allow PATH, SYSTEMROOT, TEMP, TMP
		if key == "PATH" || key == "SYSTEMROOT" || key == "SYSTEMDRIVE" || key == "WINDIR" {
			clean = append(clean, env)
		}
	}

	// Override with sandbox-safe values
	clean = append(clean,
		"TEMP="+workspace+"\\_tmp",
		"TMP="+workspace+"\\_tmp",
		"USERPROFILE="+workspace,
		"HOMEPATH="+workspace,
		"APPDATA="+workspace+"\\_appdata",
		"LOCALAPPDATA="+workspace+"\\_localappdata",
	)

	os.MkdirAll(workspace+"\\_tmp", 0755)
	os.MkdirAll(workspace+"\\_appdata", 0755)
	os.MkdirAll(workspace+"\\_localappdata", 0755)

	return clean
}

// validateWindowsEscapeVectors checks for common escape attempts in command/args.
func validateWindowsEscapeVectors(command string, args []string) error {
	all := append([]string{command}, args...)
	for _, s := range all {
		if err := validateWindowsPath(s); err != nil {
			return err
		}
		// Block PowerShell invocation
		lower := strings.ToLower(s)
		if strings.Contains(lower, "powershell") || strings.Contains(lower, "pwsh") {
			return fmt.Errorf("PowerShell invocation blocked: %s", s)
		}
		// Block cmd.exe invocation
		if strings.Contains(lower, "cmd.exe") || strings.Contains(lower, "cmd /") {
			return fmt.Errorf("cmd.exe invocation blocked: %s", s)
		}
	}
	return nil
}
