//go:build linux

package builtin

import (
	"os"
	"syscall"
)

func codeExecPlatformFileIdentity(file *os.File, info os.FileInfo) (codeExecPlatformIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return codeExecPlatformIdentity{}, errCodeExecFileIdentityUnavailable
	}
	return codeExecPlatformIdentity{
		Volume:           uint64(stat.Dev),
		FileIDHigh:       0,
		FileIDLow:        uint64(stat.Ino),
		Links:            uint64(stat.Nlink),
		ChangeTimeSec:    stat.Ctim.Sec,
		ChangeTimeNsec:   stat.Ctim.Nsec,
		ChangeTimeKnown:  true,
		NoFollowVerified: true,
	}, nil
}

func codeExecPlatformPathIdentity(info os.FileInfo) (codeExecPlatformIdentity, bool) {
	identity, err := codeExecPlatformFileIdentity(nil, info)
	return identity, err == nil
}

func openCodeExecRegularFileNoFollow(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
