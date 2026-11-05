//go:build linux

package metrics

import "syscall"

func statfs(path string) (FSStat, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return FSStat{}, err
	}

	size := uint64(s.Frsize)
	if size == 0 {
		size = uint64(s.Bsize)
	}

	return FSStat{Blocks: s.Blocks, BlocksFree: s.Bfree, BlocksAvail: s.Bavail, BlockSize: size, Files: s.Files, FilesFree: s.Ffree}, nil
}
