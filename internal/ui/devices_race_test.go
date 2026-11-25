package ui

import (
	"fmt"
	"image"
	"sync"
	"testing"
	"time"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/font/gofont"
	"gioui.org/io/input"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/widget/material"

	"github.com/denislee/media-import/internal/devices"
)

// TestDeviceButtonsSurviveGrowth grows the device lists from another
// goroutine (as the volume watcher does) while a frame lays out rows it took
// from deviceItems. Under -race this must not report a data race: the
// clickables must keep stable addresses. Before they did, growing the lists
// copied the button array while a row's Clickable was writing its state.
func TestDeviceButtonsSurviveGrowth(t *testing.T) {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	s := &state{win: new(app.Window), th: th, pal: darkPalette()}

	var mu sync.Mutex
	n := 0
	oldVolumes, oldUnmounted := listVolumes, listUnmounted
	listVolumes = func() []devices.Volume {
		mu.Lock()
		count := n
		mu.Unlock()
		vols := make([]devices.Volume, 0, count)
		for i := 0; i < count; i++ {
			vols = append(vols, devices.Volume{
				Label: fmt.Sprintf("v%d", i),
				Path:  fmt.Sprintf("/media/v%d", i),
				FS:    "vfat",
				Dev:   fmt.Sprintf("/dev/sd%c1", 'a'+i),
			})
		}
		return vols
	}
	listUnmounted = func(*devices.UnmountedCache) []devices.Unmounted {
		mu.Lock()
		count := n
		mu.Unlock()
		unm := make([]devices.Unmounted, 0, count)
		for i := 0; i < count; i++ {
			unm = append(unm, devices.Unmounted{
				Dev:  fmt.Sprintf("/dev/sr%d", i),
				FS:   "exfat",
				Size: 1 << 30,
			})
		}
		return unm
	}
	t.Cleanup(func() {
		listVolumes, listUnmounted = oldVolumes, oldUnmounted
	})

	mu.Lock()
	n = 1
	mu.Unlock()
	s.refreshVolumes()

	// One frame's rows, laid out over and over (a slow frame) while the
	// watcher grows the lists. Hover events make the clickables write their
	// state, as a real pointer does.
	items := s.deviceItems()
	var r input.Router
	frame := func(i int) {
		ops := new(op.Ops)
		gtx := layout.Context{Ops: ops, Constraints: layout.Exact(image.Pt(400, 60)),
			Now: time.Now(), Source: r.Source()}
		for _, it := range items {
			s.deviceRow(gtx, it)
		}
		r.Frame(ops)
		pos := f32.Pt(20, 20)
		if i%2 == 1 {
			pos = f32.Pt(-50, -50)
		}
		r.Queue(pointer.Event{Kind: pointer.Move, Source: pointer.Mouse, Position: pos})
	}
	frame(0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 2; i <= 64; i++ {
			mu.Lock()
			n = i
			mu.Unlock()
			s.refreshVolumes()
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 1; ; i++ {
		select {
		case <-done:
		default:
			frame(i)
			continue
		}
		break
	}

	items = s.deviceItems()
	frame(0)
	if got, want := len(items), 128; got != want {
		t.Fatalf("deviceItems() = %d items, want %d", got, want)
	}
}
