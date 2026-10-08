//go:build unix

package prepared

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestPublishWithoutHardLinksFailsExplicitly(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOTSUP, syscall.EPERM} {
		dir := t.TempDir()
		dst := filepath.Join(dir, "list.kurn")
		linkFile = func(oldname, newname string) error {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errno}
		}
		_, err := PublishBytes(dst, artifactT(t, "exact", 5))
		linkFile = os.Link
		if !errors.Is(err, ErrNoHardLinks) {
			t.Fatalf("%v: got %v, want ErrNoHardLinks", errno, err)
		}
		if names := readDirNames(t, dir); len(names) != 0 {
			t.Fatalf("%v: left %v behind (no overwrite fallback may run)", errno, names)
		}
	}
	// Any other link failure is reported as itself, not as missing support.
	dir := t.TempDir()
	linkFile = func(string, string) error { return syscall.EIO }
	_, err := PublishBytes(filepath.Join(dir, "list.kurn"), []byte("x"))
	linkFile = os.Link
	if err == nil || errors.Is(err, ErrNoHardLinks) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("got %v", err)
	}
	if names := readDirNames(t, dir); !slices.Equal(names, []string(nil)) {
		t.Fatalf("left %v behind", names)
	}
}
