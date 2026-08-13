//go:build darwin

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
		// Darwin 的 dev_t 是有符号 32 位设备号；这里有意保留内核位模式后再拓宽。
		Volume:           uint64(uint32(stat.Dev)), // #nosec G115 -- 按设备号位模式转换，不是数值符号转换。
		FileIDHigh:       0,
		FileIDLow:        stat.Ino,
		Links:            uint64(stat.Nlink),
		ChangeTimeSec:    stat.Ctimespec.Sec,
		ChangeTimeNsec:   stat.Ctimespec.Nsec,
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
