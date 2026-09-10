// Package version gives a build an identity the mesh can compare.
//
// A node announces what it is running so peers can tell a fleet that agrees
// from one that does not, and so a node that was offline can notice on
// rejoining that the mesh has moved on. That comparison is the whole reason
// this package exists, and it is deliberately conservative: the cost of
// reading a version as newer than it is gets paid by every machine at once.
package version

import (
	"cmp"
	"strconv"
	"strings"
)

// kind says whether a build names a published release.
//
// Unexported: IsRelease is the whole question a caller has. It is a named type
// rather than a bool field because the zero value has to be the unreleased one
// -- a version nobody set must not be able to outrank a real release -- and a
// name says that where `false` would not.
//
// Not derivable from the triple: v0.0.0 is a legitimate release whose numbers
// are all zero, so the discriminator has to be carried separately.
type kind uint8

const (
	// kindUnreleased names no published release.
	kindUnreleased kind = iota
	// kindRelease is exactly vMAJOR.MINOR.PATCH.
	kindRelease
)

// Order is how one version sits relative to another.
//
// OrderUnknown is the zero value because two builds that name no release say
// nothing about each other: "dev" and "v0.2.0-5-gabc1234" are not ranked, they
// are simply both unpublished.
type Order uint8

const (
	// OrderUnknown means neither side names a release, so there is no answer.
	OrderUnknown Order = iota
	// OrderSame means both name the same release.
	OrderSame
	// OrderNewer means the receiver names a later release than the argument.
	OrderNewer
	// OrderOlder means the receiver names an earlier release than the argument.
	OrderOlder
)

// Version is a build's identity.
//
// The raw text is kept whatever it says, because what a build calls itself is
// worth reporting even when it is not a release -- "dev" in `status` is the
// answer to why a node is not converging.
//
// Compare with Compare, never with ==. Two spellings of one release do not
// compare equal as structs -- Parse("v1.2.3") and Parse("1.2.3") differ in
// raw -- so using a Version as a map key, or deduplicating a slice of them
// with ==, splits a release in two and reports a fleet as disagreeing when it
// agrees. Versions arrive from peers, where both spellings occur.
type Version struct {
	raw   string
	kind  kind
	major int
	minor int
	patch int
}

// Parse reads a version string. It cannot fail.
//
// A version arrives from a peer, which is to say from a machine this one does
// not control, so unparseable input is an ordinary value and not an error: it
// parses to a version that names no release, which is exactly the "cannot
// converge on this" the caller needs. Returning an error here would push that
// judgement onto every call site, and one of them would get it wrong.
//
// Only vMAJOR.MINOR.PATCH is a release, with the leading v optional. Anything
// carrying a suffix is not, and that includes `git describe`'s own forms:
// v0.2.0-5-gabc1234 is *ahead* of v0.2.0 while v0.2.0-rc1 is *behind* it, and
// nothing in the text distinguishes them. Ranking either one against v0.2.0
// would be a guess, and a wrong guess installs the wrong binary everywhere.
func Parse(s string) Version {
	// Named rather than left implicit: unreleased is the answer for
	// everything this function does not recognise, which is most of what it
	// will ever be handed.
	v := Version{raw: strings.TrimSpace(s), kind: kindUnreleased}

	digits := strings.TrimPrefix(v.raw, "v")
	major, minor, patch, ok := splitTriple(digits)
	if !ok {
		return v
	}
	v.kind = kindRelease
	v.major, v.minor, v.patch = major, minor, patch
	return v
}

// splitTriple reads exactly three dot-separated non-negative integers.
func splitTriple(s string) (major, minor, patch int, ok bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var out [3]int
	for i, part := range parts {
		// ParseUint rather than Atoi: Atoi accepts a leading sign, and
		// "1.-0.0" is not a release. 31 bits keeps the value an int on every
		// platform and rejects a component too long to mean anything.
		n, err := strconv.ParseUint(part, 10, 31)
		if err != nil {
			return 0, 0, 0, false
		}
		out[i] = int(n)
	}
	return out[0], out[1], out[2], true
}

// String returns the version as the build reported it.
func (v Version) String() string { return v.raw }

// IsRelease reports whether this version names a published release.
func (v Version) IsRelease() bool { return v.kind == kindRelease }

// Compare ranks v against other.
//
// A release outranks anything unreleased, so a node running an unstamped
// build reads as behind the fleet -- true, and the honest thing to show. The
// reverse never holds: an unreleased build cannot outrank a release, which is
// what stops the machine somebody is developing on from dragging every other
// node onto whatever is in its working tree.
func (v Version) Compare(other Version) Order {
	switch {
	case v.kind == kindRelease && other.kind != kindRelease:
		return OrderNewer
	case v.kind != kindRelease && other.kind == kindRelease:
		return OrderOlder
	case v.kind != kindRelease:
		return OrderUnknown
	}

	switch c := cmp.Or(
		cmp.Compare(v.major, other.major),
		cmp.Compare(v.minor, other.minor),
		cmp.Compare(v.patch, other.patch),
	); {
	case c > 0:
		return OrderNewer
	case c < 0:
		return OrderOlder
	default:
		return OrderSame
	}
}
