//go:build windows

package builtin

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type codeExecWindowsFileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

func codeExecPlatformFileIdentity(file *os.File, info os.FileInfo) (codeExecPlatformIdentity, error) {
	if file == nil {
		return codeExecPlatformIdentity{}, errCodeExecFileIdentityUnavailable
	}
	connection, err := file.SyscallConn()
	if err != nil {
		return codeExecPlatformIdentity{}, errCodeExecFileIdentityUnavailable
	}
	var identity codeExecPlatformIdentity
	var queryErr error
	if err := connection.Control(func(raw uintptr) {
		handle := windows.Handle(raw)
		var handleInfo windows.ByHandleFileInformation
		if queryErr = windows.GetFileInformationByHandle(handle, &handleInfo); queryErr != nil {
			return
		}
		var basic codeExecWindowsFileBasicInfo
		queryErr = windows.GetFileInformationByHandleEx(
			handle,
			windows.FileBasicInfo,
			(*byte)(unsafe.Pointer(&basic)),
			uint32(unsafe.Sizeof(basic)),
		)
		if queryErr != nil {
			return
		}
		if handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
			basic.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			queryErr = errors.New("opened file is a reparse point")
			return
		}
		identity = codeExecPlatformIdentity{
			Volume:           uint64(handleInfo.VolumeSerialNumber),
			FileIDHigh:       uint64(handleInfo.FileIndexHigh),
			FileIDLow:        uint64(handleInfo.FileIndexLow),
			Links:            uint64(handleInfo.NumberOfLinks),
			ChangeTimeSec:    basic.ChangeTime,
			ChangeTimeNsec:   0,
			ChangeTimeKnown:  true,
			NoFollowVerified: true,
		}
	}); err != nil {
		return codeExecPlatformIdentity{}, errCodeExecFileIdentityUnavailable
	}
	if queryErr != nil || identity.Links == 0 {
		return codeExecPlatformIdentity{}, errCodeExecFileIdentityUnavailable
	}
	return identity, nil
}

func codeExecPlatformPathIdentity(os.FileInfo) (codeExecPlatformIdentity, bool) {
	return codeExecPlatformIdentity{}, false
}

func openCodeExecRegularFileNoFollow(root *os.Root, name string) (*os.File, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || filepath.Dir(clean) != "." {
		return nil, errors.New("invalid rooted file name")
	}
	path := filepath.Join(root.Name(), clean)
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_SEQUENTIAL_SCAN,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("bind opened Windows file handle failed")
	}
	return file, nil
}
