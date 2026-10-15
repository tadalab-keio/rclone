package mountlib

// --exit-if-unreachable host:port: unmount and exit when the remote stops
// answering twice in a row (one question a second, one second each).
//
// A remote reached over a network which goes away (Wi-Fi changes, a VPN
// drops) leaves the mount up while every call waits on dead connections,
// and programs that touch the drive hang with it: on Windows, Explorer
// stopped responding and was closed, and a PowerShell started to check the
// network waited at startup, as it looks at every drive. Leaving the mount
// lets those calls fail at once. A process still running unreachableExitAfter
// after the unmount started (a call stuck in the file system keeps it,
// unmounted or not) exits: that ends the stuck calls and removes the drive.
//
// Until the mount is up, the question is a TCP connection to host:port, so
// a mount which never comes up exits too. Once it is up, the question goes
// to the remote over the connections it already has (the root's metadata):
// a new connection each second to an ssh server is a connection closed
// before logging in, which OpenSSH counts against the address it comes from
// (PerSourcePenalties). With one login that timed out on top, that shut a
// lab PC out of the server for 90 s (2026-10-15).

import (
	"context"
	"errors"
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

// askRoot asks the remote for its root's metadata; a directory or nothing
// there is an answer too
func askRoot(f fs.Fs) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := f.NewObject(ctx, "")
	if err == nil || errors.Is(err, fs.ErrorIsDir) || errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorNotAFile) {
		return nil
	}
	return err
}

// watchReachable unmounts with the function in unmount (exits if none is
// set yet) once the remote has not answered unreachableFailures times in a
// row: dialing addr until the mount is up (remote is set), then asking the
// remote, one question at a time
func watchReachable(addr string, unmount *atomic.Pointer[func() error], remote *atomic.Pointer[fs.Fs]) {
	failures := 0
	var pending chan error
	for range time.Tick(unreachableEvery) {
		var err error
		if f := remote.Load(); f == nil {
			conn, e := net.DialTimeout("tcp", addr, unreachableEvery)
			if e == nil {
				_ = conn.Close()
			}
			err = e
		} else {
			if pending == nil {
				pending = make(chan error, 1)
				go func(c chan error, f fs.Fs) { c <- askRoot(f) }(pending, *f)
			}
			select {
			case err = <-pending:
				pending = nil
			case <-time.After(unreachableEvery):
				// still waiting: a failure, and no new question until it answers
				err = errors.New("no answer in " + unreachableEvery.String())
			}
		}
		if err == nil {
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
