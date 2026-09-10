//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// attachConsole reattaches this process to the terminal that launched it.
//
// The Windows binary is linked for the GUI subsystem, because a console-
// subsystem program started by Task Scheduler is given a console window and
// there is no flag anywhere that suppresses it -- the window is a property of
// the subsystem, not of how the process was started. Linking for the GUI
// subsystem is the only thing that removes it, which is what makes the service
// run unseen.
//
// The cost is that the same binary is also the command-line tool, and a GUI
// program starts with no standard handles at all, so `status` would print into
// nothing. Attaching to the parent's console gives them back.
//
// One consequence cannot be undone here and is worth knowing: a shell does not
// wait for a GUI-subsystem program, so a command's output arrives after the
// prompt has already returned. That is inherent to the subsystem and is the
// price of the service running without a window; the alternative is shipping
// two binaries.
//
// Called before any output. Failure is not an error: no parent console means
// this is the service, which is the case the subsystem choice exists for.
// attachParentProcess is ATTACH_PARENT_PROCESS: join the console of whatever
// launched us, if it has one.
const attachParentProcess = ^uintptr(0)

// AttachConsole is not among the calls x/sys/windows exports, so it is bound
// here. kernel32 is already loaded in every process.
var procAttachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

func attachConsole() {
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		// No parent console. This is the service, which is the case the GUI
		// subsystem exists for.
		return
	}
	// AttachConsole gives the process a console but leaves Go's os.Stdout and
	// os.Stderr pointing at the handles it started with, which for a GUI
	// program are invalid. Rebind them to the console we just joined.
	reopen(windows.STD_OUTPUT_HANDLE, &os.Stdout, "CONOUT$")
	reopen(windows.STD_ERROR_HANDLE, &os.Stderr, "CONOUT$")
	reopen(windows.STD_INPUT_HANDLE, &os.Stdin, "CONIN$")
}

// reopen points a standard stream at the attached console.
//
// Only when the existing handle is unusable: a redirected stream -- a pipe, or
// output sent to a file -- is a handle the process was given deliberately, and
// replacing it with the console would send the output somewhere the caller did
// not ask for.
func reopen(which uint32, stream **os.File, device string) {
	if h, err := windows.GetStdHandle(which); err == nil && h != 0 {
		if _, ferr := (*stream).Stat(); ferr == nil {
			return // already usable, and possibly redirected
		}
	}
	name, err := windows.UTF16PtrFromString(device)
	if err != nil {
		return
	}
	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE)
	h, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return
	}
	if err := windows.SetStdHandle(which, h); err != nil {
		_ = windows.CloseHandle(h)
		return
	}
	*stream = os.NewFile(uintptr(h), device)
}

// hasConsole reports whether output would reach a terminal.
//
// The GUI-subsystem binary has no console when the service starts it, and a
// real one when a person runs it. That difference decides whether the log also
// goes to the screen and to Event Viewer.
func hasConsole() bool {
	h, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	return err == nil && h != 0
}
