//go:build !windows

package update

import "os"

// replace swaps dst for the staged file.
//
// A running program holds its executable by inode here, so renaming a new file
// over the path leaves the running process entirely untouched and the next
// start picks up the new build. Nothing has to be moved aside, and there is no
// window in which the path does not exist.
func replace(dst, staged string) error {
	tmp, err := stageBeside(dst, staged)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// stalePath is empty here: a replacement leaves nothing behind to sweep.
func stalePath(string) string { return "" }
