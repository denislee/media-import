package importer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// noFlusher stops the background group-commit flusher for the test, so only
// the fsyncs the code asks for happen.
func noFlusher(t *testing.T) {
	orig := groupCommitInterval
	groupCommitInterval = time.Hour
	t.Cleanup(func() { groupCommitInterval = orig })
}

func TestGroupCommitFsyncs(t *testing.T) {
	noFlusher(t)
	root := t.TempDir()
	src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
	const n = 40
	for i := range n {
		writeBytes(t, filepath.Join(src, fmt.Sprintf("IMG_%04d.jpg", i)), blob(5000+i, byte(i)), t1)
	}
	o := baseOpts(src, dst)
	o.Verify = false
	o.Journal = filepath.Join(root, "journal")
	syncs := countSyncs(t)
	if s := run(t, o); s.Copied != n {
		t.Fatalf("%+v", s)
	}
	// One shared fsync per copy at most (B before the rename), plus closing
	// the journal and the index.
	if got := syncs.Load(); got > n+2 {
		t.Errorf("fresh import: %d fsyncs for %d files", got, n)
	}
	syncs.Store(0)
	if s := run(t, o); s.Skipped != n {
		t.Fatalf("%+v", s)
	}
	if got := syncs.Load(); got > 2 {
		t.Errorf("re-import: %d fsyncs for %d skipped files, want only the closing ones", got, n)
	}
}

func TestGroupFileSharesSyncs(t *testing.T) {
	noFlusher(t)
	f, err := os.Create(filepath.Join(t.TempDir(), "j"))
	if err != nil {
		t.Fatal(err)
	}
	syncs := countSyncs(t)
	g := newGroupFile(f)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.syncTo(g.append(fmt.Sprint("rec ", i))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := g.close(); err != nil {
		t.Fatal(err)
	}
	if got := syncs.Load(); got < 1 || got > 64 {
		t.Fatalf("%d fsyncs", got)
	}
	data, _ := os.ReadFile(f.Name())
	if lines := strings.Count(string(data), "\n"); lines != 64 {
		t.Fatalf("%d lines written", lines)
	}
}

// durability simulates power loss: it tracks, per file, how much was fsynced,
// and can roll a file back to that length.
type durability struct {
	mu     sync.Mutex
	len    map[string]int64
	frozen bool // the crash happened: later fsyncs no longer count
}

func (d *durability) freeze() {
	d.mu.Lock()
	d.frozen = true
	d.mu.Unlock()
}

func trackDurability(t *testing.T) *durability {
	d := &durability{len: map[string]int64{}}
	orig := syncFile
	syncFile = func(f *os.File) error {
		err := orig(f)
		if st, serr := f.Stat(); err == nil && serr == nil {
			d.mu.Lock()
			if !d.frozen {
				d.len[f.Name()] = st.Size()
			}
			d.mu.Unlock()
		}
		return err
	}
	t.Cleanup(func() { syncFile = orig })
	return d
}

// powerLoss truncates p to what was durable.
func (d *durability) powerLoss(t *testing.T, p string) {
	d.mu.Lock()
	n := d.len[p]
	d.mu.Unlock()
	if err := os.Truncate(p, n); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func crashAt(t *testing.T, point string, d *durability, cancel func()) {
	orig := crashPoint
	var once sync.Once
	crashPoint = func(p string) error {
		err := error(nil)
		if p == point {
			once.Do(func() {
				d.freeze()
				cancel()
				err = errors.New("simulated crash at " + p)
			})
		}
		return err
	}
	t.Cleanup(func() { crashPoint = orig })
}

// listDest returns the destination files (index dir excluded), sorted.
func listDest(t *testing.T, dst string) []string {
	var out []string
	if err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == indexDir {
			return fs.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dst, p)
			out = append(out, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestCrashMatrix crashes the first transfer at each point where the journal
// matters, loses every record that was not fsynced, resumes, and checks the
// destination holds each source exactly once under its own name.
func TestCrashMatrix(t *testing.T) {
	want := []string{"2021/06/07/IMG_0000.jpg", "2021/06/07/IMG_0001.jpg", "2021/06/07/IMG_0002.jpg", "2021/06/07/IMG_0003.jpg"}
	for _, point := range []string{"begun", "before-rename", "renamed", "done"} {
		for _, skipDups := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/skipdups=%v", point, skipDups), func(t *testing.T) {
				noFlusher(t)
				root := t.TempDir()
				src, dst := filepath.Join(root, "card"), filepath.Join(root, "out")
				for i := range 4 {
					writeBytes(t, filepath.Join(src, fmt.Sprintf("IMG_%04d.jpg", i)), blob(3000+i, byte(i)), t1)
				}
				jpath := filepath.Join(root, "journal")
				o := baseOpts(src, dst)
				o.SkipDups, o.Workers, o.Journal = skipDups, 1, jpath

				d := trackDurability(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				crashAt(t, point, d, cancel)
				if _, err := Run(ctx, o, nil, nil); err == nil {
					t.Fatal("crashed run reported success")
				}
				d.powerLoss(t, jpath)
				d.powerLoss(t, indexPath(dst))

				rs, err := ReadJournal(jpath)
				if err != nil {
					t.Fatal(err)
				}
				crashPoint = func(string) error { return nil }
				o.Resume = rs
				s, err := Run(context.Background(), o, nil, nil)
				if err != nil || s.Failed != 0 || s.Copied+s.Resumed+s.Skipped != 4 {
					t.Fatalf("resume: %v %+v", err, s)
				}
				if got := listDest(t, dst); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("destination after resume:\n%v\nwant\n%v", got, want)
				}
			})
		}
	}
}
