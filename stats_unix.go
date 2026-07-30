//go:build !windows

package main

import "syscall"

// diskUsage reports the size and free space of the filesystem containing
// path. Linux and Darwin's syscall.Statfs_t differ in the width of some
// fields (Bsize is int64 on Linux, uint32 on Darwin) but share the field
// names used here, so one implementation covers both.
func diskUsage(path string) (total, free uint64, err error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, err
	}
	total = uint64(fs.Bsize) * fs.Blocks
	free = uint64(fs.Bsize) * fs.Bfree
	return total, free, nil
}
