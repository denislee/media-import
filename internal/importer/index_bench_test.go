package importer

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func BenchmarkIndexLoad100k(b *testing.B) {
	dest := b.TempDir()
	p := IndexPath(dest)
	os.MkdirAll(filepath.Dir(p), 0o755)
	f, _ := os.Create(p)
	w := bufio.NewWriter(f)
	sha := make([]byte, 32)
	sample := make([]byte, 32)
	for i := range sha {
		sha[i] = byte(i)
		sample[i] = byte(32 + i)
	}
	for i := range 100000 {
		e := idxEntry{key: idxKey{fmt.Sprintf("IMG_%05d.JPG", i), int64(3_000_000 + i), time.Now().UnixNano()},
			rel: fmt.Sprintf("2024/07/%02d/IMG_%05d.jpg", i%28+1, i), dmtime: time.Now().UnixNano(), sha: sha, sample: sample}
		w.WriteString(e.line())
		w.WriteByte('\n')
	}
	w.Flush()
	f.Close()
	st, _ := os.Stat(p)
	b.ReportMetric(float64(st.Size())/(1<<20), "MiB-file")
	b.ReportAllocs()
	var ms runtime.MemStats
	for b.Loop() {
		ix, err := openIndex(dest, true)
		if err != nil || ix.liveCount() != 100000 {
			b.Fatal(err)
		}
		runtime.GC()
		runtime.ReadMemStats(&ms)
		b.ReportMetric(float64(ms.HeapAlloc)/(1<<20), "MiB-heap-live")
		runtime.KeepAlive(ix)
	}
}

func TestParseIndexLineRoundTrip(t *testing.T) {
	sha := make([]byte, 32)
	sample := make([]byte, 32)
	for i := range sha {
		sha[i] = byte(i)
		sample[i] = byte(i + 1)
	}
	cases := []struct {
		name   string
		src    string
		sha    []byte
		sample []byte
	}{
		{"no digests", "IMG_0001.JPG", nil, nil},
		{"sha only", "a\t\"b.jpg", sha, nil},
		{"both digests", "IMG_0002.JPG", sha, sample},
	}
	for _, tc := range cases {
		e := &idxEntry{
			key:    idxKey{tc.src, 1234, 5678},
			rel:    "2024/07/01/IMG_0001.jpg",
			dmtime: 9999,
			sha:    tc.sha,
			sample: tc.sample,
		}
		got, ok := parseIndexLine(e.line())
		if !ok {
			t.Fatalf("%s: parseIndexLine rejected %q", tc.name, e.line())
		}
		if got.key != e.key || got.rel != e.rel || got.dmtime != e.dmtime {
			t.Errorf("%s: got key=%+v rel=%q dmtime=%d, want key=%+v rel=%q dmtime=%d",
				tc.name, got.key, got.rel, got.dmtime, e.key, e.rel, e.dmtime)
		}
		if (got.sha == nil) != (tc.sha == nil) || !bytes.Equal(got.sha, tc.sha) {
			t.Errorf("%s: got sha=%x (nil=%v), want %x (nil=%v)",
				tc.name, got.sha, got.sha == nil, tc.sha, tc.sha == nil)
		}
		if (got.sample == nil) != (tc.sample == nil) || !bytes.Equal(got.sample, tc.sample) {
			t.Errorf("%s: got sample=%x (nil=%v), want %x (nil=%v)",
				tc.name, got.sample, got.sample == nil, tc.sample, tc.sample == nil)
		}
	}

	good := (&idxEntry{
		key: idxKey{"IMG_0003.JPG", 1234, 5678}, rel: "2024/07/01/IMG_0003.jpg",
		dmtime: 9999, sha: sha, sample: sample,
	}).line()
	if _, ok := parseIndexLine(good + "\textra"); ok {
		t.Error("parseIndexLine accepted a line with 9 fields")
	}
	shortSha := fmt.Sprintf("F\t\"IMG_0004.JPG\"\t1\t2\t\"a.jpg\"\t3\t%063d", 0)
	if _, ok := parseIndexLine(shortSha); ok {
		t.Error("parseIndexLine accepted a 63-char sha")
	}
	for _, long := range []string{
		fmt.Sprintf("F\t\"x\"\t1\t2\t\"a.jpg\"\t3\t%066d", 0),    // sha
		fmt.Sprintf("F\t\"x\"\t1\t2\t\"a.jpg\"\t3\t-\t%066d", 0), // sample
	} {
		if _, ok := parseIndexLine(long); ok { // and must not panic
			t.Errorf("parseIndexLine accepted an over-long digest: %q", long)
		}
	}
}
