package importer

import (
	"os"
	"sync"
	"time"
)

// groupFile is an append-only file with group commit. Records are written
// straight to the file (so they survive the process dying) but are fsynced
// in batches: by a background flusher at most every groupCommitInterval, by
// syncTo for the callers that need a record durable now, and on close.
// Concurrent syncTo callers share one fsync.
type groupFile struct {
	f *os.File

	mu      sync.Mutex
	cond    *sync.Cond
	seq     uint64 // records written
	synced  uint64 // records known durable
	syncing bool
	err     error // first write or sync error

	stop chan struct{}
	done chan struct{}
}

// groupCommitInterval is how often pending records are fsynced in the
// background. Tests raise it to count only the fsyncs callers asked for.
var groupCommitInterval = 150 * time.Millisecond

func newGroupFile(f *os.File) *groupFile {
	g := &groupFile{f: f, stop: make(chan struct{}), done: make(chan struct{})}
	g.cond = sync.NewCond(&g.mu)
	go g.flusher(groupCommitInterval)
	return g
}

func (g *groupFile) flusher(every time.Duration) {
	defer close(g.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			g.mu.Lock()
			pending := g.seq
			g.mu.Unlock()
			_ = g.syncTo(pending)
		case <-g.stop:
			return
		}
	}
}

// append writes one line and returns its sequence number for syncTo.
func (g *groupFile) append(line string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.f.WriteString(line + "\n"); err != nil && g.err == nil {
		g.err = err
	}
	g.seq++
	return g.seq
}

// syncTo returns once record seq (and every record before it) is on disk.
// If no fsync is running, the caller runs one covering every record written
// so far; otherwise it waits for the running one and checks again.
func (g *groupFile) syncTo(seq uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.synced < seq {
		if g.syncing {
			g.cond.Wait()
			continue
		}
		target := g.seq
		g.syncing = true
		g.mu.Unlock()
		err := syncFile(g.f) // records ≤ target were written before this started
		g.mu.Lock()
		g.syncing = false
		if err != nil {
			if g.err == nil {
				g.err = err
			}
			g.cond.Broadcast()
			return err
		}
		g.synced = max(g.synced, target)
		g.cond.Broadcast()
	}
	return g.err
}

// close stops the flusher, syncs what is pending and closes the file.
func (g *groupFile) close() error {
	close(g.stop)
	<-g.done
	g.mu.Lock()
	pending := g.seq
	g.mu.Unlock()
	err := g.syncTo(pending)
	if cerr := g.f.Close(); err == nil {
		err = cerr
	}
	return err
}
