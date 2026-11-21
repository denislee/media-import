package logbuf

import (
	"slices"
	"strconv"
	"testing"
)

func TestKeepsLastMax(t *testing.T) {
	b := New(5)
	for i := range 23 {
		b.Append(strconv.Itoa(i))
	}
	want := []string{"18", "19", "20", "21", "22"}
	if got := b.Lines(); !slices.Equal(got, want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
	if got := len(b.Lines()); got != 5 {
		t.Fatalf("Len() = %d, want 5", got)
	}
}

func TestOldSliceUnchanged(t *testing.T) {
	b := New(3)
	for _, s := range []string{"a", "b", "c", "d"} {
		b.Append(s)
	}
	old := b.Lines()
	snapshot := slices.Clone(old)
	for i := range 10 {
		b.Append("extra" + strconv.Itoa(i))
	}
	if !slices.Equal(old, snapshot) {
		t.Fatalf("slice returned by Lines changed: got %v, want %v", old, snapshot)
	}
}

func TestReset(t *testing.T) {
	b := New(3)
	b.Append("a")
	b.Append("b")
	b.Reset()
	if got := b.Lines(); len(got) != 0 {
		t.Fatalf("after Reset Lines() = %v, want empty", got)
	}
	if got := len(b.Lines()); got != 0 {
		t.Fatalf("after Reset Len() = %d, want 0", got)
	}
	b.Append("c")
	if got := b.Lines(); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("after Reset+Append Lines() = %v, want [c]", got)
	}
}

func BenchmarkAppend50k(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		buf := New(5000)
		for range 50000 {
			buf.Append("line")
		}
	}
}

// BenchmarkOldTrim50k is a negative control: it measures the previous
// algorithm, which re-copied the whole window on every overflow.
func BenchmarkOldTrim50k(b *testing.B) {
	for b.Loop() {
		var logs []string
		for range 50000 {
			logs = append(logs, "line")
			if len(logs) > 5000 {
				logs = append([]string(nil), logs[len(logs)-5000:]...)
			}
		}
		_ = logs
	}
}

func TestAppendIsCheap(t *testing.T) {
	r := testing.Benchmark(BenchmarkAppend50k)
	// Instrumented builds (race detector, coverage) are several times slower;
	// the time budget applies to normal builds, the allocation budget always.
	if ns := r.NsPerOp(); ns > 10_000_000 && !raceEnabled && testing.CoverMode() == "" {
		t.Errorf("50k appends took %d ns/op, want <= 10000000", ns)
	}
	if alloc := r.AllocedBytesPerOp(); alloc > 10<<20 {
		t.Errorf("50k appends allocated %d bytes/op, want <= %d", alloc, 10<<20)
	}
}
