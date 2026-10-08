package prepared

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TempPrefix and TempSuffix name the temporary files Publish creates in the
// destination directory: TempPrefix + random + TempSuffix.
const (
	TempPrefix = ".kurnart-"
	TempSuffix = ".tmp"
)

var (
	// ErrExists is returned when the destination name already exists.
	// Publication never replaces anything.
	ErrExists = errors.New("prepared: destination already exists; artifacts are published absent-only")
	// ErrNoHardLinks is returned when link(2) is refused with an error
	// that typically means the filesystem cannot create hard links (ENOTSUP,
	// EOPNOTSUPP, EPERM, EXDEV). The cause is not established: EPERM in
	// particular has other causes on some systems. Publication fails closed
	// either way; there is no overwriting fallback.
	ErrNoHardLinks = errors.New("prepared: hard link refused (the filesystem may not support hard links, which absent-only publication requires; no overwriting fallback exists)")
)

// PublishResult reports a publication.
type PublishResult struct {
	Path string
	// ForeignTemps lists temporary files found in the destination
	// directory that this invocation did not create: crash leftovers or a
	// concurrent publisher's live temporaries. They are never removed —
	// neither name nor age distinguishes a crashed writer from a live one.
	ForeignTemps []string
}

// PublishedError reports a failure after the final name was created: the
// artifact IS published and complete, but a completing step (removing this
// invocation's own temporary name, or making the directory entry durable)
// failed.
type PublishedError struct {
	Path string
	Err  error
}

func (e *PublishedError) Error() string {
	return fmt.Sprintf("prepared: %s is published, but %v", e.Path, e.Err)
}
func (e *PublishedError) Unwrap() error { return e.Err }

// testHook, when set, runs after each named publication step. Returning
// errSimulatedCrash stops Publish on the spot WITHOUT its cleanup, as a
// process crash would (tests only).
var testHook func(step string) error

var errSimulatedCrash = errors.New("prepared: simulated crash")

// linkFile is os.Link; a seam so tests can present a filesystem without
// hard links.
var linkFile = os.Link

// Publish creates dst with the bytes write produces, absent-only: it
// writes a temporary file in dst's directory, fsyncs and closes it, sets
// mode 0444, creates dst with os.Link (which fails if dst exists), removes
// the temporary name, and fsyncs the directory. It removes only the
// temporary file it created itself; others found in the directory are
// reported in ForeignTemps and left alone.
func Publish(dst string, write func(io.Writer) error) (res *PublishResult, err error) {
	dir := filepath.Dir(dst)
	res = &PublishResult{Path: dst}
	if _, err := os.Lstat(dst); err == nil {
		return res, fmt.Errorf("%w: %s", ErrExists, dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	res.ForeignTemps, err = findTemps(dir)
	if err != nil {
		return res, err
	}
	f, err := os.CreateTemp(dir, TempPrefix+"*"+TempSuffix)
	if err != nil {
		return res, err
	}
	own := f.Name()
	open := true
	crashed := false
	linked := false
	defer func() {
		if crashed {
			return // a crash runs no cleanup
		}
		if open {
			f.Close()
		}
		if !linked {
			os.Remove(own) // only ever our own temporary
		}
	}()
	step := func(name string) error {
		if testHook == nil {
			return nil
		}
		if err := testHook(name); err != nil {
			if errors.Is(err, errSimulatedCrash) {
				crashed = true
				if open {
					f.Close() // the kernel closes descriptors of a dead process
				}
			}
			return err
		}
		return nil
	}
	if err := step("created"); err != nil {
		return res, err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	if err := write(w); err != nil {
		return res, err
	}
	if err := w.Flush(); err != nil {
		return res, err
	}
	if err := step("written"); err != nil {
		return res, err
	}
	if err := f.Sync(); err != nil {
		return res, err
	}
	if err := step("synced"); err != nil {
		return res, err
	}
	// Close the only writable descriptor BEFORE the mode change: chmod does
	// not revoke a descriptor already open for writing.
	open = false
	if err := f.Close(); err != nil {
		return res, err
	}
	if err := step("closed"); err != nil {
		return res, err
	}
	if err := os.Chmod(own, 0o444); err != nil {
		return res, err
	}
	if err := step("chmod"); err != nil {
		return res, err
	}
	if err := linkFile(own, dst); err != nil {
		switch {
		case errors.Is(err, fs.ErrExist):
			return res, fmt.Errorf("%w: %s", ErrExists, dst)
		case noHardLinks(err):
			return res, fmt.Errorf("%w: %v", ErrNoHardLinks, err)
		}
		return res, fmt.Errorf("prepared: linking %s: %w", dst, err)
	}
	linked = true
	if err := step("linked"); err != nil {
		return res, err
	}
	if err := os.Remove(own); err != nil {
		return res, &PublishedError{Path: dst, Err: fmt.Errorf("removing temporary %s: %w", own, err)}
	}
	if err := step("removed"); err != nil {
		return res, err
	}
	if err := syncDir(dir); err != nil {
		return res, &PublishedError{Path: dst, Err: fmt.Errorf("syncing directory: %w", err)}
	}
	return res, nil
}

// PublishBytes publishes data at dst (see Publish).
func PublishBytes(dst string, data []byte) (*PublishResult, error) {
	return Publish(dst, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// findTemps lists Publish temporaries present in dir.
func findTemps(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, TempPrefix) && strings.HasSuffix(n, TempSuffix) && len(n) > len(TempPrefix)+len(TempSuffix) {
			out = append(out, filepath.Join(dir, n))
		}
	}
	sort.Strings(out)
	return out, nil
}
