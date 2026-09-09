//go:build !unix

package beacon

import "syscall"

// reuseControl is a no-op where the socket options are unavailable. A single
// node per host still works; two would collide on the bind.
func reuseControl(_, _ string, _ syscall.RawConn) error { return nil }
