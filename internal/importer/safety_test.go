package importer

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/denislee/media-import/internal/media"
)

// destFiles maps each regular file under dst (relative path → content), with
// the destination index directory left out.
func destFiles(t *testing.T, dst string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if _, err := os.Stat(dst); errors.Is(err, fs.ErrNotExist) {
		return out
	}
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == indexDir {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(dst, p)
		if rerr != nil {
			return rerr
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// logRecorder collects the importer's log lines; Run calls logf from worker
// goroutines.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) logf(s string) {
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

func (l *logRecorder) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// writeExternal writes a file from inside a crashPoint callback, which runs on
// a worker goroutine where t.Fatal must not be used.
func writeExternal(t *testing.T, p, data string) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Error(err)
		return
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Error(err)
	}
}

// TestTwoRunsSameDest starts a second run against a destination the first one
// still holds: it must be refused rather than pick the same free names, and the
// lock must be gone once the first run returns.
func TestTwoRunsSameDest(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "out")
	srcA := filepath.Join(root, "carda")
	srcB := filepath.Join(root, "cardb")
	write(t, filepath.Join(srcA, "DSC_0001.JPG"), "card one", t1)
	write(t, filepath.Join(srcB, "DSC_0001.JPG"), "card two!", t1)

	orig := crashPoint
	t.Cleanup(func() { crashPoint = orig })
	// Not a sync.Once: without the lock, run B reaches this hook itself, and a
	// re-entered Once would deadlock instead of failing the test.
	var started atomic.Bool
	var errB error
	crashPoint = func(p string) error {
		if p == "before-rename" && started.CompareAndSwap(false, true) {
			b := baseOpts(srcB, dst)
			b.Kinds = map[media.Kind]bool{media.Image: true}
			_, errB = Run(context.Background(), b, nil, nil)
		}
		return nil
	}

	a := baseOpts(srcA, dst)
	a.Kinds = map[media.Kind]bool{media.Image: true}
	sumA, errA := Run(context.Background(), a, nil, nil)
	if errA != nil {
		t.Fatalf("first run: %v %+v", errA, sumA)
	}
	if sumA.Copied != 1 {
		t.Fatalf("first run copied %d files, want 1: %+v", sumA.Copied, sumA)
	}
	if !errors.Is(errB, ErrDestBusy) {
		t.Fatalf("second run during the first: error %v, want ErrDestBusy", errB)
	}
	files := destFiles(t, dst)
	if len(files) != 1 {
		t.Fatalf("destination has %d files, want 1: %v", len(files), files)
	}
	for rel, content := range files {
		if content != "card one" {
			t.Errorf("%s = %q, want %q", rel, content, "card one")
		}
	}
	if _, err := os.Stat(filepath.Join(srcB, "DSC_0001.JPG")); err != nil {
		t.Errorf("refused run damaged its source: %v", err)
	}

	// The first run returned, so its lock is released: the same run now works.
	b := baseOpts(srcB, dst)
	b.Kinds = map[media.Kind]bool{media.Image: true}
	if sumB, err := Run(context.Background(), b, nil, nil); err != nil {
		t.Fatalf("second run after the first returned: %v %+v", err, sumB)
	}
	if got := destFiles(t, dst); len(got) != 2 {
		t.Fatalf("destination after both runs has %d files, want 2: %v", len(got), got)
	}
}

// TestExternalFileNotOverwritten puts a file where the import is about to
// rename its copy: the existing file must survive and the copy must land under
// another name.
func TestExternalFileNotOverwritten(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
	write(t, filepath.Join(src, "A.JPG"), "from the card", t1)
	opt := baseOpts(src, dst)
	opt.DirTemplate = ""
	opt.NoIndex = true

	orig := crashPoint
	t.Cleanup(func() { crashPoint = orig })
	var once sync.Once
	crashPoint = func(p string) error {
		if p == "before-rename" {
			once.Do(func() {
				writeExternal(t, filepath.Join(dst, "A.jpg"), "someone else's photo")
			})
		}
		return nil
	}

	var log logRecorder
	sum, err := Run(context.Background(), opt, nil, log.logf)
	if err != nil {
		t.Fatalf("run: %v %+v", err, sum)
	}
	if sum.Copied != 1 || sum.Failed != 0 {
		t.Fatalf("summary %+v, want 1 copied and 0 failed", sum)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "A.jpg")); err != nil || string(got) != "someone else's photo" {
		t.Fatalf("external A.jpg = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "A_1.jpg")); err != nil || string(got) != "from the card" {
		t.Fatalf("imported A_1.jpg = %q, %v", got, err)
	}
	for rel := range destFiles(t, dst) {
		if strings.HasSuffix(rel, ".part") {
			t.Errorf("leftover temp file %s", rel)
		}
	}
	if !log.contains("appeared while copying") {
		t.Error("no log line mentions a file that appeared while copying")
	}
}

// TestExternalFileNotReplacedBySameDeviceMove covers the same-device move path:
// a rename must not replace a file that appeared after the name was reserved.
func TestExternalFileNotReplacedBySameDeviceMove(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
	write(t, filepath.Join(src, "A.JPG"), "from the card", t1)
	opt := baseOpts(src, dst)
	opt.DirTemplate = ""
	opt.NoIndex = true
	opt.Move = true

	orig := crashPoint
	t.Cleanup(func() { crashPoint = orig })
	var once sync.Once
	crashPoint = func(p string) error {
		if p == "begun" {
			once.Do(func() {
				writeExternal(t, filepath.Join(dst, "A.jpg"), "someone else's photo")
			})
		}
		return nil
	}

	var log logRecorder
	sum, err := Run(context.Background(), opt, nil, log.logf)
	if err != nil {
		t.Fatalf("run: %v %+v", err, sum)
	}
	if sum.Failed != 0 {
		t.Fatalf("summary %+v, want 0 failed", sum)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "A.jpg")); err != nil || string(got) != "someone else's photo" {
		t.Fatalf("external A.jpg = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "A_1.jpg")); err != nil || string(got) != "from the card" {
		t.Fatalf("moved A_1.jpg = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(src, "A.JPG")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source still there after the move: %v", err)
	}
}

// TestMoveOfGrowingSourceKeepsSource moves a file that grows during the import:
// the short copy must be dropped and the source left in place.
func TestMoveOfGrowingSourceKeepsSource(t *testing.T) {
	d, err := os.MkdirTemp("/dev/shm", "mi-test-")
	if err != nil {
		t.Skipf("no second filesystem for a cross-device move: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	dst := filepath.Join(d, "out")

	root := t.TempDir()
	srcDir := filepath.Join(root, "card")
	src := filepath.Join(srcDir, "V.MP4")
	write(t, src, "first half;", t1)

	opt := Options{
		Sources:     []string{srcDir},
		Dest:        dst,
		DirTemplate: "{YYYY}/{MM}/{DD}",
		Kinds:       map[media.Kind]bool{media.Video: true},
		Move:        true,
		Verify:      true,
	}

	orig := crashPoint
	t.Cleanup(func() { crashPoint = orig })
	var once sync.Once
	crashPoint = func(p string) error {
		if p == "before-rename" {
			once.Do(func() {
				f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := f.Write([]byte("second half")); err != nil {
					t.Error(err)
				}
				if err := f.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		return nil
	}

	sum, err := Run(context.Background(), opt, nil, nil)
	if err != nil {
		t.Fatalf("run: %v %+v", err, sum)
	}
	if sum.Failed != 1 || sum.Copied != 0 {
		t.Fatalf("summary %+v, want 1 failed and 0 copied", sum)
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != "first half;second half" {
		t.Fatalf("source = %q, %v; want it intact", got, err)
	}
	for rel := range destFiles(t, dst) {
		if strings.HasSuffix(rel, ".mp4") {
			t.Errorf("a short copy was installed at %s", rel)
		}
	}
}

// TestCopyFileDetectsGrowthDuringRead checks that copyFile refuses a file that
// grew while it was being read, leaving neither the copy nor its temp file.
func TestCopyFileDetectsGrowthDuringRead(t *testing.T) {
	tmp := t.TempDir()
	data := bytes.Repeat([]byte("a"), 2<<20)
	src := filepath.Join(tmp, "src.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(outDir, "dst.bin")

	calls := 0
	wait := func() error {
		calls++
		if calls == 2 {
			f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Error(err)
				return nil
			}
			if _, err := f.Write([]byte("x")); err != nil {
				t.Error(err)
			}
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	if _, err := copyFile(src, dst, info, info.ModTime(), info.ModTime(), wait,
		func(int64) {}, func() error { return nil }, nil); !errors.Is(err, errSourceChanged) {
		t.Fatalf("copyFile of a growing source: %v, want errSourceChanged", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a short copy was left at %s: %v", dst, err)
	}
	if _, err := os.Stat(PartPath(dst)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("temp file %s was left behind: %v", PartPath(dst), err)
	}

	// Control: the same copy of a file that does not change succeeds.
	src2 := filepath.Join(tmp, "src2.bin")
	if err := os.WriteFile(src2, data, 0o644); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(src2)
	if err != nil {
		t.Fatal(err)
	}
	dst2 := filepath.Join(outDir, "dst2.bin")
	if _, err := copyFile(src2, dst2, info2, info2.ModTime(), info2.ModTime(),
		func() error { return nil }, func(int64) {}, func() error { return nil }, nil); err != nil {
		t.Fatalf("copyFile of an unchanged source: %v", err)
	}
	got, err := os.ReadFile(dst2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("control copy differs from its source (%d bytes, want %d)", len(got), len(data))
	}
}

// TestRenameNoReplace checks the rename, hard-link and lstat fallbacks never
// replace an existing destination.
func TestRenameNoReplace(t *testing.T) {
	origRename, origLink := renameat2, linkFile
	t.Cleanup(func() { renameat2, linkFile = origRename, origLink })

	newPair := func(t *testing.T) (old, target string) {
		t.Helper()
		dir := t.TempDir()
		old = filepath.Join(dir, "old")
		target = filepath.Join(dir, "new")
		write(t, old, "old data", t1)
		return old, target
	}

	t.Run("free", func(t *testing.T) {
		old, target := newPair(t)
		if err := renameNoReplace(old, target); err != nil {
			t.Fatalf("renameNoReplace: %v", err)
		}
		if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("old file still there: %v", err)
		}
		if b, err := os.ReadFile(target); err != nil || string(b) != "old data" {
			t.Errorf("new file = %q, %v", b, err)
		}
	})

	t.Run("taken", func(t *testing.T) {
		old, target := newPair(t)
		write(t, target, "existing", t1)
		if err := renameNoReplace(old, target); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("renameNoReplace onto an existing file: %v, want fs.ErrExist", err)
		}
		if b, err := os.ReadFile(target); err != nil || string(b) != "existing" {
			t.Errorf("target = %q, %v; want it untouched", b, err)
		}
		if b, err := os.ReadFile(old); err != nil || string(b) != "old data" {
			t.Errorf("old = %q, %v; want it left in place", b, err)
		}
	})

	t.Run("link fallback", func(t *testing.T) {
		renameat2 = func(string, string) error { return unix.EINVAL }
		t.Cleanup(func() { renameat2 = origRename })

		old, target := newPair(t)
		if err := renameNoReplace(old, target); err != nil {
			t.Fatalf("free target through the link fallback: %v", err)
		}
		if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("old file still there: %v", err)
		}
		if b, err := os.ReadFile(target); err != nil || string(b) != "old data" {
			t.Errorf("new file = %q, %v", b, err)
		}

		old2, target2 := newPair(t)
		write(t, target2, "existing", t1)
		if err := renameNoReplace(old2, target2); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("taken target through the link fallback: %v, want fs.ErrExist", err)
		}
		if b, err := os.ReadFile(target2); err != nil || string(b) != "existing" {
			t.Errorf("target = %q, %v; want it untouched", b, err)
		}
		if b, err := os.ReadFile(old2); err != nil || string(b) != "old data" {
			t.Errorf("old = %q, %v; want it left in place", b, err)
		}
	})

	t.Run("lstat fallback", func(t *testing.T) {
		renameat2 = func(string, string) error { return unix.EINVAL }
		linkFile = func(o, n string) error {
			return &os.LinkError{Op: "link", Old: o, New: n, Err: unix.EPERM}
		}
		t.Cleanup(func() { renameat2, linkFile = origRename, origLink })

		old, target := newPair(t)
		if err := renameNoReplace(old, target); err != nil {
			t.Fatalf("free target through the lstat fallback: %v", err)
		}
		if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("old file still there: %v", err)
		}
		if b, err := os.ReadFile(target); err != nil || string(b) != "old data" {
			t.Errorf("new file = %q, %v", b, err)
		}

		old2, target2 := newPair(t)
		write(t, target2, "existing", t1)
		if err := renameNoReplace(old2, target2); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("taken target through the lstat fallback: %v, want fs.ErrExist", err)
		}
		if b, err := os.ReadFile(target2); err != nil || string(b) != "existing" {
			t.Errorf("target = %q, %v; want it untouched", b, err)
		}
		if b, err := os.ReadFile(old2); err != nil || string(b) != "old data" {
			t.Errorf("old = %q, %v; want it left in place", b, err)
		}
	})
}

// TestChtimesFailureAfterMoveIsNotAFailure: a moved file whose date cannot be
// set is still imported, with a warning.
func TestChtimesFailureAfterMoveIsNotAFailure(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
	write(t, filepath.Join(src, "A.JPG"), "x", t1)
	opt := baseOpts(src, dst)
	opt.DirTemplate = ""
	opt.NoIndex = true
	opt.Move = true

	orig := chtimes
	chtimes = func(string, time.Time, time.Time) error { return errors.New("EPERM simulated") }
	t.Cleanup(func() { chtimes = orig })

	var log logRecorder
	sum, err := Run(context.Background(), opt, nil, log.logf)
	if err != nil {
		t.Fatalf("run: %v %+v", err, sum)
	}
	if sum.Copied != 1 || sum.Failed != 0 {
		t.Fatalf("summary %+v, want 1 copied and 0 failed", sum)
	}
	if _, err := os.Stat(filepath.Join(dst, "A.jpg")); err != nil {
		t.Fatalf("moved file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "A.JPG")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source still there: %v", err)
	}
	if !log.contains("date could not be set") {
		t.Error("no log line warns that the date could not be set")
	}
}

// TestLockFileTwice checks the destination lock is exclusive while held and
// released by Close.
func TestLockFileTwice(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "lock")
	f1, err := LockFile(p)
	if err != nil {
		t.Fatalf("first LockFile: %v", err)
	}
	if _, err := LockFile(p); !errors.Is(err, ErrLocked) {
		f1.Close()
		t.Fatalf("second LockFile: %v, want ErrLocked", err)
	}
	if err := f1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	f3, err := LockFile(p)
	if err != nil {
		t.Fatalf("LockFile after release: %v", err)
	}
	if err := f3.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
