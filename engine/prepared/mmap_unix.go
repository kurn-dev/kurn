//go:build unix

package prepared

import (
	"fmt"
	"os"
	"syscall"
)

const mmapSupported = true

// mapFile maps the first size bytes of f read-only and shared. The mapping
// starts at a page boundary and stays valid after f is closed.
func mapFile(f *os.File, size int) ([]byte, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var buf []byte
	var merr error
	if err := rc.Control(func(fd uintptr) {
		buf, merr = syscall.Mmap(int(fd), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	}); err != nil {
		return nil, err
	}
	if merr != nil {
		return nil, merr
	}
	return buf, nil
}

// unmapFile releases a mapping that was never exposed.
func unmapFile(b []byte) error { return syscall.Munmap(b) }

// fileKey returns the device and inode from an fstat result.
func fileKey(fi os.FileInfo) (dev, ino uint64, err error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("prepared: mapped loader: %s: no device and inode in the file status", fi.Name())
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}
