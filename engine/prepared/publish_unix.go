//go:build unix

package prepared

import (
	"errors"
	"os"
	"syscall"
)

// noHardLinks reports whether a link(2) error is one that filesystems
// without hard links answer (FAT/exFAT and some network filesystems give
// EPERM or ENOTSUP). It classifies, it does not establish the cause: EPERM
// in particular can mean other things, which is why ErrNoHardLinks is worded
// as "may not support". Either way publication fails closed.
func noHardLinks(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EXDEV)
}

// syncDir fsyncs a directory, making a new entry in it power-loss-durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
