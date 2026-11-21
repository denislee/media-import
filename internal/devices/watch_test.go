package devices

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsBlockUevent(t *testing.T) {
	block := []byte("add@/devices/x/block/sdc\x00ACTION=add\x00SUBSYSTEM=block\x00DEVNAME=sdc")
	if !isBlockUevent(block) {
		t.Error("isBlockUevent(block device event) = false, want true")
	}
	usb := []byte("add@/devices/x/block/sdc\x00ACTION=add\x00SUBSYSTEM=usb\x00DEVNAME=sdc")
	if isBlockUevent(usb) {
		t.Error("isBlockUevent(usb event) = true, want false")
	}
	blockx := []byte("add@/devices/x/block/sdc\x00SUBSYSTEM=blockx\x00DEVNAME=sdc")
	if isBlockUevent(blockx) {
		t.Error("isBlockUevent(SUBSYSTEM=blockx) = true, want false")
	}
}

func TestUnmountedCacheRunsListOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "block")
	if err := os.Mkdir(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "sda"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mi := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(mi, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := &UnmountedCache{
		sysBlock:  sys,
		mountinfo: mi,
		list: func() []Unmounted {
			calls++
			return []Unmounted{{Dev: "/dev/sdc1"}}
		},
	}

	for i := 0; i < 3; i++ {
		got := c.Get()
		if len(got) != 1 || got[0].Dev != "/dev/sdc1" {
			t.Fatalf("Get() = %v, want one entry with Dev /dev/sdc1", got)
		}
	}
	if calls != 1 {
		t.Fatalf("list called %d times after three Gets, want 1", calls)
	}

	// A new block device must force lsblk to run again.
	if err := os.WriteFile(filepath.Join(sys, "sdc"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c.Get()
	if calls != 2 {
		t.Fatalf("list called %d times after a new block device, want 2", calls)
	}

	// So must a change in the mount table.
	if err := os.WriteFile(mi, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Get()
	if calls != 3 {
		t.Fatalf("list called %d times after a mount table change, want 3", calls)
	}

	// Invalidate forces one more run even if nothing observable changed.
	c.Invalidate()
	c.Get()
	if calls != 4 {
		t.Fatalf("list called %d times after Invalidate, want 4", calls)
	}
	c.Get()
	if calls != 4 {
		t.Fatalf("list called %d times after a further Get, want 4", calls)
	}
}

func TestWatchStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Watch(ctx, time.Hour, func(bool) {})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Watch did not return promptly after cancel")
	}
}

func TestWatchIdleDoesNotWake(t *testing.T) {
	before := pollCalls.Load()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Watch(ctx, time.Hour, func(bool) {})
		close(done)
	}()
	time.Sleep(1500 * time.Millisecond)
	n := pollCalls.Load() - before
	cancel()
	<-done
	if n > 3 {
		t.Errorf("%d polls in 1.5 s while idle, want at most 3 (one per watcher, plus a stray uevent)", n)
	}
}

func TestPollTimeout(t *testing.T) {
	tests := []struct {
		stop int
		d    time.Duration
		want int
	}{
		{3, -1, -1},
		{3, 0, 0},
		{3, 1500 * time.Millisecond, 1500},
		{3, 1500*time.Millisecond + 1, 1501},
		{-1, -1, 1000},
		{-1, 5 * time.Second, 1000},
		{-1, 200 * time.Millisecond, 200},
	}
	for _, tt := range tests {
		if got := pollTimeout(tt.stop, tt.d); got != tt.want {
			t.Errorf("pollTimeout(%d, %v) = %d, want %d", tt.stop, tt.d, got, tt.want)
		}
	}
}

func TestWatchFallbackFires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan bool, 10)
	go Watch(ctx, 50*time.Millisecond, func(b bool) {
		select {
		case ch <- b:
		default:
		}
	})
	timeout := time.After(3 * time.Second)
	for {
		select {
		case b := <-ch:
			if !b {
				return
			}
		case <-timeout:
			t.Fatal("Watch did not fire the fallback")
		}
	}
}
