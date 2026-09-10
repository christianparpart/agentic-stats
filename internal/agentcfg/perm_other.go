//go:build !windows

package agentcfg

// restrictToOwner is a no-op where the mode bits already did the work.
//
// Save creates the file 0600, which the kernel honours here, so there is
// nothing left to tighten.
func restrictToOwner(string) error { return nil }
