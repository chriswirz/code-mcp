package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// diskUsage reports the size and free space of the volume containing path,
// via the same Win32 call Explorer's own disk-space bar uses. There is no
// stdlib wrapper for it, so this calls kernel32 directly rather than adding a
// dependency for one function.
func diskUsage(path string) (total, free uint64, err error) {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeAvail, totalBytes, totalFree uint64
	ret, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 {
		return 0, 0, callErr
	}
	return totalBytes, totalFree, nil
}
