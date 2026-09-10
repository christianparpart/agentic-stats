//go:build windows

package beacon

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// reuseControl lets several sockets share the beacon port.
//
// Windows needs only SO_REUSEADDR for this, and means something more permissive
// by it than the BSD stacks do: it allows a genuine second bind to the same
// address and port rather than merely reusing a lingering one, which is what
// SO_REUSEPORT is for elsewhere. There is no SO_REUSEPORT to pair it with.
//
// Without this, only the first listener on a host binds and the rest fail with
// "only one usage of each socket address is normally permitted". That is not an
// exotic case here: a Windows machine running a Windows or Linux VM is an
// ordinary arrangement in this fleet, and the failure is silent from the
// outside -- the node simply never discovers anyone.
func reuseControl(_, _ string, c syscall.RawConn) error {
	var setErr error
	err := c.Control(func(fd uintptr) {
		setErr = windows.SetsockoptInt(
			windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return setErr
}
