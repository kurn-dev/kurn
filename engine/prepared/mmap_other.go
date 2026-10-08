//go:build !unix

package prepared

import (
	"errors"
	"os"
)

// mmapSupported is false: OpenMapped refuses with errors.ErrUnsupported
// before touching the file.
const mmapSupported = false

func mapFile(*os.File, int) ([]byte, error) { return nil, errors.ErrUnsupported }

func unmapFile([]byte) error { return errors.ErrUnsupported }

func fileKey(os.FileInfo) (dev, ino uint64, err error) { return 0, 0, errors.ErrUnsupported }
