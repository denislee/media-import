package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
