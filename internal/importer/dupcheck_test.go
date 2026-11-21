package importer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denislee/media-import/internal/media"
)

// blob returns n bytes of deterministic, non-repeating-looking content.
func blob(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed ^ byte(i>>9)
	}
	return b
}

func writeBytes(t testing.TB, p string, data []byte, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// dupOpts imports from src to dst with the index disabled, so only the
// templated-path check is exercised.
func dupOpts(src, dst, mode string) Options {
	o := baseOpts(src, dst)
	o.Verify = false
	o.DupCheck = mode
	o.NoIndex = true
	return o
}

// prepDup creates a source file and a destination file at its templated path
// (2021/06/07/IMG.jpg; mtime t1 is the fallback capture date).
func prepDup(t *testing.T, srcData, dstData []byte, dstMtime time.Time) (src, dst string) {
	root := t.TempDir()
	src, dst = filepath.Join(root, "card"), filepath.Join(root, "out")
	writeBytes(t, filepath.Join(src, "IMG.jpg"), srcData, t1)
	writeBytes(t, filepath.Join(dst, "2021/06/07/IMG.jpg"), dstData, dstMtime)
	return
}

func run(t testing.TB, o Options) Summary {
	t.Helper()
	sum, err := Run(context.Background(), o, nil, nil)
	if err != nil || sum.Failed != 0 {
		t.Fatalf("run: %v %+v", err, sum.Progress)
	}
	return sum
}

func TestFastPathSampledTradeoff(t *testing.T) {
	data := blob(1<<20, 1)
	other := bytes.Clone(data)
	other[300_000] ^= 0xff // outside the three sampled blocks
	for _, tc := range []struct {
		mode      string
		wantSkip  int
		wantQuick int
	}{{DupCheckFast, 1, 1}, {DupCheckFull, 0, 0}} {
		src, dst := prepDup(t, data, other, t1)
		sum := run(t, dupOpts(src, dst, tc.mode))
		if sum.Skipped != tc.wantSkip || sum.DupQuick != tc.wantQuick {
			t.Errorf("%s: %+v", tc.mode, sum.Progress)
		}
		if tc.mode == DupCheckFast && sum.DupCheckBytes > 2*3*sampleSize {
			t.Errorf("fast path read %d bytes", sum.DupCheckBytes)
		}
	}
}

func TestFastPathSampleMismatchCopies(t *testing.T) {
	data := blob(1<<20, 1)
	other := bytes.Clone(data)
	other[len(other)-10] ^= 0xff // inside the last sampled block
	src, dst := prepDup(t, data, other, t1)
	sum := run(t, dupOpts(src, dst, DupCheckFast))
	if sum.Copied != 1 || sum.Skipped != 0 {
		t.Fatalf("%+v", sum.Progress)
	}
	exists(t, filepath.Join(dst, "2021/06/07/IMG_1.jpg"))
}

func TestMtimeMismatchFallsBackToFull(t *testing.T) {
	data := blob(1<<20, 2)
	other := bytes.Clone(data)
	other[300_000] ^= 0xff
	far := t1.Add(time.Hour)

	src, dst := prepDup(t, data, data, far)
	sum := run(t, dupOpts(src, dst, DupCheckFast))
	if sum.Skipped != 1 || sum.DupFull != 1 || sum.DupQuick != 0 {
		t.Errorf("identical: %+v", sum.Progress)
	}

	src, dst = prepDup(t, data, other, far)
	sum = run(t, dupOpts(src, dst, DupCheckFast))
	if sum.Copied != 1 || sum.Skipped != 0 {
		t.Errorf("differing: %+v", sum.Progress)
	}
}

func TestMtimeTolerance(t *testing.T) {
	data := blob(1<<20, 3)
	for _, tc := range []struct {
		off   time.Duration
		quick bool
	}{{1900 * time.Millisecond, true}, {-2 * time.Second, true}, {3 * time.Second, false}} {
		src, dst := prepDup(t, data, data, t1.Add(tc.off))
		sum := run(t, dupOpts(src, dst, DupCheckFast))
		if sum.Skipped != 1 || (sum.DupQuick == 1) != tc.quick {
			t.Errorf("offset %v: %+v", tc.off, sum.Progress)
		}
	}
}

// A copy made with MtimeCapture on is recognised by a run with it off, and
// the reverse, through the second expected mtime.
func TestMtimeCaptureEitherWay(t *testing.T) {
	data := blob(1<<20, 4)
	capture := time.Date(2020, 1, 2, 3, 4, 5, 0, time.Local)
	srcMtime := capture.Add(48 * time.Hour)
	for _, firstCapture := range []bool{true, false} {
		root := t.TempDir()
		src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
		writeBytes(t, filepath.Join(src, "IMG_20200102_030405.jpg"), data, srcMtime)
		o := dupOpts(src, dst, DupCheckFast)
		o.MtimeCapture = firstCapture
		if s := run(t, o); s.Copied != 1 {
			t.Fatalf("first run %+v", s.Progress)
		}
		o.MtimeCapture = !firstCapture
		if s := run(t, o); s.Skipped != 1 || s.DupQuick != 1 {
			t.Errorf("capture=%v then %v: %+v", firstCapture, !firstCapture, s.Progress)
		}
	}
}

// Several same-size candidates: the source is hashed once after the first
// full compare rather than re-read for every candidate.
func TestSeveralSameSizeCandidates(t *testing.T) {
	data := blob(1<<20, 5)
	src, dst := prepDup(t, data, blob(1<<20, 6), t1.Add(time.Hour))
	writeBytes(t, filepath.Join(dst, "2021/06/07/IMG_1.jpg"), blob(1<<20, 7), t1.Add(time.Hour))
	writeBytes(t, filepath.Join(dst, "2021/06/07/IMG_2.jpg"), data, t1.Add(time.Hour))
	sum := run(t, dupOpts(src, dst, DupCheckFast))
	if sum.Skipped != 1 || sum.DupFull != 1 {
		t.Fatalf("%+v", sum.Progress)
	}
	if sum.Bytes != sum.TotalBytes {
		t.Errorf("progress %d of %d", sum.Bytes, sum.TotalBytes)
	}
}

// Many workers importing files that all map to the same destination name must
// never be handed the same path.
func TestConcurrentCollisions(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
	const n = 60
	for i := range n {
		writeBytes(t, filepath.Join(src, fmt.Sprintf("d%02d", i), "IMG.jpg"), blob(1000+i, byte(i)), t1)
	}
	o := dupOpts(src, dst, DupCheckFast)
	o.Workers = 16
	if s := run(t, o); s.Copied != n {
		t.Fatalf("%+v", s.Progress)
	}
	ents, err := os.ReadDir(filepath.Join(dst, "2021/06/07"))
	if err != nil || len(ents) != n {
		t.Fatalf("%d files in destination (%v)", len(ents), err)
	}
	// A second concurrent run finds every one of them.
	if s := run(t, o); s.Skipped != n || s.Copied != 0 {
		t.Fatalf("rerun %+v", s.Progress)
	}
}

func countCaptureTime(t *testing.T) *atomic.Int64 {
	var n atomic.Int64
	orig := captureTime
	captureTime = func(p string, k media.Kind, fi os.FileInfo, o media.DateOptions) (time.Time, media.DateSource) {
		n.Add(1)
		return orig(p, k, fi, o)
	}
	t.Cleanup(func() { captureTime = orig })
	return &n
}

func TestIndexSkipsDateAndContent(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	run(t, o)
	calls := countCaptureTime(t)
	sum := run(t, o)
	if sum.Skipped != 3 || sum.DupIndexed != 3 {
		t.Fatalf("%+v", sum.Progress)
	}
	if calls.Load() != 0 {
		t.Errorf("CaptureTime called %d times on an indexed re-import", calls.Load())
	}
	if sum.DupCheckBytes != 0 {
		t.Errorf("read %d bytes", sum.DupCheckBytes)
	}
	if sum.Bytes != sum.TotalBytes {
		t.Errorf("progress %d of %d", sum.Bytes, sum.TotalBytes)
	}
}

func TestIndexStaleEntries(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	run(t, o)
	a := filepath.Join(dst, "2021/06/07/A.jpg")

	// Destination modified (different mtime): falls back to the normal check,
	// which finds it differs and copies again.
	writeBytes(t, a, []byte("xyz"), t2)
	sum := run(t, o)
	if sum.Copied != 1 || sum.DupIndexed != 2 {
		t.Fatalf("modified: %+v", sum.Progress)
	}

	// Destination deleted: copied again.
	for _, p := range []string{a, filepath.Join(dst, "2021/06/07/A_2.jpg")} {
		os.Remove(p)
	}
	sum = run(t, o)
	if sum.Copied != 1 {
		t.Fatalf("deleted: %+v", sum.Progress)
	}
}

func TestIndexCorruptIgnored(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	writeBytes(t, IndexPath(dst), []byte("garbage\nF\t\"x\"\t1\nF\t\"A.JPG\"\t3\t1\t\"../escape\"\t1\t-\nF\t\"A.JPG\"\t3\tzz\t\"a\"\t1\t-\n"), t1)
	if s := run(t, o); s.Copied != 3 {
		t.Fatalf("%+v", s.Progress)
	}
	// Torn last line after the valid records.
	f, _ := os.OpenFile(IndexPath(dst), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("F\t\"IMG")
	f.Close()
	if s := run(t, o); s.Skipped != 3 || s.DupIndexed != 3 {
		t.Fatalf("%+v", s.Progress)
	}
}

func TestIndexFullModeComparesContent(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	run(t, o)
	o.DupCheck = DupCheckFull
	sum := run(t, o)
	if sum.Skipped != 3 || sum.DupFull != 3 || sum.DupIndexed != 0 {
		t.Fatalf("%+v", sum.Progress)
	}
}

func TestDuplicateUnderOtherName(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	run(t, o)
	// Change the layout: without DupAnyName everything is copied again.
	o.DirTemplate = "{YYYY}"
	o.DupAnyName = true
	// Hide the index's source keys so only the checksum lookup can match.
	idx, _ := os.ReadFile(IndexPath(dst))
	writeBytes(t, IndexPath(dst), []byte(strings.ReplaceAll(string(idx), "F\t\"", "F\t\"x")), t1)
	sum := run(t, o)
	if sum.Skipped != 3 || sum.DupOtherName != 3 || sum.Copied != 0 {
		t.Fatalf("%+v", sum.Progress)
	}
	if _, err := os.Stat(filepath.Join(dst, "2021")); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Join(dst, "2021")); len(ents) != 1 { // only the old 06/ dir
		t.Fatalf("new files written: %v", ents)
	}
}

func TestScanSkipsIndexDir(t *testing.T) {
	src, dst := setup(t)
	writeBytes(t, filepath.Join(src, IndexDir, "x.jpg"), []byte("x"), t1)
	o := baseOpts(src, dst)
	o.IncludeHidden = true
	if s := run(t, o); s.Total != 4 {
		t.Fatalf("%+v", s.Progress)
	}
}

func TestDryRunDoesNotWriteIndex(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	o.DryRun = true
	run(t, o)
	if _, err := os.Stat(IndexPath(dst)); !os.IsNotExist(err) {
		t.Fatal("dry run wrote the index")
	}
}

func TestSampledSame(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	d := blob(4*sampleSize+17, 9)
	writeBytes(t, a, d, t1)
	writeBytes(t, b, d, t1)
	if ok, n, err := sampledSame(a, b, int64(len(d))); !ok || err != nil || n != 6*sampleSize {
		t.Fatal(ok, n, err)
	}
}

func TestSampledSameMiddleDiffers(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	d := blob(4*sampleSize+17, 9)
	d2 := bytes.Clone(d)
	d2[len(d)/2] ^= 0xff
	writeBytes(t, a, d, t1)
	writeBytes(t, b, d2, t1)
	if ok, _, err := sampledSame(a, b, int64(len(d))); ok || err != nil {
		t.Fatal(ok, err)
	}
}

// BenchmarkReimport measures re-importing a card whose files are all in the
// destination already, per duplicate-check method. Run with
//
//	go test ./internal/importer -bench Reimport -benchtime 3x
func BenchmarkReimport(b *testing.B) {
	root := b.TempDir()
	src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
	const files, size = 40, 4 << 20
	for i := range files {
		writeBytes(b, filepath.Join(src, fmt.Sprintf("IMG_%04d.jpg", i)), blob(size, byte(i)), t1)
	}
	o := baseOpts(src, dst)
	o.Verify = false
	run(b, o)
	for _, mode := range []struct {
		name    string
		check   string
		noIndex bool
	}{{"full", DupCheckFull, true}, {"fast", DupCheckFast, true}, {"indexed", DupCheckFast, false}} {
		b.Run(mode.name, func(b *testing.B) {
			o := o
			o.DupCheck, o.NoIndex = mode.check, mode.noIndex
			var read int64
			for b.Loop() {
				s := run(b, o)
				if s.Skipped != files {
					b.Fatalf("%+v", s.Progress)
				}
				read = s.DupCheckBytes
			}
			b.ReportMetric(float64(read)/(1<<20), "MiB-read/op")
		})
	}
}
