//go:build windows

package agentcfg

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictToOwner replaces the file's permissions with one entry: this user.
//
// Windows ignores the Unix mode bits passed to OpenFile, so the 0600 that
// protects this file everywhere else does nothing here and the file inherits
// whatever the parent directory grants -- which under a roaming profile or a
// redirected AppData can be a good deal more than one person. The file holds the
// mesh key, and that key admits a machine to the mesh, decrypts every record in
// the archive and unlocks the dashboard, so "probably only me" is not good
// enough.
//
// The DACL is set protected, which drops inherited entries rather than adding
// to them. Without that flag the grant below would be an addition to whatever
// was inherited, which is the opposite of restricting.
func restrictToOwner(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("agentcfg: identify the current user: %w", err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("agentcfg: build an access list: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("agentcfg: restrict access to %s: %w", path, err)
	}
	return nil
}
