// Package importer scans source directories for media files and copies (or
// moves) them into an organised destination tree, preserving timestamps.
package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/denislee/media-import/internal/media"
	"github.com/denislee/media-import/internal/organize"
	"golang.org/x/sys/unix"
)

// Options configures an import run.
type Options struct {
	Sources       []string
	Dest          string
	DirTemplate   string
	NameTemplate  string
	Kinds         map[media.Kind]bool
	Move          bool   // delete source after a successful copy
	DryRun        bool   // only report what would happen
	SkipDups      bool   // skip files already present (same size + content)
	DupCheck      string // DupCheckFast (default) or DupCheckFull
	DupAnyName    bool   // also detect copies stored under another name (hashes the source; needs the index)
	NoIndex       bool   // neither read nor write the destination index
	Verify        bool   // re-read the copy and compare checksums
	UseFFprobe    bool   // use ffprobe for formats without built-in parsers
	MtimeCapture  bool   // set file mtime to the capture time instead of the source mtime
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
	Elapsed    time.Duration
	Paused     bool
	Resumed    int            // files skipped because an earlier, interrupted run had finished them
	Verified   int            // copies whose checksum was re-read from disk and matched
	Active     []FileProgress // files being processed right now, one per busy worker
	ByKind     map[string]int // files imported per media kind (kind name → count)

	// Duplicate-check statistics.
	DupCheckBytes int64         // bytes read (source + destination) to decide duplicates
	DupQuick      int           // duplicates decided by size, mtime and sampled blocks
	DupFull       int           // duplicates decided by comparing every byte
	DupIndexed    int           // duplicates found through the destination index
	DupOtherName  int           // duplicates found under another name by checksum
	DupCheckTime  time.Duration // time spent checking, summed over workers
}

// FileProgress describes one file a worker is processing.
type FileProgress struct {
	Path  string
	Dest  string
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
	s.fp = FileProgress{Path: j.src, Size: j.info.Size(), Stage: "reading date"}
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
	phase                                atomic.Value
	slots                                []slot
	resumed, verified                    atomic.Int64
	gate                                 *Gate
	jnl                                  *journal
	byKind                               [media.Audio + 1]atomic.Int64

	dupBytes, dupNanos                     atomic.Int64
	dupQuick, dupFull, dupIndexed, dupOthr atomic.Int64
	idx                                    *index
	dirs                                   sync.Map // directories known to exist
	devs                                   sync.Map // dir → device id (uint64)

	mu       sync.Mutex
	reserved map[string]resv
	seen     map[string]int64      // destination paths known to exist → size (-1: not a regular file)
	names    map[string]*nameSlots // dir/name → suffixes known taken
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
	r := &runner{opt: opt, logf: logf, reserved: map[string]resv{}, seen: map[string]int64{}, names: map[string]*nameSlots{}, slots: make([]slot, opt.Workers), gate: opt.Pause}
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
	if !opt.NoIndex {
		ix, err := openIndex(opt.Dest, opt.DryRun)
		if err != nil {
			logf("WARN cannot write destination index: " + err.Error())
		}
		r.idx = ix
		defer r.idx.close()
	}
	r.phase.Store("scanning")
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
		if !opt.DryRun {
			need := r.totalBytes.Load()
			if rs := opt.Resume; rs != nil {
				for _, j := range jobs {
					if rs.Done[j.src] {
						need -= j.info.Size()
					}
				}
			}
			if free, ok := freeBytesFn(opt.Dest); ok && need > free {
				logf(fmt.Sprintf("WARN destination has %s free but up to %s may be copied (duplicates are skipped, so it may still fit)", HumanSize(free), HumanSize(need)))
			}
		}
		err = r.process(ctx, jobs)
	}
	close(stopTicker)
	<-tickerDone

	r.phase.Store("done")
	s := Summary{Progress: r.snapshot(start)}
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
	switch opt.DupCheck {
	case "":
		opt.DupCheck = DupCheckFast
	case DupCheckFast, DupCheckFull:
	default:
		return fmt.Errorf("unknown duplicate check mode %q", opt.DupCheck)
	}
	if opt.Workers <= 0 {
		opt.Workers = min(4, runtime.NumCPU())
	}
	return nil
}

func (r *runner) snapshot(start time.Time) Progress {
	now := time.Now()
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
		// One instant for both, so Elapsed stays exactly constant while paused.
		Elapsed:  now.Sub(start) - r.gate.pausedFor(now),
		Paused:   r.gate.Paused(),
		Resumed:  int(r.resumed.Load()),
		Verified: int(r.verified.Load()),
		Active:   r.active(),
		ByKind:   r.kindCounts(),

		DupCheckBytes: r.dupBytes.Load(),
		DupQuick:      int(r.dupQuick.Load()),
		DupFull:       int(r.dupFull.Load()),
		DupIndexed:    int(r.dupIndexed.Load()),
		DupOtherName:  int(r.dupOthr.Load()),
		DupCheckTime:  time.Duration(r.dupNanos.Load()),
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

func (r *runner) fail(src string, err error) {
	msg := src + ": " + err.Error()
	r.failed.Add(1)
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
				if p == r.opt.Dest || name == indexDir || (p != src && !r.opt.IncludeHidden && strings.HasPrefix(name, ".")) {
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

// crashPoint is called at the points of a transfer where a crash changes
// what resume must do; tests make it return an error to simulate one.
var crashPoint = func(string) error { return nil }

// captureTime is media.CaptureTime; tests replace it to count calls.
var captureTime = media.CaptureTime

func (r *runner) handle(ctx context.Context, j job, sl *slot) {
	if ctx.Err() != nil {
		return
	}
	sl.begin(j)
	defer sl.end()
	if rs := r.opt.Resume; rs != nil {
		if rs.Done[j.src] {
			r.resumed.Add(1)
			r.bytes.Add(j.info.Size())
			return
		}
		if dst, ok := rs.Pending[j.src]; ok && r.recoverPending(ctx, j, dst, sl) {
			return
		}
	}
	if r.opt.SkipDups {
		if dst, how := r.indexHit(ctx, j, sl); how != notDup {
			r.skipDup(j, sl, dst, how, digests{})
			return
		}
	}
	t, how := captureTime(j.src, j.kind, j.info, media.DateOptions{UseFFprobe: r.opt.UseFFprobe})

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
	dst, dup, err := r.reserve(ctx, dir, name, j, [2]time.Time{j.info.ModTime(), t}, sl)
	if err != nil {
		if ctx.Err() == nil {
			r.bytes.Add(max(0, j.info.Size()-sl.counted.Load())) // count it as processed so the bar can reach 100%
			r.fail(j.src, err)
		}
		return
	}
	if dup != notDup {
		r.skipDup(j, sl, dst, dup, digests{})
		return
	}
	written := false
	defer func() { r.release(dst, written, j.info.Size()) }()
	var known []byte

	if r.opt.SkipDups && r.opt.DupAnyName {
		other, d, err := r.otherNameHit(ctx, j, sl)
		if err != nil {
			if ctx.Err() == nil {
				r.bytes.Add(max(0, j.info.Size()-sl.counted.Load()))
				r.fail(j.src, err)
			}
			return
		}
		if other != "" {
			r.skipDup(j, sl, other, dupOther, d)
			return
		}
		known = d.sha // the source was hashed already; the copy need not hash it again
	}

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
	err = r.ensureDir(filepath.Dir(dst))
	var d digests
	if err == nil {
		d, err = r.transfer(ctx, j, dst, mtime, sl, known)
	}
	if err != nil {
		if ctx.Err() == nil {
			r.bytes.Add(max(0, j.info.Size()-sl.counted.Load()))
			r.fail(j.src, err)
		}
		return
	}
	written = true
	r.jnl.done(j.src, dst)
	if err := crashPoint("done"); err != nil {
		r.fail(j.src, err)
		return
	}
	r.copied.Add(1)
	r.byKind[j.kind].Add(1)
	r.record(j, dst, d)
	r.logf(fmt.Sprintf("%s %s -> %s [%s]", verb, j.src, dst, how))
}

func srcKey(j job) idxKey {
	return idxKey{filepath.Base(j.src), j.info.Size(), j.info.ModTime().UnixNano()}
}

// record adds a destination index entry for j stored at dst.
func (r *runner) record(j job, dst string, d digests) {
	if r.idx == nil || r.opt.DryRun {
		return
	}
	st, err := os.Stat(dst)
	if err != nil {
		return
	}
	if err := r.idx.add(srcKey(j), dst, st.ModTime(), d); err != nil {
		r.logf("WARN cannot update destination index: " + err.Error())
	}
}

// skipDup finishes a source that is already in the destination at dst. It is
// left on the source, even when moving.
func (r *runner) skipDup(j job, sl *slot, dst string, how dupHow, d digests) {
	r.skipped.Add(1)
	r.bytes.Add(max(0, j.info.Size()-sl.counted.Load()))
	r.jnl.done(j.src, dst)
	switch how {
	case dupQuick:
		r.dupQuick.Add(1) // not recorded: the content was only sampled
	case dupFull:
		r.dupFull.Add(1)
		r.record(j, dst, d)
	case dupOther:
		r.dupOthr.Add(1)
		r.record(j, dst, d)
	case dupIndexed:
		r.dupIndexed.Add(1)
	}
	r.logf(fmt.Sprintf("SKIP %s (already at %s, %s)", j.src, dst, how.note()))
}

// indexHit looks the source up in the destination index, before its date is
// parsed. A hit counts only if the recorded destination still has the
// recorded size and mtime; in full mode its content is compared as well.
func (r *runner) indexHit(ctx context.Context, j job, sl *slot) (string, dupHow) {
	if r.idx == nil {
		return "", notDup
	}
	defer r.timeDup(time.Now())
	for _, e := range r.idx.lookup(srcKey(j)) {
		dst := filepath.Join(r.opt.Dest, e.rel)
		st, err := os.Stat(dst)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				r.idx.remove(e.key, e.rel)
			}
			continue
		}
		if !st.Mode().IsRegular() || st.Size() != e.key.size || st.ModTime().UnixNano() != e.dmtime {
			continue
		}
		if r.opt.DupCheck != DupCheckFull {
			return dst, dupIndexed
		}
		if same, err := r.compare(ctx, j, dst, sl); err == nil && same {
			return dst, dupFull
		}
	}
	return "", notDup
}

// otherNameHit looks for an indexed destination file with the same size and
// checksum as the source, wherever it is stored. It reads the source only
// when the index knows a file of the same size, and then compares sample
// digests (192 KiB read) first: the whole source is hashed only if a sample
// matches or a candidate has none (indexed by an older version). It returns
// the digests it computed, so the copy can reuse them.
func (r *runner) otherNameHit(ctx context.Context, j job, sl *slot) (string, digests, error) {
	size := j.info.Size()
	cands := r.idx.withSize(size)
	if len(cands) == 0 {
		return "", digests{}, nil
	}
	defer r.timeDup(time.Now())
	sl.set(stageOtherNm, "")
	sl.bytes.Store(0)
	var d digests
	if slices.ContainsFunc(cands, func(e idxEntry) bool { return e.sample != nil }) {
		sample, n, err := sampleDigest(j.src, size)
		r.dupBytes.Add(n)
		if err != nil {
			return "", digests{}, err
		}
		d.sample = sample
		if !slices.ContainsFunc(cands, func(e idxEntry) bool { return e.sample == nil || bytes.Equal(e.sample, sample) }) {
			sl.set("preparing", "")
			return "", d, nil
		}
	}
	sum, err := hashFile(j.src, func() error { return r.gate.wait(ctx) }, func(n int64) {
		r.dupBytes.Add(n)
		sl.bytes.Add(n)
	}, false)
	if err != nil {
		return "", digests{}, err
	}
	d.sha = sum
	for _, e := range cands {
		if !bytes.Equal(e.sha, sum) {
			continue
		}
		dst := filepath.Join(r.opt.Dest, e.rel)
		st, err := os.Stat(dst)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				r.idx.remove(e.key, e.rel)
			}
			continue
		}
		if st.Size() == e.key.size && st.ModTime().UnixNano() == e.dmtime {
			return dst, d, nil
		}
	}
	sl.set("preparing", "")
	return "", d, nil
}

// transfer copies or moves j to dst and returns the digests of the data
// written (none when the file was renamed in place). known is the source's
// SHA-256 when it was computed already.
func (r *runner) transfer(ctx context.Context, j job, dst string, mtime time.Time, sl *slot, known []byte) (digests, error) {
	atime := accessTime(j.info)
	size := j.info.Size()
	// The B record must be durable before the copy appears under its final
	// name, or a crash in between would leave a file resume does not know
	// about (and would import again as name_1). A same-device rename needs no
	// such barrier: once it happened the source is gone and is not rescanned.
	seq := r.jnl.begin(j.src, dst)
	if err := crashPoint("begun"); err != nil {
		return digests{}, err
	}
	if r.opt.Move && r.sameDevice(j.info, filepath.Dir(dst)) {
		sl.set("moving", dst)
		if err := os.Rename(j.src, dst); err == nil {
			r.bytes.Add(size)
			sl.counted.Store(size)
			sl.bytes.Store(size)
			return digests{}, os.Chtimes(dst, atime, mtime)
		}
	}
	wait := func() error { return r.gate.wait(ctx) }
	var d digests
	for attempt := 1; ; attempt++ {
		stage := "copying"
		if attempt > 1 {
			stage = "copying again"
		}
		sl.set(stage, dst)
		sl.bytes.Store(0)
		var err error
		d, err = copyFile(j.src, dst, j.info, atime, mtime, wait, func(n int64) {
			r.bytes.Add(n)
			sl.counted.Add(n)
			sl.bytes.Add(n)
		}, func() error {
			if err := crashPoint("before-rename"); err != nil {
				return err
			}
			return r.jnl.syncTo(seq)
		}, known)
		if err != nil {
			return digests{}, err
		}
		if err := crashPoint("renamed"); err != nil {
			return digests{}, err
		}
		if !r.opt.Verify {
			break
		}
		sl.set("verifying", "")
		sl.bytes.Store(0)
		ok, err := verifyFile(dst, d.sha, wait, func(n int64) { sl.bytes.Add(n) })
		if err != nil {
			return digests{}, fmt.Errorf("verify: %w", err)
		}
		if ok {
			r.verified.Add(1)
			break
		}
		os.Remove(dst)
		if attempt == 2 {
			return digests{}, errors.New("verify: destination checksum mismatch twice, copy removed (failing disk or card?)")
		}
		r.logf(fmt.Sprintf("WARN %s: destination checksum mismatch, copying again", dst))
		r.bytes.Add(-sl.counted.Load())
		sl.counted.Store(0)
	}
	if r.opt.Move {
		sl.set("removing source", "")
		if err := os.Remove(j.src); err != nil {
			return digests{}, fmt.Errorf("copied but could not remove source: %w", err)
		}
	}
	return d, nil
}

// recoverPending handles a file whose transfer an interrupted run had started.
// It removes a leftover temp file and, if the destination was completed,
// records it as done. It reports whether the file needs no further work.
func (r *runner) recoverPending(ctx context.Context, j job, dst string, sl *slot) bool {
	part := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".part")
	if err := os.Remove(part); err == nil {
		r.logf("RESUME removed incomplete " + part)
	}
	st, err := os.Stat(dst)
	if err != nil || st.Size() != j.info.Size() {
		return false
	}
	same, err := r.compare(ctx, j, dst, sl)
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

func accessTime(fi fs.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	return fi.ModTime()
}

func (r *runner) sameDevice(fi fs.FileInfo, dir string) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if v, ok := r.devs.Load(dir); ok {
		return v.(uint64) == uint64(st.Dev)
	}
	var ds syscall.Stat_t
	if err := syscall.Stat(dir, &ds); err != nil {
		return false
	}
	r.devs.Store(dir, uint64(ds.Dev))
	return st.Dev == ds.Dev
}

// freeBytesFn is freeBytes; tests replace it to force a low-space warning.
var freeBytesFn = freeBytes

// freeBytes reports the space available to unprivileged users on the
// filesystem holding dir. The destination tree often does not exist yet, so
// it walks up to the nearest path that does.
func freeBytes(dir string) (int64, bool) {
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, false
		}
		dir = parent
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}

// HumanSize formats n with 1024-based units: "512 B", "1.5 KiB", "3.0 GiB".
func HumanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	const units = "KMGTPE"
	v := float64(n)
	i := -1
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %ciB", v, units[i])
}
