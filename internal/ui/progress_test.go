package ui

import (
	"testing"
	"time"

	"gioui.org/app"

	"github.com/denislee/media-import/internal/importer"
	"github.com/denislee/media-import/internal/logbuf"
)

func snap(bytes int64, paused bool, elapsed time.Duration) importer.Progress {
	return importer.Progress{
		Phase:      "importing",
		Total:      10,
		Done:       2,
		Copied:     2,
		Bytes:      bytes,
		TotalBytes: 1 << 30,
		Paused:     paused,
		Elapsed:    elapsed,
		Active: []importer.FileProgress{{
			Path: "/src/a.jpg", Stage: "copying", Size: 1 << 20, Bytes: bytes % (1 << 20),
		}},
		ByKind: map[string]int{"photo": 2},
	}
}

// TestPausedProgressSkipsRedraws checks that a paused import does not redraw
// the window on every tick.
func TestPausedProgressSkipsRedraws(t *testing.T) {
	s := &state{win: new(app.Window), logs: logbuf.New(100)}
	s.prog = importer.Progress{Phase: "scanning"}
	now := time.Now()
	redraws := 0
	for i := range 20 {
		_, redraw := s.onProgress(snap(5000, true, 3*time.Second+time.Duration(i)*time.Millisecond),
			now.Add(time.Duration(i)*100*time.Millisecond))
		if redraw {
			redraws++
		}
	}
	if redraws > 1 {
		t.Errorf("%d redraws for a paused import, want at most 1 (HEAD redrew every tick)", redraws)
	}
}

// TestMovingProgressRedrawsEveryTick checks that a running import still
// redraws on every tick.
func TestMovingProgressRedrawsEveryTick(t *testing.T) {
	s := &state{win: new(app.Window), logs: logbuf.New(100)}
	s.prog = importer.Progress{Phase: "scanning"}
	now := time.Now()
	redraws := 0
	for i := range 20 {
		_, redraw := s.onProgress(
			snap(int64(i+1)*(1<<20)+int64(i)*1000, false, 3*time.Second+time.Duration(i)*100*time.Millisecond),
			now.Add(time.Duration(i)*100*time.Millisecond))
		if redraw {
			redraws++
		}
	}
	if redraws != 20 {
		t.Errorf("%d redraws for a moving import, want 20", redraws)
	}
}

// TestElapsedSecondRedraws checks that a whole-second elapsed change is a
// visible change while a sub-second one is not.
func TestElapsedSecondRedraws(t *testing.T) {
	if !progressChanged(snap(5000, true, 3*time.Second), snap(5000, true, 4*time.Second), 0, 0) {
		t.Error("elapsed 3s -> 4s did not count as a change")
	}
	if progressChanged(snap(5000, true, 3*time.Second), snap(5000, true, 3*time.Second+200*time.Millisecond), 0, 0) {
		t.Error("elapsed 3s -> 3.2s counted as a change")
	}
}
