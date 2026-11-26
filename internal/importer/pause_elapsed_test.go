package importer

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestPausedElapsedIsConstant checks that a paused import's elapsed time does
// not move, or every progress tick looks like a change and redraws the window.
func TestPausedElapsedIsConstant(t *testing.T) {
	src, dst := setup(t)
	g := &Gate{}
	g.Pause()
	opt := baseOpts(src, dst)
	opt.Pause = g

	ctx, cancel := context.WithCancel(context.Background())
	var (
		mu      sync.Mutex
		elapsed []time.Duration
		done    = make(chan struct{})
	)
	go func() {
		defer close(done)
		_, _ = Run(ctx, opt, func(p Progress) {
			if !p.Paused {
				return
			}
			mu.Lock()
			elapsed = append(elapsed, p.Elapsed)
			mu.Unlock()
		}, nil)
	}()

	time.Sleep(time.Second)
	mu.Lock()
	got := append([]time.Duration(nil), elapsed...)
	mu.Unlock()
	cancel()
	g.Resume()
	<-done

	if len(got) < 5 {
		t.Fatalf("only %d paused samples, want at least 5", len(got))
	}
	for i, d := range got {
		if d != got[0] {
			t.Fatalf("sample %d elapsed %v differs from first %v", i, d, got[0])
		}
	}
}

// TestElapsedNotNegativeWhenPausedBeforeRun checks that a gate paused before
// Run starts does not make Progress.Elapsed negative: the pause accumulated
// before the run began is not part of the run's time, so it must not be
// subtracted from an elapsed clock that never included it.
func TestElapsedNotNegativeWhenPausedBeforeRun(t *testing.T) {
	src, dst := setup(t)
	g := &Gate{}
	g.Pause()

	// Pause before Run starts, so pausedAtStart already covers this time.
	time.Sleep(200 * time.Millisecond)

	opt := baseOpts(src, dst)
	opt.Pause = g

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu      sync.Mutex
		samples []Progress
		done    = make(chan struct{})
	)
	var final Progress
	go func() {
		defer close(done)
		final, _ = Run(ctx, opt, func(p Progress) {
			mu.Lock()
			samples = append(samples, p)
			mu.Unlock()
		}, nil)
	}()

	time.Sleep(300 * time.Millisecond)
	g.Resume()
	<-done

	mu.Lock()
	got := append([]Progress(nil), samples...)
	mu.Unlock()

	if len(got) == 0 {
		t.Fatal("no progress samples collected")
	}
	for i, p := range got {
		if p.Elapsed < 0 {
			t.Fatalf("sample %d elapsed %v is negative", i, p.Elapsed)
		}
		// Pausing only after Run started adds a few milliseconds, never the
		// 200ms the gate was already closed before the run began.
		if p.Paused && p.Elapsed >= 100*time.Millisecond {
			t.Fatalf("paused sample %d elapsed %v, want under 100ms (the pre-Run pause must not count)", i, p.Elapsed)
		}
	}
	if final.Elapsed < 0 {
		t.Fatalf("final elapsed %v is negative", final.Elapsed)
	}
}
