package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// preallocate must never fail on filesystems without fallocate support; only a
// real out-of-space error is reported, so this has to be err == nil.
func TestPreallocateIgnoresUnsupported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := preallocate(f, 4096); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
}

func TestHumanSize(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1536, "1.5 KiB"},
		{3 << 30, "3.0 GiB"},
	} {
		if got := HumanSize(c.n); got != c.want {
			t.Errorf("HumanSize(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// The destination tree usually does not exist yet, so freeBytes has to walk up
// to an ancestor that does and report its free space.
func TestFreeBytesWalksUp(t *testing.T) {
	free, ok := freeBytes(filepath.Join(t.TempDir(), "a", "b", "c"))
	if !ok {
		t.Fatal("freeBytes did not find an existing ancestor")
	}
	if free <= 0 {
		t.Fatalf("freeBytes = %d, want > 0", free)
	}
}

// A destination with less free space than the run may copy logs a warning, but
// the import still goes ahead.
func TestLowSpaceWarning(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
	writeBytes(t, filepath.Join(src, "IMG.jpg"), blob(4096, 1), t1)

	orig := freeBytesFn
	freeBytesFn = func(string) (int64, bool) { return 1, true }
	defer func() { freeBytesFn = orig }()

	var (
		mu    sync.Mutex
		lines []string
	)
	logf := func(s string) {
		mu.Lock()
		lines = append(lines, s)
		mu.Unlock()
	}
	if _, err := Run(context.Background(), baseOpts(src, dst), nil, logf); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, s := range lines {
		if strings.HasPrefix(s, "WARN destination has") {
			return
		}
	}
	t.Fatalf("no low-space warning in %v", lines)
}

// stubFallocate replaces fallocate, counts the calls and returns ret from each.
func stubFallocate(t *testing.T, ret error) *int {
	t.Helper()
	calls := 0
	orig := fallocate
	fallocate = func(fd int, mode uint32, off int64, n int64) error {
		calls++
		return ret
	}
	t.Cleanup(func() { fallocate = orig })
	return &calls
}

// copyForTest copies a file of size zero bytes from src to dst, both inside
// dir, and returns the copy's error.
func copyForTest(t *testing.T, dir string, size int) error {
	t.Helper()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	if err := os.WriteFile(src, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = copyFile(src, dst, info, time.Now(), time.Now(), func() error { return nil }, func(int64) {}, func() error { return nil }, nil)
	return err
}

// Small copies do not pay for an extra fallocate syscall.
func TestNoPreallocateForSmallFiles(t *testing.T) {
	calls := stubFallocate(t, nil)
	if err := copyForTest(t, t.TempDir(), 4096); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("fallocate calls = %d, want 0", *calls)
	}
}

// A large copy that cannot fit fails before writing, leaving no .part file.
func TestPreallocateENOSPCFailsFast(t *testing.T) {
	calls := stubFallocate(t, unix.ENOSPC)
	dir := t.TempDir()
	err := copyForTest(t, dir, 2<<20)
	if *calls != 1 {
		t.Fatalf("fallocate calls = %d, want 1", *calls)
	}
	if err == nil {
		t.Fatal("copyFile succeeded, want an out-of-space error")
	}
	if !strings.Contains(err.Error(), "not enough space") {
		t.Fatalf("error = %v, want it to mention not enough space", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("temporary file %q left behind", e.Name())
		}
	}
}
