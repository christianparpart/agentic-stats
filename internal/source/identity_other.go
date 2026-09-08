//go:build !unix

package source

import "io/fs"

// identityOf returns a zero identity on platforms where one is not readily
// available. Replacement detection then falls back to content fingerprinting,
// which FileStream handles.
func identityOf(fs.FileInfo) FileIdentity { return FileIdentity{} }
