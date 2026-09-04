//go:build !unix

package system

import "io/fs"

// ownerOf has no answer off unix. Inspection treats an unknown owner as
// "cannot verify" and says so, rather than claiming the file is correctly
// owned. Provisioning is refused on these platforms before it reaches here.
func ownerOf(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }

func chownPath(string, int, int) error { return ErrNotSupported }
