//go:build windows

package blob

import (
	"syscall"
	"unsafe"
)

// GetDiskFreeSpaceExW is the Win32 API for disk capacity. Loaded lazily from
// kernel32 so this stays stdlib-only (no golang.org/x/sys dependency).
var (
	modkernel32            = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceEx = modkernel32.NewProc("GetDiskFreeSpaceExW")
)

// Capacity reports the disk usage of the blob store root via GetDiskFreeSpaceEx.
// used = total − free; total = full volume size.
func (f *FS) Capacity() (used, total int64, err error) {
	rootPtr, err := syscall.UTF16PtrFromString(f.root)
	if err != nil {
		return 0, 0, err
	}
	var freeAvailable, totalBytes, totalFree uint64
	r1, _, callErr := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(rootPtr)),
		uintptr(unsafe.Pointer(&freeAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r1 == 0 {
		return 0, 0, callErr
	}
	total = int64(totalBytes)
	used = int64(totalBytes - totalFree)
	return used, total, nil
}
