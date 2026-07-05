//go:build !windows

package blob

import "syscall"

// Capacity reports the disk usage of the blob store root via syscall.Statfs.
// used = total − available (includes reserved blocks); total = full disk size.
func (f *FS) Capacity() (used, total int64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(f.root, &st); err != nil {
		return 0, 0, err
	}
	total = int64(st.Blocks) * int64(st.Bsize)  // #nosec G115 -- disk block count × block size, no realistic int64 overflow
	avail := int64(st.Bavail) * int64(st.Bsize) // #nosec G115 -- disk block count × block size, no realistic int64 overflow
	used = total - avail
	return used, total, nil
}
