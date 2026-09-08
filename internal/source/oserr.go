package source

import (
	"errors"
	"io/fs"
)

// isNotExist reports whether err indicates a missing file, through any of the
// wrapping layers a FileSystem implementation may add.
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
