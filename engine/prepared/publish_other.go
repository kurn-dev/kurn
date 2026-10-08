//go:build !unix

package prepared

import "errors"

// noHardLinks: without POSIX errno values to distinguish, only an explicit
// unsupported-operation error is classified as "no hard links".
func noHardLinks(err error) bool { return errors.Is(err, errors.ErrUnsupported) }

// syncDir is a no-op where directories cannot be opened for fsync.
func syncDir(string) error { return nil }
