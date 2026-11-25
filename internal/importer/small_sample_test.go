package importer

import (
	"bytes"
	"crypto/sha256"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRandom writes n pseudo-random bytes (fixed seed) to dir/name and
// returns the data.
func writeRandom(t *testing.T, dir, name string, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < len(data); i += 8 {
		v := r.Uint64()
		for j := 0; j < 8 && i+j < len(data); j++ {
			data[i+j] = byte(v >> (8 * j))
		}
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

// copyDigests copies src to dst with copyFile and returns its digests.
func copyDigests(t *testing.T, src, dst string, known []byte) digests {
	t.Helper()
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	noop := func(int64) {}
	d, err := copyFile(src, dst, info, now, now,
		func() error { return nil }, noop, func() error { return nil }, known)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSmallFileSampleIsSHA(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.bin")
	data := writeRandom(t, dir, "a.bin", 4096)
	d := copyDigests(t, src, filepath.Join(dir, "b.bin"), nil)
	want := sha256.Sum256(data)
	if !bytes.Equal(d.sha, want[:]) {
		t.Errorf("sha = %x, want %x", d.sha, want[:])
	}
	if !bytes.Equal(d.sample, want[:]) {
		t.Errorf("sample = %x, want %x", d.sample, want[:])
	}
	got, _, err := sampleDigest(src, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, d.sample) {
		t.Errorf("sampleDigest = %x, copy sample = %x", got, d.sample)
	}
}

func TestLargeFileKeepsSampleDigest(t *testing.T) {
	dir := t.TempDir()
	size := 4*sampleSize + 123
	src := filepath.Join(dir, "a.bin")
	data := writeRandom(t, dir, "a.bin", int(size))
	d := copyDigests(t, src, filepath.Join(dir, "b.bin"), nil)
	want := sha256.Sum256(data)
	if !bytes.Equal(d.sha, want[:]) {
		t.Errorf("sha = %x, want %x", d.sha, want[:])
	}
	got, _, err := sampleDigest(src, int64(size))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.sample, got) {
		t.Errorf("sample = %x, sampleDigest = %x", d.sample, got)
	}
	if bytes.Equal(d.sample, d.sha) {
		t.Error("large file's sample digest equals its SHA-256")
	}
}

func TestSmallFileHashesOnce(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.bin")
	data := writeRandom(t, dir, "a.bin", 4096)
	dst := filepath.Join(dir, "b.bin")
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	allocs := func(known []byte) float64 {
		return testing.AllocsPerRun(20, func() {
			now := time.Now()
			_, _ = copyFile(src, dst, info, now, now,
				func() error { return nil }, func(int64) {}, func() error { return nil }, known)
		})
	}
	withSHA := allocs(nil)
	withKnown := allocs(sum[:])
	if withSHA >= withKnown {
		t.Errorf("allocs without known sum = %v, with known sum = %v: a small copy should run one hasher, not the sample hasher and SHA-256 both", withSHA, withKnown)
	}
}
