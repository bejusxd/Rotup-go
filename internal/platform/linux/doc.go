// Package linux holds the Linux removable-media adapters: block-device
// enumeration, the authoritative mount table, and the mount lifecycle that runs
// through the privileged helper.
//
// Every implementation file in this package carries a linux build tag, so on
// other platforms the package is empty and the composition root selects the
// unsupported-platform adapters instead. That keeps runtime GOOS branching out
// of the domain and application packages entirely.
package linux
