package main

import (
	"bytes"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// startHotplug launches goroutines that auto-rescan when:
//   - mounts change (poll /proc/self/mountinfo for POLLPRI)
//   - block devices are added/removed (netlink uevent socket)
//
// Both events feed into a single debounced trigger so a burst of kernel
// events still maps to one scan. Errors at startup are non-fatal — the
// "Rescan" button keeps working either way.
func (u *UI) startHotplug() {
	trigger := make(chan struct{}, 1)
	go u.debouncedRescan(trigger, 250*time.Millisecond)
	go watchMountinfo(trigger)
	go watchUevent(trigger)
}

func (u *UI) debouncedRescan(trigger <-chan struct{}, wait time.Duration) {
	var timer *time.Timer
	for range trigger {
		if timer == nil {
			timer = time.AfterFunc(wait, func() { go u.doScan() })
		} else {
			timer.Reset(wait)
		}
	}
}

func nudge(trigger chan<- struct{}) {
	select {
	case trigger <- struct{}{}:
	default:
	}
}

// watchMountinfo blocks on /proc/self/mountinfo via poll(2). The kernel marks
// the file with POLLPRI / POLLERR whenever the mount table changes — a much
// cheaper signal than re-running lsblk on a timer.
func watchMountinfo(trigger chan<- struct{}) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		logf("hotplug: open mountinfo failed: %v", err)
		return
	}
	defer f.Close()
	fd := int(f.Fd())
	logf("hotplug: watching /proc/self/mountinfo for mount changes")
	for {
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLPRI | unix.POLLERR}}
		n, err := unix.Poll(pfd, -1)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			logf("hotplug: mountinfo poll error: %v", err)
			return
		}
		if n == 0 {
			continue
		}
		// Rewind and drain so the next poll fires on the next change rather
		// than re-firing immediately on the same state.
		_, _ = f.Seek(0, 0)
		_, _ = f.Read(make([]byte, 4096))
		nudge(trigger)
	}
}

// watchUevent listens on a NETLINK_KOBJECT_UEVENT socket for block-device
// add/remove/change messages from the kernel. Doesn't require root.
func watchUevent(trigger chan<- struct{}) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		logf("hotplug: uevent socket failed: %v", err)
		return
	}
	addr := &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1}
	if err := unix.Bind(fd, addr); err != nil {
		logf("hotplug: uevent bind failed: %v", err)
		unix.Close(fd)
		return
	}
	defer unix.Close(fd)
	logf("hotplug: listening on netlink uevent")
	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			logf("hotplug: uevent recv error: %v", err)
			return
		}
		if n <= 0 {
			continue
		}
		msg := buf[:n]
		// We only care about block-subsystem events; checking the payload
		// for "SUBSYSTEM=block" filters out the firehose of input/usb/etc.
		if !bytes.Contains(msg, []byte("SUBSYSTEM=block")) {
			continue
		}
		nudge(trigger)
	}
}
