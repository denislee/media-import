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
