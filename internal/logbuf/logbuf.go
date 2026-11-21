// Package logbuf keeps the last N log lines with amortised O(1) appends.
package logbuf

// Buffer keeps the most recent lines appended to it, up to a fixed maximum.
//
// The buffer never mutates a line it has already published: appends only ever
// extend the tail, and the periodic compaction moves the retained lines into a
// fresh backing array instead of overwriting the old one. A slice handed out by
// Lines therefore stays valid — and keeps its contents — even after many more
// appends, so a reader may hold it without the lock its owner uses.
type Buffer struct {
	max   int
	lines []string
}

// New returns a Buffer that keeps at most max lines. It panics if max <= 0.
func New(max int) *Buffer {
	if max <= 0 {
		panic("logbuf: New called with non-positive max")
	}
	return &Buffer{max: max}
}

// Append adds s as the newest line.
//
// It never writes into an existing backing array at an index below
// len(b.lines), so any slice previously returned by Lines is left unchanged;
// once the live log has grown to twice the maximum the retained lines are
// copied into a new array, which makes the cost of an append amortised O(1).
func (b *Buffer) Append(s string) {
	b.lines = append(b.lines, s)
	if len(b.lines) >= 2*b.max {
		trimmed := make([]string, b.max, 2*b.max)
		copy(trimmed, b.lines[len(b.lines)-b.max:])
		b.lines = trimmed
	}
}

// Lines returns the last min(len, max) lines, oldest first.
//
// The result is a subslice of the buffer's storage, not a copy, so the caller
// must not modify its elements; its capacity is clipped to its length, so
// appending to it cannot clobber lines the buffer still owns. Because Append
// never rewrites published lines, the returned slice keeps its contents and
// may safely be held by a reader without the owner's lock — at worst it lags
// behind newer appends.
func (b *Buffer) Lines() []string {
	if len(b.lines) <= b.max {
		return b.lines[:len(b.lines):len(b.lines)]
	}
	i := len(b.lines) - b.max
	return b.lines[i:len(b.lines):len(b.lines)]
}

// Reset discards every kept line. Slices returned earlier by Lines are not
// affected: they keep their contents because Reset only drops the buffer's
// reference to the array.
func (b *Buffer) Reset() {
	b.lines = nil
}
