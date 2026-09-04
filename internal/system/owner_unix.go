//go:build unix

package system

import (
	"io/fs"
	"os"
	"syscall"
)

// ownerOf extracts the numeric owner from a stat result. Ownership is the whole
// point of the helper and sudoers checks, so it is read directly rather than
// inferred from anything portable.
func ownerOf(info fs.FileInfo) (uid, gid int, ok bool) {
	stat, valid := info.Sys().(*syscall.Stat_t)
	if !valid {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}

func chownPath(path string, uid, gid int) error { return os.Chown(path, uid, gid) }
