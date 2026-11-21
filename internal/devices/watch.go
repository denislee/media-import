package devices

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Watch calls changed (block is true for block-device events) whenever the mounted volumes or the block devices may
// have changed, until ctx is done. It sleeps in the kernel between events:
//
//   - /proc/self/mountinfo is polled for POLLPRI, which the kernel raises
//     when the mount table changes;
//   - a NETLINK_KOBJECT_UEVENT socket reports block devices being added,
//     removed or changed. Udev may still be probing a new device (label,
//     filesystem) when the kernel event arrives, so a block event is reported
//     again a little later.
//
// gvfs mounts (phones, cameras) are FUSE directories that neither source
// announces, so changed is also called every fallback interval.
//
// Both sleep without a timeout of their own; an eventfd signalled when ctx is
// done wakes them.
func Watch(ctx context.Context, fallback time.Duration, changed func(block bool)) {
	stop, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		stop = -1
	}
	var unregister func() bool
	if stop >= 0 {
		unregister = context.AfterFunc(ctx, func() {
			var one [8]byte
			one[0] = 1
			unix.Write(stop, one[:])
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); watchMounts(ctx, stop, fallback, func() { changed(false) }) }()
	go func() { defer wg.Done(); watchUevents(ctx, stop, func() { changed(true) }) }()
	wg.Wait()
	if stop >= 0 {
		unregister()
		unix.Close(stop)
	}
}

// pollCalls counts unix.Poll calls by the watchers, so tests can check that an
// idle watcher sleeps instead of waking up periodically.
var pollCalls atomic.Int64

func poll(fds []unix.PollFd, timeoutMs int) (int, error) {
	pollCalls.Add(1)
	return unix.Poll(fds, timeoutMs)
}

// pollTimeout is the poll timeout in ms: until the deadline d (-1: none) when
// stop is usable, otherwise at most a second so ctx is still noticed.
func pollTimeout(stop int, d time.Duration) int {
	if stop < 0 {
		if d < 0 || d > time.Second {
			d = time.Second
		}
	}
	if d < 0 {
		return -1
	}
	return int((d + time.Millisecond - 1) / time.Millisecond)
}

func watchMounts(ctx context.Context, stop int, fallback time.Duration, changed func()) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		tickUntilDone(ctx, fallback, changed)
		return
	}
	defer f.Close()
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLPRI}}
	if stop >= 0 {
		fds = append(fds, unix.PollFd{Fd: int32(stop), Events: unix.POLLIN})
	}
	last := time.Now()
	for ctx.Err() == nil {
		n, err := poll(fds, pollTimeout(stop, max(0, fallback-time.Since(last))))
		switch {
		case err != nil && err != unix.EINTR:
			tickUntilDone(ctx, fallback, changed)
			return
		case stop >= 0 && n > 0 && fds[len(fds)-1].Revents != 0:
			return
		case n > 0 && fds[0].Revents&(unix.POLLPRI|unix.POLLERR) != 0:
			last = time.Now()
			changed()
		case time.Since(last) >= fallback:
			last = time.Now()
			changed()
		}
	}
}

func watchUevents(ctx context.Context, stop int, changed func()) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return // the mountinfo watcher and its fallback still run
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1}); err != nil {
		return
	}
	buf := make([]byte, 16<<10)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if stop >= 0 {
		fds = append(fds, unix.PollFd{Fd: int32(stop), Events: unix.POLLIN})
	}
	var later *time.Timer
	defer func() {
		if later != nil {
			later.Stop()
		}
	}()
	for ctx.Err() == nil {
		n, err := poll(fds, pollTimeout(stop, -1))
		if err != nil && err != unix.EINTR {
			return
		}
		if stop >= 0 && n > 0 && fds[len(fds)-1].Revents != 0 {
			return
		}
		if n <= 0 || fds[0].Revents == 0 {
			continue
		}
		m, _, err := unix.Recvfrom(fd, buf, unix.MSG_DONTWAIT)
		if err != nil || !isBlockUevent(buf[:m]) {
			continue
		}
		changed()
		if later != nil {
			later.Stop()
		}
		later = time.AfterFunc(1500*time.Millisecond, func() {
			if ctx.Err() == nil {
				changed()
			}
		})
	}
}

// isBlockUevent reports whether a kernel uevent (NUL-separated
// "action@devpath", "KEY=value"…) is about a block device.
func isBlockUevent(msg []byte) bool {
	for f := range bytes.SplitSeq(msg, []byte{0}) {
		if bytes.Equal(f, []byte("SUBSYSTEM=block")) {
			return true
		}
	}
	return false
}

func tickUntilDone(ctx context.Context, every time.Duration, changed func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			changed()
		}
	}
}

// UnmountedCache runs ListUnmounted (an lsblk fork) only when something it
// depends on changed: the set of block devices in /sys/class/block or the
// mount table. Invalidate forces the next call to run it.
type UnmountedCache struct {
	mu     sync.Mutex
	valid  bool
	blocks []string
	mounts [32]byte
	last   []Unmounted

	// for tests
	sysBlock, mountinfo string
	list                func() []Unmounted
}

// Get returns the unmounted removable partitions.
func (c *UnmountedCache) Get() []Unmounted {
	sysBlock, mountinfo, list := c.sysBlock, c.mountinfo, c.list
	if sysBlock == "" {
		sysBlock, mountinfo, list = "/sys/class/block", "/proc/self/mountinfo", ListUnmounted
	}
	var blocks []string
	if ents, err := os.ReadDir(sysBlock); err == nil {
		for _, e := range ents {
			blocks = append(blocks, e.Name())
		}
	}
	var mounts [32]byte
	if data, err := os.ReadFile(mountinfo); err == nil {
		mounts = sha256.Sum256(data)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && slices.Equal(blocks, c.blocks) && mounts == c.mounts {
		return c.last
	}
	c.last, c.blocks, c.mounts, c.valid = list(), blocks, mounts, true
	return c.last
}

// Invalidate makes the next Get run lsblk again (after a device event: a
// device's label or filesystem may change without its name changing).
func (c *UnmountedCache) Invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}
