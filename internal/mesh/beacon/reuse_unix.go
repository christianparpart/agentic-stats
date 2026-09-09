//go:build unix

package beacon

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reuseControl lets several sockets share the beacon port.
//
// Without this only the first listener on a host binds and the rest fail with
// "address already in use". That is not an exotic case: a machine and a VM
// running on it, or two nodes during testing, are ordinary arrangements, and
// the failure is silent from the outside -- the node simply never discovers
// anyone.
func reuseControl(_, _ string, c syscall.RawConn) error {
	var setErr error
	err := c.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			setErr = err
			return
		}
		// SO_REUSEPORT is what actually permits two bound sockets to both
		// receive the multicast group on BSD-derived stacks, macOS included.
		setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if err != nil {
		return err
	}
	return setErr
}
