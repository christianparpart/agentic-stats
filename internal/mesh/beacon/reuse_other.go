//go:build !unix && !windows

package beacon

import "syscall"

// reuseControl is a no-op on platforms with neither option. A single node per
// host still works; two would collide on the bind.
func reuseControl(_, _ string, _ syscall.RawConn) error { return nil }
