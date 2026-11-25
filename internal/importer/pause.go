package importer

import (
	"context"
	"sync"
	"time"
)

// Gate pauses and resumes a running import. The zero value is ready to use.
type Gate struct {
	mu       sync.Mutex
	ch       chan struct{} // non-nil while paused; closed on resume
	since    time.Time
	pausedOf time.Duration
}

// Pause makes workers stop at the next chunk or file boundary.
func (g *Gate) Pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ch == nil {
		g.ch, g.since = make(chan struct{}), time.Now()
	}
}

// Resume lets paused workers continue.
func (g *Gate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ch != nil {
		close(g.ch)
		g.ch = nil
		g.pausedOf += time.Since(g.since)
	}
}

// Paused reports whether the gate is closed.
func (g *Gate) Paused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ch != nil
}

// pausedFor is the total time spent paused up to now.
func (g *Gate) pausedFor(now time.Time) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.pausedOf
	if g.ch != nil {
		d += now.Sub(g.since)
	}
	return d
}

// wait blocks while paused, returning early if ctx is cancelled.
func (g *Gate) wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.ch
	g.mu.Unlock()
	if ch == nil {
		return ctx.Err()
	}
	select {
	case <-ch:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
