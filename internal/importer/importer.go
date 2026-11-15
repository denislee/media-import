// Package importer scans source directories for media files and copies (or
// moves) them into an organised destination tree, preserving timestamps.
package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/denislee/media-import/internal/media"
	"github.com/denislee/media-import/internal/organize"
)

// Options configures an import run.
type Options struct {
	Sources       []string
	Dest          string
	DirTemplate   string
	NameTemplate  string
	Kinds         map[media.Kind]bool
	Move          bool // delete source after a successful copy
	DryRun        bool // only report what would happen
	SkipDups      bool // skip files already present (same size + content)
	Verify        bool // re-read the copy and compare checksums
	UseFFprobe    bool // use ffprobe for formats without built-in parsers
	MtimeCapture  bool // set file mtime to the capture time instead of the source mtime
	IncludeHidden bool
	Workers       int

	Journal string       // append-only progress journal used to resume; "" disables
	Resume  *ResumeState // state of an interrupted run to continue (from ReadJournal)
	Pause   *Gate        // optional pause/resume control
}

// Progress is a snapshot of a running import.
type Progress struct {
	Phase      string // "scanning", "importing", "done"
	Found      int
	Total      int
	Done       int
	Copied     int
	Skipped    int
	Failed     int
	Bytes      int64
	TotalBytes int64
	Current    string
	Elapsed    time.Duration
	Workers    int
	Paused     bool
	Resumed    int            // files skipped because an earlier, interrupted run had finished them
	Verified   int            // copies whose checksum was re-read from disk and matched
	Active     []FileProgress // files being processed right now, one per busy worker
	ByKind     map[string]int // files imported per media kind (kind name → count)
}

// FileProgress describes one file a worker is processing.
type FileProgress struct {
	Path  string
	Dest  string
	Kind  string
	Stage string // "reading date", "preparing", "checking if already imported", "copying", "moving", "verifying", "removing source"…
	Size  int64
	Bytes int64
}

// slot tracks what one worker is doing.
type slot struct {
	mu      sync.Mutex
	fp      FileProgress
	active  bool
	bytes   atomic.Int64 // shown in the per-file bar (copy or verify progress)
	counted atomic.Int64 // bytes this file has contributed to the overall total
}

func (s *slot) begin(j job) {
	s.mu.Lock()
	s.fp = FileProgress{Path: j.src, Kind: j.kind.String(), Size: j.info.Size(), Stage: "reading date"}
	s.active = true
	s.mu.Unlock()
	s.bytes.Store(0)
	s.counted.Store(0)
}

func (s *slot) set(stage, dest string) {
	s.mu.Lock()
	s.fp.Stage = stage
	if dest != "" {
		s.fp.Dest = dest
	}
	s.mu.Unlock()
}

func (s *slot) end() {
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
}

// Summary is the final result of a run.
type Summary struct {
	Progress
	Errors []string
}

type job struct {
	src    string
	kind   media.Kind
	info   fs.FileInfo
	source string
}

type runner struct {
	opt  Options
	logf func(string)

	found, done, copied, skipped, failed atomic.Int64
	bytes, totalBytes                    atomic.Int64
	total                                atomic.Int64
	phase, current                       atomic.Value
	slots                                []slot
	resumed, verified                    atomic.Int64
	gate                                 *Gate
	jnl                                  *journal
	byKind                               [media.Audio + 1]atomic.Int64

	mu       sync.Mutex
	reserved map[string]bool
	errs     []string
}

// Run executes an import. progress is called periodically (from another
// goroutine) and logf for each notable event; both may be nil.
func Run(ctx context.Context, opt Options, progress func(Progress), logf func(string)) (Summary, error) {
	if logf == nil {
		logf = func(string) {}
	}
	if err := validate(&opt); err != nil {
		return Summary{}, err
	}
	r := &runner{opt: opt, logf: logf, reserved: map[string]bool{}, slots: make([]slot, opt.Workers), gate: opt.Pause}
	if r.gate == nil {
		r.gate = &Gate{}
	}
	if !opt.DryRun {
		j, err := openJournal(opt.Journal, opt.Resume != nil)
		if err != nil {
			logf("WARN cannot write resume journal: " + err.Error())
		}
		r.jnl = j
		defer r.jnl.close()
	}
	r.phase.Store("scanning")
	r.current.Store("")
	start := time.Now()

	stopTicker := make(chan struct{})
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		if progress == nil {
			return
		}
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				progress(r.snapshot(start))
			case <-stopTicker:
				return
			}
		}
	}()

	jobs, err := r.scan(ctx)
	if err == nil {
		r.phase.Store("importing")
		r.total.Store(int64(len(jobs)))
		r.jnl.write("T", strconv.Itoa(len(jobs)), strconv.FormatInt(r.totalBytes.Load(), 10))
		err = r.process(ctx, jobs)
	}
	close(stopTicker)
	<-tickerDone

	r.phase.Store("done")
	r.current.Store("")
	s := Summary{Progress: r.snapshot(start), Errors: r.errs}
	if progress != nil {
		progress(s.Progress)
	}
	return s, err
}

func validate(opt *Options) error {
	if len(opt.Sources) == 0 {
		return errors.New("no source directory selected")
	}
	if opt.Dest == "" {
		return errors.New("no destination directory selected")
	}
	dest, err := filepath.Abs(opt.Dest)
	if err != nil {
		return err
	}
	opt.Dest = dest
	for i, s := range opt.Sources {
		abs, err := filepath.Abs(s)
		if err != nil {
			return err
		}
		st, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("source %s: %w", s, err)
		}
		if !st.IsDir() {
			return fmt.Errorf("source %s is not a directory", s)
		}
		if abs == dest {
			return errors.New("source and destination are the same directory")
		}
		opt.Sources[i] = abs
	}
	anyKind := false
	for _, v := range opt.Kinds {
		anyKind = anyKind || v
	}
	if !anyKind {
		return errors.New("no media types selected")
	}
	if opt.Workers <= 0 {
		opt.Workers = min(4, runtime.NumCPU())
	}
	return nil
}

func (r *runner) snapshot(start time.Time) Progress {
	return Progress{
		Phase:      r.phase.Load().(string),
		Found:      int(r.found.Load()),
		Total:      int(r.total.Load()),
		Done:       int(r.done.Load()),
		Copied:     int(r.copied.Load()),
		Skipped:    int(r.skipped.Load()),
		Failed:     int(r.failed.Load()),
		Bytes:      r.bytes.Load(),
		TotalBytes: r.totalBytes.Load(),
		Current:    r.current.Load().(string),
		Elapsed:    time.Since(start) - r.gate.pausedFor(),
		Workers:    len(r.slots),
		Paused:     r.gate.Paused(),
		Resumed:    int(r.resumed.Load()),
		Verified:   int(r.verified.Load()),
		Active:     r.active(),
		ByKind:     r.kindCounts(),
	}
}

func (r *runner) active() []FileProgress {
	var out []FileProgress
	for i := range r.slots {
		s := &r.slots[i]
		s.mu.Lock()
		if s.active {
			fp := s.fp
			fp.Bytes = s.bytes.Load()
			out = append(out, fp)
		}
		s.mu.Unlock()
	}
	return out
}

func (r *runner) kindCounts() map[string]int {
	m := map[string]int{}
	for k := range r.byKind {
		if n := r.byKind[k].Load(); n > 0 {
			m[media.Kind(k).String()] = int(n)
		}
	}
	return m
}

func (r *runner) fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	r.failed.Add(1)
	r.mu.Lock()
	r.errs = append(r.errs, msg)
	r.mu.Unlock()
	r.logf("ERROR " + msg)
}

func (r *runner) scan(ctx context.Context) ([]job, error) {
	var jobs []job
	for _, src := range r.opt.Sources {
		label := filepath.Base(src)
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				r.logf(fmt.Sprintf("WARN cannot read %s: %v", p, err))
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if p == r.opt.Dest || (p != src && !r.opt.IncludeHidden && strings.HasPrefix(name, ".")) {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || (!r.opt.IncludeHidden && strings.HasPrefix(name, ".")) {
				return nil
			}
			kind := media.Classify(name)
			if kind == media.Unknown || !r.opt.Kinds[kind] {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				r.logf(fmt.Sprintf("WARN stat %s: %v", p, err))
				return nil
			}
			jobs = append(jobs, job{src: p, kind: kind, info: info, source: label})
			r.found.Add(1)
			r.totalBytes.Add(info.Size())
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

func (r *runner) process(ctx context.Context, jobs []job) error {
	ch := make(chan job)
	var wg sync.WaitGroup
	for i := range r.opt.Workers {
		wg.Add(1)
		go func(sl *slot) {
			defer wg.Done()
			for j := range ch {
				_ = r.gate.wait(ctx)
				r.handle(ctx, j, sl)
				r.done.Add(1)
			}
		}(&r.slots[i])
	}
loop:
	for _, j := range jobs {
		select {
		case ch <- j:
		case <-ctx.Done():
			break loop
		}
	}
	close(ch)
	wg.Wait()
	return ctx.Err()
}

func (r *runner) handle(ctx context.Context, j job, sl *slot) {
	if ctx.Err() != nil {
		return
	}
	r.current.Store(j.src)
	sl.begin(j)
	defer sl.end()
	if rs := r.opt.Resume; rs != nil {
		if rs.Done[j.src] {
			r.resumed.Add(1)
			r.bytes.Add(j.info.Size())
			return
		}
		if dst, ok := rs.Pending[j.src]; ok && r.recoverPending(j, dst, sl) {
			return
		}
	}
	t, how := media.CaptureTime(j.src, j.kind, j.info, media.DateOptions{UseFFprobe: r.opt.UseFFprobe})

	ext := filepath.Ext(j.src)
	f := organize.Fields{
		Time:   t,
		Kind:   j.kind.String(),
		Name:   strings.TrimSuffix(filepath.Base(j.src), ext),
		Ext:    media.Ext(j.src),
		Source: j.source,
	}
	dir := filepath.Join(r.opt.Dest, organize.Dir(r.opt.DirTemplate, f))
	name := organize.FileName(r.opt.NameTemplate, f, strings.ToLower(ext))

	sl.set("preparing", "")
	dst, dup, err := r.reserve(ctx, dir, name, j, sl)
	if err != nil {
		if ctx.Err() == nil {
			r.bytes.Add(max(0, j.info.Size()-sl.counted.Load())) // count it as processed so the bar can reach 100%
			r.fail("%s: %v", j.src, err)
		}
		return
	}
	if dup {
		r.skipped.Add(1)
		r.bytes.Add(max(0, j.info.Size()-sl.counted.Load()))
		r.jnl.done(j.src, dst)
		r.logf(fmt.Sprintf("SKIP %s (already at %s)", j.src, dst))
		return
	}
	defer r.release(dst)

	verb := "COPY"
	if r.opt.Move {
		verb = "MOVE"
	}
	if r.opt.DryRun {
		r.copied.Add(1)
		r.byKind[j.kind].Add(1)
		r.bytes.Add(j.info.Size())
		r.logf(fmt.Sprintf("DRY %s %s -> %s [%s %s]", verb, j.src, dst, t.Format("2006-01-02 15:04:05"), how))
		return
	}

	mtime := j.info.ModTime()
	if r.opt.MtimeCapture {
		mtime = t
	}
	if err := r.transfer(ctx, j, dst, mtime, sl); err != nil {
		if ctx.Err() == nil {
			r.bytes.Add(max(0, j.info.Size()-sl.counted.Load()))
			r.fail("%s: %v", j.src, err)
		}
		return
	}
	r.jnl.done(j.src, dst)
	r.copied.Add(1)
	r.byKind[j.kind].Add(1)
	r.logf(fmt.Sprintf("%s %s -> %s [%s]", verb, j.src, dst, how))
}

// reserve picks a free destination path. If an identical file already exists
// and SkipDups is set it returns dup=true.
func (r *runner) reserve(ctx context.Context, dir, name string, j job, sl *slot) (string, bool, error) {
	if !r.opt.DryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", false, err
		}
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; i < 10000; i++ {
		cand := filepath.Join(dir, name)
		if i > 0 {
			cand = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
		}
		r.mu.Lock()
		if r.reserved[cand] {
			r.mu.Unlock()
			continue
		}
		st, err := os.Stat(cand)
		if errors.Is(err, fs.ErrNotExist) {
			r.reserved[cand] = true
			r.mu.Unlock()
			return cand, false, nil
		}
		r.mu.Unlock()
		if err != nil {
			return "", false, err
		}
		if r.opt.SkipDups && st.Size() == j.info.Size() {
			same, err := r.compare(ctx, j, cand, sl)
			if err != nil {
				return "", false, err
			}
			if same {
				return cand, true, nil
			}
			sl.set("preparing", "")
		}
	}
	return "", false, errors.New("too many name collisions")
}

func (r *runner) release(p string) {
	r.mu.Lock()
	delete(r.reserved, p)
	r.mu.Unlock()
}

func (r *runner) transfer(ctx context.Context, j job, dst string, mtime time.Time, sl *slot) error {
	atime := accessTime(j.info)
	size := j.info.Size()
	r.jnl.begin(j.src, dst)
	if r.opt.Move && sameDevice(j.info, filepath.Dir(dst)) {
		sl.set("moving", dst)
		if err := os.Rename(j.src, dst); err == nil {
			r.bytes.Add(size)
			sl.counted.Store(size)
			sl.bytes.Store(size)
			return os.Chtimes(dst, atime, mtime)
		}
	}
	wait := func() error { return r.gate.wait(ctx) }
	for attempt := 1; ; attempt++ {
		stage := "copying"
		if attempt > 1 {
			stage = "copying again"
		}
		sl.set(stage, dst)
		sl.bytes.Store(0)
		sum, err := copyFile(ctx, j.src, dst, j.info, atime, mtime, wait, func(n int64) {
			r.bytes.Add(n)
			sl.counted.Add(n)
			sl.bytes.Add(n)
		})
		if err != nil {
			return err
		}
		if !r.opt.Verify {
			break
		}
		sl.set("verifying", "")
		sl.bytes.Store(0)
		ok, err := verifyFile(dst, sum, wait, func(n int64) { sl.bytes.Add(n) })
		if err != nil {
			return fmt.Errorf("verify: %w", err)
		}
		if ok {
			r.verified.Add(1)
			break
		}
		os.Remove(dst)
		if attempt == 2 {
			return errors.New("verify: destination checksum mismatch twice, copy removed (failing disk or card?)")
		}
		r.logf(fmt.Sprintf("WARN %s: destination checksum mismatch, copying again", dst))
		r.bytes.Add(-sl.counted.Load())
		sl.counted.Store(0)
	}
	if r.opt.Move {
		sl.set("removing source", "")
		if err := os.Remove(j.src); err != nil {
			return fmt.Errorf("copied but could not remove source: %w", err)
		}
	}
	return nil
}

// recoverPending handles a file whose transfer an interrupted run had started.
// It removes a leftover temp file and, if the destination was completed,
// records it as done. It reports whether the file needs no further work.
func (r *runner) recoverPending(j job, dst string, sl *slot) bool {
	part := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".part")
	if err := os.Remove(part); err == nil {
		r.logf("RESUME removed incomplete " + part)
	}
	st, err := os.Stat(dst)
	if err != nil || st.Size() != j.info.Size() {
		return false
	}
	same, err := r.compare(context.Background(), j, dst, sl)
	if err != nil || !same {
		r.logf(fmt.Sprintf("WARN %s exists but differs from %s; importing again under a new name", dst, j.src))
		return false
	}
	if r.opt.Move {
		if err := os.Remove(j.src); err != nil {
			r.logf(fmt.Sprintf("WARN could not remove source %s: %v", j.src, err))
		}
	}
	r.jnl.done(j.src, dst)
	r.resumed.Add(1)
	r.bytes.Add(max(0, j.info.Size()-sl.counted.Load()))
	r.logf(fmt.Sprintf("RESUME %s already complete at %s", j.src, dst))
	return true
}

// verifyFile re-reads p from the disk (not the page cache) and compares its
// SHA-256 with want.
func verifyFile(p string, want []byte, wait func() error, onBytes func(int64)) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	// The copy was fsynced, so its pages are clean and can be dropped; the
	// read below then has to come from the device itself.
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := wait(); err != nil {
			return false, err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			onBytes(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return false, rerr
		}
	}
	return bytes.Equal(h.Sum(nil), want), nil
}

// copyFile copies src to dst via a temporary file, fsyncs it, restores the
// permissions and timestamps, and returns the SHA-256 of the data written.
func copyFile(ctx context.Context, src, dst string, info fs.FileInfo, atime, mtime time.Time, wait func() error, onBytes func(int64)) ([]byte, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".part")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm()|0o600)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(tmp)
		}
	}()

	h := sha256.New()
	buf := make([]byte, 1<<20)
	w := io.MultiWriter(out, h)
	for {
		if err := wait(); err != nil {
			return nil, err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return nil, err
			}
			onBytes(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if err := out.Sync(); err != nil {
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	if err := os.Chtimes(tmp, atime, mtime); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return nil, err
	}
	ok = true
	return h.Sum(nil), nil
}

func fileHash(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var h hash.Hash = sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// compare checks whether src and an existing destination file are identical,
// reporting progress on the worker's slot and in the overall total. If they
// differ, the bytes counted so far are taken back out of the total, since the
// file will be copied (and counted) again.
func (r *runner) compare(ctx context.Context, j job, existing string, sl *slot) (bool, error) {
	sl.set("checking if already imported", existing)
	sl.bytes.Store(0)
	same, err := sameContent(j.src, existing, func() error { return r.gate.wait(ctx) }, func(n int64) {
		sl.bytes.Add(n)
		sl.counted.Add(n)
		r.bytes.Add(n)
	})
	if err != nil || !same {
		r.bytes.Add(-sl.counted.Swap(0))
		sl.bytes.Store(0)
	}
	return same, err
}

// sameContent compares two files chunk by chunk, stopping at the first
// difference. onBytes is called with the number of bytes compared.
func sameContent(a, b string, wait func() error, onBytes func(int64)) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	const chunk = 1 << 20
	ba, bb := make([]byte, chunk), make([]byte, chunk)
	for {
		if err := wait(); err != nil {
			return false, err
		}
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		onBytes(int64(na))
		endA := ea == io.EOF || ea == io.ErrUnexpectedEOF
		endB := eb == io.EOF || eb == io.ErrUnexpectedEOF
		switch {
		case ea != nil && !endA:
			return false, ea
		case eb != nil && !endB:
			return false, eb
		case endA || endB:
			return endA && endB, nil
		}
	}
}

func accessTime(fi fs.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	return fi.ModTime()
}

func sameDevice(fi fs.FileInfo, dir string) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	var ds syscall.Stat_t
	if err := syscall.Stat(dir, &ds); err != nil {
		return false
	}
	return st.Dev == ds.Dev
}
