//go:build unix

package source

import (
	"io/fs"
	"syscall"
)

// asUint64 widens a platform-dependent stat field.
//
// The width of Stat_t.Dev differs by platform -- int32 on darwin, uint64 on
// linux -- so a direct conversion is necessary on one and redundant on the
// other. Routing through a generic keeps the code identical everywhere and the
// conversion genuinely needed in both builds.
func asUint64[T ~int32 | ~uint32 | ~int64 | ~uint64](v T) uint64 { return uint64(v) }

// identityOf extracts the device and inode, which together identify a file
// independently of its path.
func identityOf(fi fs.FileInfo) FileIdentity {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}
	}
	return FileIdentity{Device: asUint64(st.Dev), Serial: asUint64(st.Ino)}
}
