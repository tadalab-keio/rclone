//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || (openbsd && cgo) || windows)

package cmount

// Read only directories and hidden dot files.
//
// --read-only-dir: below these directories (paths from the root of the
// mount) nothing may be changed through the mount. Opening a file to write,
// making, removing or renaming a file or directory and setting a time fail at
// once with "access denied", and files there are shown read only (on
// Windows, the read only attribute). A remote which refuses writes there
// anyway (permission denied) would otherwise see them only when the file is
// uploaded after it is closed, with --vfs-cache-mode writes: the program
// which saved it is told it was saved, and the upload is retried.
//
// --windows-hide-dot-files: files and directories whose names begin with a
// dot are shown with the hidden attribute, as a Unix listing hides them.
//
// Both are given to the FUSE layer as BSD flags (UF_READONLY, UF_HIDDEN),
// which WinFsp turns into Windows file attributes (cgofuse fuse_stat_ex).

import (
	"path"
	"strings"

	"github.com/rclone/rclone/vfs"
	"github.com/winfsp/cgofuse/fuse"
)

// cleanMountPath turns a path of the mount ("/a/b", "a/b/") into "a/b" ("" for the root)
func cleanMountPath(p string) string {
	p = strings.Trim(path.Clean("/"+strings.ReplaceAll(p, `\`, "/")), "/")
	if p == "." {
		return ""
	}
	return p
}

// readOnlyDirs returns the cleaned --read-only-dir paths
func (fsys *FS) readOnlyDirs() []string {
	fsys.roOnce.Do(func() {
		for _, d := range fsys.opt.ReadOnlyDirs {
			if d = cleanMountPath(d); d != "" {
				fsys.roDirs = append(fsys.roDirs, d)
			}
		}
	})
	return fsys.roDirs
}

// readOnly reports whether p, a path of the mount, is a --read-only-dir or below one
func (fsys *FS) readOnly(p string) bool {
	dirs := fsys.readOnlyDirs()
	if len(dirs) == 0 {
		return false
	}
	p = cleanMountPath(p)
	fold := fsys.VFS.Opt.CaseInsensitive
	for _, d := range dirs {
		if len(p) < len(d) || (len(p) > len(d) && p[len(d)] != '/') {
			continue
		}
		if p[:len(d)] == d || (fold && strings.EqualFold(p[:len(d)], d)) {
			return true
		}
	}
	return false
}

// refuseChange returns -EACCES if any of paths is read only, else 0
func (fsys *FS) refuseChange(paths ...string) int {
	for _, p := range paths {
		if fsys.readOnly(p) {
			return -fuse.EACCES
		}
	}
	return 0
}

// writeFlags reports whether FUSE open flags would change the file
func writeFlags(flags int) bool {
	return flags&fuse.O_ACCMODE != fuse.O_RDONLY || flags&(fuse.O_APPEND|fuse.O_CREAT|fuse.O_TRUNC) != 0
}

// statFlags returns the BSD flags (Windows attributes) of node
func (fsys *FS) statFlags(node vfs.Node) (flags uint32) {
	if fsys.opt.WindowsHideDotFiles && strings.HasPrefix(node.Name(), ".") {
		flags |= fuse.UF_HIDDEN
	}
	if node.IsFile() && fsys.readOnly(node.Path()) {
		flags |= fuse.UF_READONLY
	}
	return flags
}
