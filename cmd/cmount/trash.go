//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || (openbsd && cgo) || windows)

package cmount

// The Windows Recycle Bin on a mount.
//
// Explorer recycles an item by renaming it to
// \$RECYCLE.BIN\<SID>\$R<id><ext>, next to a \$RECYCLE.BIN\<SID>\$I<id><ext>
// it writes first, which holds the original path and the deletion time.
//
// On Windows the \$RECYCLE.BIN tree is kept in a local directory on the PC
// and never reaches the remote. The rename is caught: with --windows-trash
// (the default) the item is moved into a freedesktop trash on the remote
// (https://specifications.freedesktop.org/trash-spec/) with an
// info/<name>.trashinfo, and without it the item is deleted. Either way the
// $I file is removed, so the Recycle Bin of the drive stays empty.
//
// Which trash: the trash of the owner of the directory holding the item,
// when the backend can tell (sftp: TrashDir, <home of that owner>/.local/
// share/Trash); else, for an item below /home/<user>/,
// /home/<user>/.local/share/Trash; else --windows-trash-other (default
// .local/share/Trash, relative to the login directory of the remote, i.e.
// the trash of the user the remote logs in as). The item is renamed within
// the VFS when the trash is inside the remote root; otherwise it is copied
// through the VFS (so data not yet uploaded is included) and then deleted,
// as it may be on another file system.

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/winfsp/cgofuse/fuse"
)

const recycleBinName = "$RECYCLE.BIN"

// the target of the rename by which Explorer recycles an item
var recycleItemRegex = regexp.MustCompile(`(?i)^/\$RECYCLE\.BIN/[^/]+/\$R[^/]+$`)

// newRecycleVFS makes the VFS holding the \$RECYCLE.BIN tree of the mount
// in a local directory, on Windows only.
func newRecycleVFS(f fs.Fs) *vfs.VFS {
	if runtime.GOOS != "windows" {
		return nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		fs.Errorf(nil, "Recycle Bin: no local cache directory, keeping %s on the remote: %v", recycleBinName, err)
		return nil
	}
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '_'
	}, fs.ConfigString(f))
	dir := filepath.Join(base, "rclone", "recycle", name)
	if err := os.MkdirAll(filepath.Join(dir, recycleBinName), 0700); err != nil {
		fs.Errorf(nil, "Recycle Bin: can't make %s, keeping %s on the remote: %v", dir, recycleBinName, err)
		return nil
	}
	lf, err := fs.NewFs(context.Background(), dir)
	if err != nil {
		fs.Errorf(nil, "Recycle Bin: can't open %s, keeping %s on the remote: %v", dir, recycleBinName, err)
		return nil
	}
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeWrites // Windows rewrites desktop.ini in place
	fs.Debugf(nil, "Recycle Bin: %s kept in %s", recycleBinName, dir)
	return vfs.New(context.Background(), lf, &opt)
}

// inRecycleBin reports whether p is \$RECYCLE.BIN or below it
func (fsys *FS) inRecycleBin(p string) bool {
	if fsys.recycle == nil {
		return false
	}
	first := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 2)[0]
	return strings.EqualFold(first, recycleBinName)
}

// vfsFor returns the VFS which holds p
func (fsys *FS) vfsFor(p string) *vfs.VFS {
	if fsys.inRecycleBin(p) {
		return fsys.recycle
	}
	return fsys.VFS
}

// recycleRename handles a rename from the remote into the Recycle Bin
func (fsys *FS) recycleRename(oldPath, newPath string) (errc int) {
	if !recycleItemRegex.MatchString(newPath) {
		return -fuse.EXDEV
	}
	defer fsys.dropRecycleInfo(newPath)
	node, err := fsys.VFS.Stat(oldPath)
	if err != nil {
		return translateError(err)
	}
	if !fsys.opt.WindowsTrash {
		fs.Infof(oldPath, "Recycle Bin: deleting (--windows-trash is off)")
		if node.IsDir() {
			return translateError(node.RemoveAll())
		}
		return translateError(node.Remove())
	}
	if err = fsys.moveToTrash(oldPath, node); err != nil {
		fs.Errorf(oldPath, "Recycle Bin: not moved to the trash: %v", err)
		return -fuse.EPERM
	}
	return 0
}

// trashInfo returns the content of a .trashinfo file for abs
func trashInfo(abs string) string {
	return "[Trash Info]\nPath=" + (&url.URL{Path: abs}).EscapedPath() +
		"\nDeletionDate=" + time.Now().Format("2006-01-02T15:04:05") + "\n"
}

// trashName returns name, or name.N.ext, not yet used by exists
func trashName(base string, exists func(name string) bool) string {
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	name := base
	for n := 2; exists(name); n++ {
		name = stem + "." + strconv.Itoa(n) + ext
	}
	return name
}

// trashOwner is a backend which knows the trash of a directory's owner
type trashOwner interface {
	TrashDir(ctx context.Context, remote string) (string, error)
}

// trashFor returns the trash for an item at oldPath (abs on the remote), or
// "" for --windows-trash-other
func (fsys *FS) trashFor(oldPath, abs string) string {
	if t, ok := fsys.f.(trashOwner); ok {
		trash, err := t.TrashDir(context.Background(), oldPath)
		if err == nil {
			return trash
		}
		fs.Debugf(oldPath, "Recycle Bin: the owner's trash is not known: %v", err)
	}
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	if len(parts) >= 3 && parts[0] == "home" {
		return "/home/" + parts[1] + "/.local/share/Trash"
	}
	return ""
}

// moveToTrash moves oldPath into the trash it belongs to
func (fsys *FS) moveToTrash(oldPath string, node vfs.Node) error {
	if !strings.HasPrefix(fsys.f.Root(), "/") {
		return fsys.copyToTrash(oldPath, node, oldPath, fsys.opt.WindowsTrashOther)
	}
	root := "/" + strings.Trim(fsys.f.Root(), "/")
	abs := path.Join(root, oldPath)
	trash := fsys.trashFor(oldPath, abs)
	if trash == "" {
		return fsys.copyToTrash(oldPath, node, abs, fsys.opt.WindowsTrashOther)
	}
	if abs == trash || strings.HasPrefix(abs, trash+"/") {
		return fmt.Errorf("%s is in the trash already", abs)
	}
	if root != "/" && !strings.HasPrefix(trash+"/", root+"/") {
		return fsys.copyToTrash(oldPath, node, abs, trash)
	}
	rel := strings.TrimPrefix(trash, root)
	if err := fsys.VFS.MkdirAll(rel+"/files", 0700); err != nil {
		return err
	}
	if err := fsys.VFS.MkdirAll(rel+"/info", 0700); err != nil {
		return err
	}
	name := trashName(path.Base(oldPath), func(name string) bool {
		_, errFile := fsys.VFS.Stat(rel + "/files/" + name)
		_, errInfo := fsys.VFS.Stat(rel + "/info/" + name + ".trashinfo")
		return errFile != vfs.ENOENT || errInfo != vfs.ENOENT
	})
	info := rel + "/info/" + name + ".trashinfo"
	if err := fsys.VFS.WriteFile(info, []byte(trashInfo(abs)), 0600); err != nil {
		return err
	}
	if err := fsys.VFS.Rename(oldPath, rel+"/files/"+name); err != nil {
		_ = fsys.VFS.Remove(info)
		return err
	}
	fs.Infof(oldPath, "Recycle Bin: moved to %s/files/%s", trash, name)
	return nil
}

// copyToTrash copies oldPath into trash and deletes it
func (fsys *FS) copyToTrash(oldPath string, node vfs.Node, abs, trash string) error {
	ctx := context.Background()
	if trash == "" {
		return fmt.Errorf("no trash known for %s and --windows-trash-other is empty", abs)
	}
	dst, err := fs.NewFs(ctx, fsys.f.Name()+":"+trash)
	if err != nil && err != fs.ErrorIsFile {
		return err
	}
	for _, d := range []string{"files", "info"} {
		if err := dst.Mkdir(ctx, d); err != nil {
			return err
		}
	}
	used := map[string]bool{}
	for _, d := range []string{"files", "info"} {
		entries, err := dst.List(ctx, d)
		if err != nil {
			return err
		}
		for _, e := range entries {
			used[d+"/"+path.Base(e.Remote())] = true
		}
	}
	name := trashName(path.Base(oldPath), func(name string) bool {
		return used["files/"+name] || used["info/"+name+".trashinfo"]
	})
	if _, err := operations.Rcat(ctx, dst, "info/"+name+".trashinfo", io.NopCloser(strings.NewReader(trashInfo(abs))), time.Now(), nil); err != nil {
		return err
	}
	if err := fsys.copyNode(ctx, dst, "files/"+name, node); err != nil {
		return err
	}
	if node.IsDir() {
		err = node.RemoveAll()
	} else {
		err = node.Remove()
	}
	if err != nil {
		return err
	}
	fs.Infof(oldPath, "Recycle Bin: copied to %s/files/%s and deleted", trash, name)
	return nil
}

// copyNode copies node (a file, or a directory with all below it) through
// the VFS to remote in dst
func (fsys *FS) copyNode(ctx context.Context, dst fs.Fs, remote string, node vfs.Node) error {
	if dir, ok := node.(*vfs.Dir); ok {
		if err := dst.Mkdir(ctx, remote); err != nil {
			return err
		}
		nodes, err := dir.ReadDirAll()
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if err := fsys.copyNode(ctx, dst, remote+"/"+n.Name(), n); err != nil {
				return err
			}
		}
		return nil
	}
	h, err := fsys.VFS.OpenFile(node.Path(), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	_, err = operations.Rcat(ctx, dst, remote, h, node.ModTime(), nil)
	return err
}

// dropRecycleInfo removes the $I file Windows wrote for the $R name
func (fsys *FS) dropRecycleInfo(newPath string) {
	dir, base := path.Split(newPath)
	if len(base) < 2 || !strings.EqualFold(base[:2], "$R") {
		return
	}
	if err := fsys.recycle.Remove(dir + "$I" + base[2:]); err != nil && err != vfs.ENOENT {
		fs.Debugf(newPath, "Recycle Bin: $I file not removed: %v", err)
	}
}
