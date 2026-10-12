package vfscache

// A copy of a file whose upload or rename failed is kept in
// --vfs-rescue-dir, so that whatever goes wrong after such an error (a
// retry, a later rename, the cache being cleaned) can no longer lose the
// data the user wrote. Off when the option is empty.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/fs"
)

// _rescue copies the cache file of item into the rescue directory, once
// for each content (size and modification time)
//
// call with item.mu held
func (item *Item) _rescue(reason string) {
	dir := item.c.opt.RescueDir
	if dir == "" {
		return
	}
	src := item.c.toOSPath(item.name)
	fi, err := os.Stat(src)
	if err != nil {
		fs.Errorf(item.name, "vfs cache: %s; no copy kept: %v", reason, err)
		return
	}
	key := fmt.Sprintf("%d,%d", fi.Size(), fi.ModTime().UnixNano())
	if item.rescued == key {
		return
	}
	dst := filepath.Join(dir, time.Now().Format("20060102-150405.000"), filepath.FromSlash(item.name))
	if err := copyFile(src, dst); err != nil {
		fs.Errorf(item.name, "vfs cache: %s; copy to %s failed: %v", reason, dst, err)
		return
	}
	item.rescued = key
	fs.Logf(item.name, "vfs cache: %s; a copy is kept in %s", reason, dst)
}

// Rescue copies the cache file of name into the rescue directory, if the
// cache holds it
func (c *Cache) Rescue(name, reason string) {
	item, found := c.get(name)
	if !found {
		return
	}
	item.mu.Lock()
	item._rescue(reason)
	item.mu.Unlock()
}

// copyFile copies src to a new file dst, making its directory
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
