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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Duplicate-check modes (Options.DupCheck).
const (
	DupCheckFast = "fast" // same size + matching mtime → sampled compare
	DupCheckFull = "full" // always compare every byte
)

// dupHow says whether (and how) a source was found to be already imported.
type dupHow int

const (
	notDup     dupHow = iota
	dupQuick          // same size and mtime, sampled blocks matched
	dupFull           // every byte compared
	dupIndexed        // destination index entry, confirmed by a fresh stat
	dupOther          // same SHA-256 as an indexed file under another name
)

func (h dupHow) note() string {
	switch h {
	case dupQuick:
		return "quick check"
	case dupFull:
		return "content verified"
	case dupIndexed:
		return "indexed"
	case dupOther:
		return "same checksum, other name"
	}
	return ""
}

// resv is the state of a destination path in runner.reserved.
type resv uint8

const (
	resvNone    resv = iota
	resvBusy         // a worker is writing it
	resvWritten      // this run wrote it; it exists on disk
)

const (
	sampleSize     = 64 << 10
	mtimeSlack     = 2 * time.Second // FAT32 stores mtimes with 2 s resolution
	bufSize        = 1 << 20
	preallocMin    = 1 << 20 // smaller copies are not preallocated: their write fails as fast as fallocate would
	writebackEvery = 8 << 20 // start writeback of a large copy every this many bytes
	stageCheck     = "checking if already imported"
	stageQuick     = "checking if already imported (quick)"
	stageOtherNm   = "looking for copies under other names"
)

// statCand is os.Stat for reserve; tests replace it to count probes.
var statCand = os.Stat

var bufPool = sync.Pool{New: func() any { b := make([]byte, bufSize); return &b }}

func getBuf() *[]byte  { return bufPool.Get().(*[]byte) }
func putBuf(b *[]byte) { bufPool.Put(b) }

// maxCollisions bounds the suffixes tried for one destination name.
const maxCollisions = 10000

// nameSlots remembers, for one destination dir+name, how far the suffix
// sequence name, name_1, name_2, … is already known to be taken, so the k-th
// same-named source does not walk k candidates. Guarded by runner.mu.
type nameSlots struct {
	next   int             // every suffix below next is in runner.seen
	bySize map[int64][]int // suffixes below next, by seen size (regular files only)
}

// candName is the i-th candidate for base+ext in dir: name, name_1, name_2, …
func candName(dir, base, ext string, i int) string {
	if i == 0 {
		return filepath.Join(dir, base+ext)
	}
	return filepath.Join(dir, base+"_"+strconv.Itoa(i)+ext)
}

// advance moves ns.next past suffixes that runner.seen already holds. r.mu
// must be held. seen entries are never removed, so a suffix below next stays
// taken for the rest of the run.
func (r *runner) advance(ns *nameSlots, dir, base, ext string) {
	for ns.next < maxCollisions {
		sz, ok := r.seen[candName(dir, base, ext, ns.next)]
		if !ok {
			return
		}
		if sz >= 0 {
			ns.bySize[sz] = append(ns.bySize[sz], ns.next)
		}
		ns.next++
	}
}

// reserve picks a free destination path. If an identical file already exists
// and SkipDups is set it returns that path and how it was recognised.
//
// os.Stat runs without holding r.mu. That is safe because a claimed path
// stays in r.reserved: resvBusy until its transfer finishes, then resvWritten
// (never deleted), so a stat that raced with another worker's rename is
// detected and repeated.
//
// Suffixes below the name's nameSlots.next are skipped unless their seen size
// equals the source's (only those can be duplicates), which is what the
// per-candidate check below would decide for them anyway.
func (r *runner) reserve(ctx context.Context, dir, name string, j job, expect [2]time.Time, sl *slot) (string, dupHow, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	size := j.info.Size()
	var (
		srcSum   []byte
		fullDone bool
	)
	key := filepath.Join(dir, name)
	r.mu.Lock()
	ns := r.names[key]
	if ns == nil {
		ns = &nameSlots{bySize: map[int64][]int{}}
		r.names[key] = ns
	}
	r.advance(ns, dir, base, ext)
	var known []int
	if r.opt.SkipDups {
		known = slices.Clone(ns.bySize[size])
	}
	start := ns.next
	r.mu.Unlock()
	// try examines suffix i; done reports that reserve has its answer.
	try := func(i int) (cand string, how dupHow, done bool, err error) {
		cand = candName(dir, base, ext, i)
		for {
			r.mu.Lock()
			before := r.reserved[cand]
			seenSize, seen := r.seen[cand]
			r.mu.Unlock()
			if before == resvBusy {
				return "", notDup, false, nil
			}
			// A path this run already found (or wrote) cannot be free, and can be
			// a duplicate only if its size matches: skip the stat otherwise. This
			// keeps many same-named sources at O(1) stats each instead of O(n).
			if seen && (!r.opt.SkipDups || seenSize != size) {
				return "", notDup, false, nil
			}
			st, err := statCand(cand)
			if errors.Is(err, fs.ErrNotExist) {
				r.mu.Lock()
				now := r.reserved[cand]
				switch {
				case now == resvBusy:
					r.mu.Unlock()
					return "", notDup, false, nil
				case now == resvWritten && before != resvWritten:
					// Written by another worker after our stat: look again.
					r.mu.Unlock()
					continue
				}
				r.reserved[cand] = resvBusy
				r.mu.Unlock()
				return cand, notDup, true, nil
			}
			if err != nil {
				return "", notDup, true, err
			}
			r.mu.Lock()
			if st.Mode().IsRegular() {
				r.seen[cand] = st.Size()
			} else {
				r.seen[cand] = -1 // never a duplicate candidate
			}
			r.mu.Unlock()
			if !r.opt.SkipDups || st.Size() != size || !st.Mode().IsRegular() {
				return "", notDup, false, nil
			}
			how, err := r.checkCandidate(ctx, j, cand, st, expect, sl, &srcSum, &fullDone)
			if err != nil {
				return "", notDup, true, err
			}
			if how != notDup {
				return cand, how, true, nil
			}
			sl.set("preparing", "")
			return "", notDup, false, nil
		}
	}
	for _, i := range known {
		if cand, how, done, err := try(i); done {
			return cand, how, err
		}
	}
	for i := start; i < maxCollisions; i++ {
		if cand, how, done, err := try(i); done {
			return cand, how, err
		}
	}
	return "", notDup, errors.New("too many name collisions")
}

// release ends a reservation. A path that was written stays known to the
// run so concurrent reservations re-check it (see reserve).
func (r *runner) release(p string, written bool, size int64) {
	r.mu.Lock()
	if written {
		r.reserved[p] = resvWritten
		r.seen[p] = size
	} else if r.reserved[p] == resvBusy {
		delete(r.reserved, p)
	}
	r.mu.Unlock()
}

// ensureDir creates dir, once per run. It is called only when a file is
// about to be written there, so duplicates never create empty folders.
func (r *runner) ensureDir(dir string) error {
	if r.opt.DryRun {
		return nil
	}
	if _, ok := r.dirs.Load(dir); ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r.dirs.Store(dir, true)
	return nil
}

// checkCandidate decides whether the existing same-size file cand is a copy
// of the source, using the cheapest test the DupCheck mode allows.
func (r *runner) checkCandidate(ctx context.Context, j job, cand string, st fs.FileInfo, expect [2]time.Time, sl *slot, srcSum *[]byte, fullDone *bool) (dupHow, error) {
	defer r.timeDup(time.Now())
	size := j.info.Size()
	if r.opt.DupCheck != DupCheckFull && mtimeNear(st.ModTime(), expect) && size > 3*sampleSize {
		sl.set(stageQuick, cand)
		same, n, err := sampledSame(j.src, cand, size)
		r.dupBytes.Add(n)
		if err != nil || !same {
			return notDup, err // differing samples prove the files differ
		}
		return dupQuick, nil
	}
	var same bool
	var err error
	if !*fullDone {
		*fullDone = true
		same, err = r.compare(ctx, j, cand, sl)
	} else {
		// A further same-size candidate: hash the source once instead of
		// reading it again for every candidate.
		same, err = r.compareHashed(ctx, j, cand, sl, srcSum)
	}
	if err != nil || !same {
		return notDup, err
	}
	return dupFull, nil
}

func mtimeNear(m time.Time, expect [2]time.Time) bool {
	for _, e := range expect {
		if e.IsZero() {
			continue
		}
		d := m.Sub(e)
		if d < 0 {
			d = -d
		}
		if d <= mtimeSlack {
			return true
		}
	}
	return false
}

// sampledSame compares the blocks sampleRanges(size) names (the same ones the
// index's sample digest covers) in a and b (both size bytes long). It returns
// the number of bytes read.
func sampledSame(a, b string, size int64) (bool, int64, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, 0, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, 0, err
	}
	defer fb.Close()
	pa, pb := getBuf(), getBuf()
	defer putBuf(pa)
	defer putBuf(pb)
	var read int64
	for _, rg := range sampleRanges(size) {
		for off := rg[0]; off < rg[1]; {
			n := min(int64(len(*pa)), rg[1]-off)
			ba, bb := (*pa)[:n], (*pb)[:n]
			na, err := fa.ReadAt(ba, off)
			read += int64(na)
			if err != nil && !(errors.Is(err, io.EOF) && int64(na) == n) {
				return false, read, err
			}
			nb, err := fb.ReadAt(bb, off)
			read += int64(nb)
			if err != nil && !(errors.Is(err, io.EOF) && int64(nb) == n) {
				return false, read, err
			}
			if !bytes.Equal(ba, bb) {
				return false, read, nil
			}
			off += n
		}
	}
	return true, read, nil
}

// compare checks whether src and an existing destination file are identical,
// reporting progress on the worker's slot and in the overall total. If they
// differ, the bytes counted so far are taken back out of the total, since the
// file will be copied (and counted) again.
func (r *runner) compare(ctx context.Context, j job, existing string, sl *slot) (bool, error) {
	sl.set(stageCheck, existing)
	sl.bytes.Store(0)
	same, err := sameContent(j.src, existing, func() error { return r.gate.wait(ctx) }, func(n int64) {
		r.dupBytes.Add(2 * n)
		sl.bytes.Add(n)
		sl.counted.Add(n)
		r.bytes.Add(n)
	})
	if err != nil || !same {
		r.uncount(sl)
	}
	return same, err
}

// compareHashed is compare for a second or later candidate: the source is
// hashed once (*srcSum caches it) and each candidate is hashed in full.
func (r *runner) compareHashed(ctx context.Context, j job, existing string, sl *slot, srcSum *[]byte) (bool, error) {
	sl.set(stageCheck, existing)
	sl.bytes.Store(0)
	wait := func() error { return r.gate.wait(ctx) }
	if *srcSum == nil {
		s, err := hashFile(j.src, wait, func(n int64) { r.dupBytes.Add(n) }, false)
		if err != nil {
			return false, err
		}
		*srcSum = s
	}
	d, err := hashFile(existing, wait, func(n int64) {
		r.dupBytes.Add(n)
		sl.bytes.Add(n)
		sl.counted.Add(n)
		r.bytes.Add(n)
	}, true)
	same := err == nil && bytes.Equal(d, *srcSum)
	if !same {
		r.uncount(sl)
	}
	return same, err
}

// uncount takes the bytes the slot's file added to the total back out and
// clears its bar, for a file that will be read (and counted) again.
func (r *runner) uncount(sl *slot) {
	r.bytes.Add(-sl.counted.Swap(0))
	sl.bytes.Store(0)
}

func (r *runner) timeDup(start time.Time) { r.dupNanos.Add(int64(time.Since(start))) }

// sameContent compares two files chunk by chunk, stopping at the first
// difference. The two files are read concurrently so reads from the source
// device and the destination disk overlap. onBytes is called with the number
// of bytes compared.
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
	_ = unix.Fadvise(int(fa.Fd()), 0, 0, unix.FADV_SEQUENTIAL)
	_ = unix.Fadvise(int(fb.Fd()), 0, 0, unix.FADV_SEQUENTIAL)
	// The destination data is not needed again; keep the page cache useful.
	defer func() { _ = unix.Fadvise(int(fb.Fd()), 0, 0, unix.FADV_DONTNEED) }()

	pa, pb := getBuf(), getBuf()
	defer putBuf(pa)
	defer putBuf(pb)
	ba, bb := *pa, *pb
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 1)
	for {
		if err := wait(); err != nil {
			return false, err
		}
		go func() {
			n, err := io.ReadFull(fb, bb)
			ch <- res{n, err}
		}()
		na, ea := io.ReadFull(fa, ba)
		rb := <-ch
		nb, eb := rb.n, rb.err
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

// hashFile returns the SHA-256 of p, calling wait before each chunk. With
// dropAfter the file's pages are dropped from the page cache once hashed, so
// a large destination file does not stay resident.
func hashFile(p string, wait func() error, onBytes func(int64), dropAfter bool) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if dropAfter {
		defer func() { _ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED) }()
	}
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_SEQUENTIAL)
	return hashReader(f, wait, onBytes)
}

func hashReader(f io.Reader, wait func() error, onBytes func(int64)) ([]byte, error) {
	h := sha256.New()
	pb := getBuf()
	defer putBuf(pb)
	buf := *pb
	for {
		if err := wait(); err != nil {
			return nil, err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			onBytes(int64(n))
		}
		if rerr == io.EOF {
			return h.Sum(nil), nil
		}
		if rerr != nil {
			return nil, rerr
		}
	}
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
	got, err := hashReader(f, wait, onBytes)
	if err != nil {
		return false, err
	}
	return bytes.Equal(got, want), nil
}

// fallocate is unix.Fallocate; tests replace it to count calls and fake ENOSPC.
var fallocate = unix.Fallocate

// syncFileRange is unix.SyncFileRange; tests replace it to count calls.
var syncFileRange = unix.SyncFileRange

// preallocate reserves size bytes for f, so a copy that cannot possibly fit
// fails before it writes anything. Only a real "no space left" error is
// returned: filesystems without fallocate support (EOPNOTSUPP and friends)
// simply allocate as the data arrives.
func preallocate(f *os.File, size int64) error {
	err := fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, size) // keep the size: a short copy must not gain zero padding
	if errors.Is(err, unix.ENOSPC) {
		return err
	}
	return nil
}

// copyFile copies src to dst via a temporary file, starting writeback as a
// large copy goes, fsyncs it, restores the permissions and timestamps, and
// returns the SHA-256 of the data written.
// beforeRename runs just before the temporary file is renamed to dst; an
// error from it aborts the copy. When knownSum (the source's SHA-256) is
// given, the data is not hashed again and knownSum is returned.
func copyFile(src, dst string, info fs.FileInfo, atime, mtime time.Time, wait func() error, onBytes func(int64), beforeRename func() error, knownSum []byte) (digests, error) {
	in, err := os.Open(src)
	if err != nil {
		return digests{}, err
	}
	defer in.Close()
	_ = unix.Fadvise(int(in.Fd()), 0, 0, unix.FADV_SEQUENTIAL)

	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".part")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm()|0o600)
	if err != nil {
		return digests{}, err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(tmp)
		}
	}()
	// Reserve space up front for large copies, so one that cannot possibly fit
	// fails before writing anything. Small files skip the extra syscall;
	// filesystems without fallocate are ignored.
	if info.Size() >= preallocMin {
		if err := preallocate(out, info.Size()); err != nil {
			return digests{}, fmt.Errorf("not enough space for %s: %w", dst, err)
		}
	}

	writers := []io.Writer{out}
	var h hash.Hash
	if knownSum == nil {
		h = sha256.New()
		writers = append(writers, h)
	}
	// A file no larger than the sampled blocks is sampled whole, so its sample
	// digest is its SHA-256: when that is computed anyway, hash the data once.
	var sh *sampleHasher
	if h == nil || len(sampleRanges(info.Size())) > 1 {
		sh = newSampleHasher(info.Size())
		writers = append(writers, sh)
	}
	pb := getBuf()
	defer putBuf(pb)
	buf := *pb
	w := io.MultiWriter(writers...)
	var written, flushed int64
	for {
		if err := wait(); err != nil {
			return digests{}, err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return digests{}, err
			}
			onBytes(int64(n))
			written += int64(n)
			if written-flushed >= writebackEvery {
				// Start writing this range back now, so the final fsync waits for
				// little more than the tail. It only starts I/O: durability still
				// comes from the Sync below, and errors are left for it to report.
				_ = syncFileRange(int(out.Fd()), flushed, written-flushed, unix.SYNC_FILE_RANGE_WRITE)
				flushed = written
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return digests{}, rerr
		}
	}
	// An import of many GB should not evict the desktop's working set.
	_ = unix.Fadvise(int(in.Fd()), 0, 0, unix.FADV_DONTNEED)
	if err := out.Sync(); err != nil {
		return digests{}, err
	}
	// The pages are clean after the sync, so they can be dropped right away.
	_ = unix.Fadvise(int(out.Fd()), 0, 0, unix.FADV_DONTNEED)
	if err := out.Close(); err != nil {
		return digests{}, err
	}
	if err := os.Chtimes(tmp, atime, mtime); err != nil {
		return digests{}, err
	}
	if err := beforeRename(); err != nil {
		return digests{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return digests{}, err
	}
	ok = true
	d := digests{sha: knownSum}
	if h != nil {
		d.sha = h.Sum(nil)
	}
	switch {
	case sh != nil:
		d.sample = sh.sum()
	case written == info.Size(): // as sh.sum, nil if the file changed while read
		d.sample = d.sha
	}
	return d, nil
}
