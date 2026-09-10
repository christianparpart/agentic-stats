//go:build windows

package agentcfg_test

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// assertOwnerOnly checks that nobody but the current user is granted access.
//
// The Unix mode bits are meaningless here -- os.Stat reports 0666 for any file
// that is not read-only, whatever its access list says -- so this walks the DACL
// that actually governs the file. Asserting the mode instead would pass while
// the mesh key sat readable by everyone who can reach the profile, which is the
// bug this replaced.
func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the security descriptor: %v", err)
	}
	dacl, defaulted, err := sd.DACL()
	if err != nil {
		t.Fatalf("read the access list: %v", err)
	}
	if dacl == nil {
		t.Fatal("the file has no access list, so everyone can reach the mesh key")
	}
	if defaulted {
		t.Error("the access list was inherited rather than set for this file")
	}
	if dacl.AceCount == 0 {
		t.Fatal("the access list is empty, which cannot be right")
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("identify the current user: %v", err)
	}
	me := user.User.Sid.String()

	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatalf("read access entry %d: %v", i, err)
		}
		// The SID is laid out inline after the fixed header, which is why the
		// field is named SidStart rather than being a pointer.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if got := sid.String(); got != me {
			t.Errorf("%s is granted access to the mesh key; only the owner should be", got)
		}
	}
}
