package mountlib

// --exit-if-unreachable host:port: unmount and exit when a TCP connection
// to host:port fails twice in a row (one try a second, one second each).
//
// A remote reached over a network which goes away (Wi-Fi changes, a VPN
// drops) leaves the mount up while every call waits on dead connections,
// and programs that touch the drive hang with it: on Windows, Explorer
// stopped responding and was closed, and a PowerShell started to check the
// network waited at startup, as it looks at every drive. Leaving the mount
// lets those calls fail at once. The check starts before the remote is
// first reached, so a mount which never comes up exits too. A process still
// running unreachableExitAfter after the unmount started (a call stuck in
// the file system keeps it, unmounted or not) exits: that ends the stuck
// calls and removes the drive.

import (
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/fs"
)

const (
	unreachableEvery     = time.Second
	unreachableFailures  = 2
	unreachableExitAfter = 3 * time.Second
)

// watchReachable unmounts with the function in unmount (exits if none is
// set yet) once addr has been unreachable unreachableFailures times in a row
func watchReachable(addr string, unmount *atomic.Pointer[func() error]) {
	failures := 0
	for range time.Tick(unreachableEvery) {
		conn, err := net.DialTimeout("tcp", addr, unreachableEvery)
		if err == nil {
			_ = conn.Close()
			failures = 0
			continue
		}
		failures++
		if failures < unreachableFailures {
			continue
		}
		fn := unmount.Load()
		if fn == nil {
			fs.Errorf(nil, "%s unreachable (%v) before mounting: exiting", addr, err)
			os.Exit(1)
		}
		fs.Logf(nil, "%s unreachable (%v): unmounting", addr, err)
		go func() {
			if err := (*fn)(); err != nil {
				fs.Errorf(nil, "unmount failed: %v", err)
			}
		}()
		// a call stuck in the file system keeps the process (and the
		// program waiting on that call) after the drive is gone
		time.Sleep(unreachableExitAfter)
		fs.Errorf(nil, "still running %v after unmounting: exiting", unreachableExitAfter)
		os.Exit(1)
	}
}
