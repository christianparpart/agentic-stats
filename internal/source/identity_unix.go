//go:build unix

package source

import (
	"io/fs"
	"syscall"
)

// identityOf extracts the device and inode, which together identify a file
// independently of its path.
func identityOf(fi fs.FileInfo) FileIdentity {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}
	}
	return FileIdentity{Device: uint64(st.Dev), Serial: st.Ino}
}
