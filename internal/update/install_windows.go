//go:build windows

package update

import (
	"fmt"
	"os"
)

// staleSuffix marks an image that a replacement moved aside.
const staleSuffix = ".old"

// replace swaps dst for the staged file.
//
// Windows will not let a running image be overwritten or deleted, but it will
// let one be renamed -- which is exactly what the Go linker does when it builds
// over a binary that is running, leaving a "~" file beside it. So the current
// image is moved aside, the new one takes its place, and the old one is
// deleted if nothing holds it. When something does -- which is the normal case,
// because this process is running from it -- it is swept at the next start.
//
// The rename is what makes this safe: at no point is there no file at dst,
// except for the instant between the two renames, and a failure there puts the
// original back rather than leaving the node with no binary at all.
func replace(dst, staged string) error {
	tmp, err := stageBeside(dst, staged)
	if err != nil {
		return err
	}

	aside := dst + staleSuffix
	// A leftover from a previous update, if whatever held it has since exited.
	_ = os.Remove(aside)

	if err := os.Rename(dst, aside); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return fmt.Errorf("move the running image aside: %w", err)
	}

	if err := os.Rename(tmp, dst); err != nil {
		if rerr := os.Rename(aside, dst); rerr != nil {
			// Both failed, which is the one case that leaves the node
			// without a usable binary. Say so loudly enough to act on.
			return fmt.Errorf("%w; the old image is at %s and must be moved back by hand: %w",
				err, aside, rerr)
		}
		_ = os.Remove(tmp)
		return err
	}

	// Best effort: still mapped whenever this process is running from it.
	_ = os.Remove(aside)
	return nil
}

// stalePath is where replace leaves the image it could not delete.
func stalePath(dst string) string { return dst + staleSuffix }
