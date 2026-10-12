//go:build cmount && windows

package cmount

// The drive icon (--drive-icon) and the path to check a Mount Manager
// mountpoint by.
//
// Explorer takes a drive icon from
// HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\DriveIcons\<letter>\DefaultIcon,
// which only an administrator can write. So the icon is copied, at every
// mount, to %ProgramData%\rclone\drive-icons\<letter>.ico, and that key
// points there: written once by an administrator (rclone run elevated does
// it, or the command logged), then every later mount just replaces the file.
// The icon may be a file on the mount itself ("X:\icon.ico"), so changing it
// there changes the drive icon at the next mount.

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rclone/rclone/fs"
	"golang.org/x/sys/windows/registry"
)

// statPath returns the path to check whether mountpoint is available
func statPath(mountpoint string) string {
	if isMountManagerDrive(mountpoint) {
		return mountpoint[len(mountpoint)-2:] + `\`
	}
	return mountpoint
}

// setDriveIcon gives the drive of mountpoint the icon at iconPath
func setDriveIcon(fsys *FS, mountpoint, iconPath string) {
	m := strings.TrimSuffix(mountpoint, `\`)
	if len(m) < 2 || m[len(m)-1] != ':' {
		fs.Errorf(nil, "Drive icon: %q is not a drive letter mountpoint", mountpoint)
		return
	}
	letter := strings.ToUpper(m[len(m)-2 : len(m)-1])
	var data []byte
	var err error
	if len(iconPath) > 2 && strings.EqualFold(iconPath[:2], letter+":") {
		// on the mount itself: read it through the VFS
		data, err = fsys.VFS.ReadFile(filepath.ToSlash(iconPath[2:]))
	} else {
		data, err = os.ReadFile(iconPath)
	}
	if err != nil {
		fs.Errorf(nil, "Drive icon: can't read %s: %v", iconPath, err)
		return
	}
	dir := filepath.Join(os.Getenv("ProgramData"), "rclone", "drive-icons")
	dst := filepath.Join(dir, letter+".ico")
	if old, err := os.ReadFile(dst); err != nil || string(old) != string(data) {
		if err = os.MkdirAll(dir, 0777); err == nil {
			err = os.WriteFile(dst, data, 0666)
		}
		if err != nil {
			fs.Errorf(nil, "Drive icon: can't write %s (an administrator can make %s writable for Users): %v", dst, dir, err)
			return
		}
	}
	keyPath := `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\DriveIcons\` + letter + `\DefaultIcon`
	current := ""
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, keyPath, registry.QUERY_VALUE); err == nil {
		current, _, _ = k.GetStringValue("")
		_ = k.Close()
	}
	if !strings.EqualFold(current, dst) {
		k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, keyPath, registry.SET_VALUE)
		if err == nil {
			err = k.SetStringValue("", dst)
			_ = k.Close()
		}
		if err != nil {
			fs.Logf(nil, "Drive icon: needs, once, as an administrator: reg add \"HKLM\\%s\" /ve /d \"%s\" /f", keyPath, dst)
			return
		}
	}
	// tell Explorer (SHCNE_ASSOCCHANGED)
	if proc := syscall.NewLazyDLL("shell32.dll").NewProc("SHChangeNotify"); proc.Find() == nil {
		_, _, _ = proc.Call(0x08000000, 0, 0, 0)
	}
	fs.Debugf(nil, "Drive icon: %s from %s", dst, iconPath)
}
