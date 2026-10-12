package sftp

// Owner and permission bits of what rclone makes or replaces on the server,
// and the trash of a directory's owner.
//
// inherit_owner: a new file or directory gets the owner and group of the
// directory it is made in, like Samba's "inherit owner". A server reached as
// one user (often root, through server_command) then leaves files owned as
// if the owner of that directory had made them, and a path through a
// symbolic link takes the owner of the real directory. Changing the owner
// needs a server allowed to (root); otherwise the owner stays and this is
// logged at debug level.
//
// new_file_mode / new_dir_mode: the permission bits given to a new file or
// directory (octal), instead of what the server makes from the mode asked
// for and its umask.
//
// replace_keep_attrs: when a file is moved over an existing file, the moved
// file first gets the owner, group and permission bits of the file it
// replaces. Uploads end this way since v1.63 (the .partial file is moved
// over the old one, see --inplace), and so do editors saving through a
// temporary file. Writing into the file kept these; replacing it made them
// those of a new file. A move over an existing object through
// operations.Move (a rename on a mount, moveto) deletes the old object
// first, so a removed file's owner and mode are kept for keepRemovedFor and
// given to a file moved to the same path within that time.

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"github.com/rclone/rclone/fs"
)

// modeBits keeps what chmod sets: permissions, setuid, setgid, sticky
const modeBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// parseMode reads an octal mode option; ok is false when it is empty
func parseMode(s string) (mode os.FileMode, ok bool, err error) {
	if s == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n > 07777 {
		return 0, false, fmt.Errorf("not an octal mode up to 7777: %q", s)
	}
	m := os.FileMode(n & 0777)
	if n&04000 != 0 {
		m |= os.ModeSetuid
	}
	if n&02000 != 0 {
		m |= os.ModeSetgid
	}
	if n&01000 != 0 {
		m |= os.ModeSticky
	}
	return m, true, nil
}

// checkAttrOptions checks the mode options when the Fs is made
func checkAttrOptions(opt *Options) error {
	if _, _, err := parseMode(opt.NewFileMode); err != nil {
		return fmt.Errorf("new_file_mode: %w", err)
	}
	if _, _, err := parseMode(opt.NewDirMode); err != nil {
		return fmt.Errorf("new_dir_mode: %w", err)
	}
	return nil
}

// wantsNewAttrs reports whether a new file or directory gets anything set
func (f *Fs) wantsNewAttrs(isDir bool) bool {
	if f.opt.InheritOwner {
		return true
	}
	if isDir {
		return f.opt.NewDirMode != ""
	}
	return f.opt.NewFileMode != ""
}

// owner returns the owner and group in a stat result
func owner(fi os.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*sftp.FileStat)
	if !ok {
		return 0, 0, false
	}
	return int(st.UID), int(st.GID), true
}

// applyNew sets the owner and the permission bits of p, a file or
// directory just made on the server
func (f *Fs) applyNew(ctx context.Context, p string, isDir bool) {
	if !f.wantsNewAttrs(isDir) {
		return
	}
	modeOpt := f.opt.NewFileMode
	if isDir {
		modeOpt = f.opt.NewDirMode
	}
	mode, setMode, _ := parseMode(modeOpt)
	c, err := f.getSftpConnection(ctx)
	if err != nil {
		fs.Debugf(f, "owner and mode of %q not set: %v", p, err)
		return
	}
	defer f.putSftpConnection(&c, nil)
	if f.opt.InheritOwner {
		fi, err := c.sftpClient.Stat(path.Dir(p))
		if err != nil {
			fs.Debugf(f, "owner of %q not set: parent: %v", p, err)
		} else if uid, gid, ok := owner(fi); ok {
			if err := c.sftpClient.Chown(p, uid, gid); err != nil {
				fs.Debugf(f, "owner of %q not set to %d:%d: %v", p, uid, gid, err)
			}
		}
	}
	// after chown, which may clear setuid and setgid
	if setMode {
		if err := c.sftpClient.Chmod(p, mode); err != nil {
			fs.Debugf(f, "mode of %q not set to %v: %v", p, mode, err)
		}
	}
}

// keepRemovedFor is how long a removed file's owner and mode wait for a
// file moved to its path (operations.Move deletes, then moves at once)
const keepRemovedFor = 10 * time.Second

// fileAttrs are the owner, group and mode of a removed file
type fileAttrs struct {
	uid, gid int
	mode     os.FileMode
	at       time.Time
}

// rememberRemoved keeps the owner and mode of p, a file about to be removed
func (f *Fs) rememberRemoved(c *conn, p string) {
	if !f.opt.ReplaceKeepAttrs {
		return
	}
	fi, err := c.sftpClient.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	uid, gid, ok := owner(fi)
	if !ok {
		return
	}
	f.ownerMu.Lock()
	defer f.ownerMu.Unlock()
	if f.removed == nil {
		f.removed = map[string]fileAttrs{}
	}
	for k, a := range f.removed {
		if time.Since(a.at) > keepRemovedFor {
			delete(f.removed, k)
		}
	}
	f.removed[p] = fileAttrs{uid: uid, gid: gid, mode: fi.Mode() & modeBits, at: time.Now()}
}

// takeRemoved returns the owner and mode of a file removed at p just before
func (f *Fs) takeRemoved(p string) (fileAttrs, bool) {
	f.ownerMu.Lock()
	defer f.ownerMu.Unlock()
	a, ok := f.removed[p]
	delete(f.removed, p)
	return a, ok && time.Since(a.at) <= keepRemovedFor
}

// keepReplacedAttrs gives src the owner, group and permission bits of dst
// when dst is an existing file which src is about to replace, or a file
// removed at dst just before
func (f *Fs) keepReplacedAttrs(c *conn, src, dst string) {
	if !f.opt.ReplaceKeepAttrs {
		return
	}
	var a fileAttrs
	fi, err := c.sftpClient.Lstat(dst)
	switch {
	case err == nil:
		if !fi.Mode().IsRegular() {
			return
		}
		uid, gid, ok := owner(fi)
		if !ok {
			return
		}
		a = fileAttrs{uid: uid, gid: gid, mode: fi.Mode() & modeBits}
	case errors.Is(err, iofs.ErrNotExist):
		var ok bool
		if a, ok = f.takeRemoved(dst); !ok {
			return
		}
	default:
		fs.Debugf(f, "replace %q: not stat'd: %v", dst, err)
		return
	}
	if err := c.sftpClient.Chown(src, a.uid, a.gid); err != nil {
		fs.Debugf(f, "replace %q: owner %d:%d not kept: %v", dst, a.uid, a.gid, err)
	}
	if err := c.sftpClient.Chmod(src, a.mode); err != nil {
		fs.Debugf(f, "replace %q: mode %v not kept: %v", dst, a.mode, err)
	}
}

// TrashDir returns the freedesktop home trash
// (https://specifications.freedesktop.org/trash-spec/) of the owner of the
// directory holding remote: <home>/.local/share/Trash, the home read with
// getent passwd on the server. It needs shell access.
func (f *Fs) TrashDir(ctx context.Context, remote string) (string, error) {
	if f.shellType == shellTypeNotSupported || f.shellType == "" {
		return "", errors.New("no shell access to look up the owner's home")
	}
	dir := path.Dir(f.remotePath(strings.TrimPrefix(remote, "/")))
	c, err := f.getSftpConnection(ctx)
	if err != nil {
		return "", err
	}
	fi, err := c.sftpClient.Stat(dir)
	f.putSftpConnection(&c, err)
	if err != nil {
		return "", fmt.Errorf("owner of %q: %w", dir, err)
	}
	uid, _, ok := owner(fi)
	if !ok {
		return "", fmt.Errorf("owner of %q unknown", dir)
	}
	f.ownerMu.Lock()
	home, found := f.ownerHome[uid]
	f.ownerMu.Unlock()
	if !found {
		out, err := f.run(ctx, "getent passwd "+strconv.Itoa(uid))
		if err != nil {
			return "", err
		}
		fields := strings.Split(strings.TrimSpace(string(out)), ":")
		if len(fields) < 7 || !strings.HasPrefix(fields[5], "/") {
			return "", fmt.Errorf("no home for uid %d", uid)
		}
		home = path.Clean(fields[5])
		f.ownerMu.Lock()
		if f.ownerHome == nil {
			f.ownerHome = map[int]string{}
		}
		f.ownerHome[uid] = home
		f.ownerMu.Unlock()
	}
	return path.Join(home, ".local/share/Trash"), nil
}
