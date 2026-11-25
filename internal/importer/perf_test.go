package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// perfFiles is the card size used by the import benchmarks.
const perfFiles = 2000

// countSyncs replaces the journal/index fsync hook with a counting wrapper and
// restores it when the test or benchmark finishes.
func countSyncs(t testing.TB) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := syncFile
	syncFile = func(f *os.File) error {
		n.Add(1)
		return orig(f)
	}
	t.Cleanup(func() { syncFile = orig })
	return &n
}

// countStats does the same for the destination-probe stat hook.
func countStats(t testing.TB) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := statCand
	statCand = func(name string) (os.FileInfo, error) {
		n.Add(1)
		return orig(name)
	}
	t.Cleanup(func() { statCand = orig })
	return &n
}

// makeSmallCard writes a card of perfFiles small JPEGs with distinct content
// and a fixed mtime, so imports never touch EXIF.
func makeSmallCard(b *testing.B, root string) string {
	b.Helper()
	src := filepath.Join(root, "card")
	for i := 0; i < perfFiles; i++ {
		name := filepath.Join(src, fmt.Sprintf("IMG_%04d.jpg", i))
		writeBytes(b, name, blob(4096+i%7, byte(i)), t1)
	}
	return src
}

// BenchmarkFreshImport measures a first import of a fresh card, including the
// per-file journal and index fsync cost.
func BenchmarkFreshImport(b *testing.B) {
	root := b.TempDir()
	src := makeSmallCard(b, root)
	syncs := countSyncs(b)
	for b.Loop() {
		dst, err := os.MkdirTemp(root, "out")
		if err != nil {
			b.Fatal(err)
		}
		o := baseOpts(src, dst)
		o.Verify = false
		o.Journal = filepath.Join(dst, "journal")
		syncs.Store(0)
		s := run(b, o)
		if s.Copied != perfFiles {
			b.Fatalf("copied %d, want %d", s.Copied, perfFiles)
		}
	}
	b.ReportMetric(float64(syncs.Load())/perfFiles, "fsyncs/file")
}

// BenchmarkReimportNoIndex measures a re-run over an unchanged card with the
// destination index disabled, i.e. the mtime/size skip path.
func BenchmarkReimportNoIndex(b *testing.B) {
	root := b.TempDir()
	src := makeSmallCard(b, root)
	dst := filepath.Join(root, "out")
	o := baseOpts(src, dst)
	o.Verify = false
	o.NoIndex = true
	o.Journal = filepath.Join(root, "journal")
	run(b, o)
	syncs := countSyncs(b)
	for b.Loop() {
		syncs.Store(0)
		s := run(b, o)
		if s.Skipped != perfFiles {
			b.Fatalf("skipped %d, want %d", s.Skipped, perfFiles)
		}
	}
	b.ReportMetric(float64(syncs.Load())/perfFiles, "fsyncs/file")
}

// BenchmarkSameNameCollisions measures the cost of resolving many identical
// base names landing on the same templated date directory.
func BenchmarkSameNameCollisions(b *testing.B) {
	const n = 500
	root := b.TempDir()
	src := filepath.Join(root, "card")
	for i := 0; i < n; i++ {
		p := filepath.Join(src, fmt.Sprintf("d%03d", i), "IMG.jpg")
		writeBytes(b, p, blob(1000+i, byte(i)), t1)
	}
	stats := countStats(b)
	for b.Loop() {
		dst, err := os.MkdirTemp(root, "out")
		if err != nil {
			b.Fatal(err)
		}
		o := baseOpts(src, dst)
		o.Verify = false
		o.NoIndex = true
		stats.Store(0)
		s := run(b, o)
		if s.Copied != n {
			b.Fatalf("copied %d, want %d", s.Copied, n)
		}
	}
	b.ReportMetric(float64(stats.Load())/n, "stats/file")
}

// Same-named sources cost O(1) stats each, and a source identical to one
// written earlier in the same run is still recognised among the collisions.
func TestCollisionStatsAndDups(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
	const n = 100
	for i := range n {
		writeBytes(t, filepath.Join(src, fmt.Sprintf("d%03d", i), "IMG.jpg"), blob(1000+i, byte(i)), t1)
	}
	writeBytes(t, filepath.Join(src, "z999", "IMG.jpg"), blob(1000+7, 7), t1) // same as d007
	o := baseOpts(src, dst)
	o.Verify, o.NoIndex, o.Workers = false, true, 1
	stats := countStats(t)
	s := run(t, o)
	if s.Copied != n || s.Skipped != 1 {
		t.Fatalf("%+v", s)
	}
	if got := stats.Load(); got > 3*(n+1) {
		t.Errorf("%d stats for %d same-named files", got, n+1)
	}
}
