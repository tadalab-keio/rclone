//go:build cmount && !windows && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || (openbsd && cgo))

package cmount

import "github.com/rclone/rclone/fs"

// statPath returns the path to check whether mountpoint is available
func statPath(mountpoint string) string {
	return mountpoint
}

// setDriveIcon is supported on Windows only
func setDriveIcon(fsys *FS, mountpoint, iconPath string) {
	fs.Logf(nil, "--drive-icon is supported on Windows only")
}
