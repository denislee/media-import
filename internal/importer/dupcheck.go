package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	sampleSize   = 64 << 10
	mtimeSlack   = 2 * time.Second // FAT32 stores mtimes with 2 s resolution
	bufSize      = 1 << 20
	stageCheck   = "checking if already imported"
	stageQuick   = "checking if already imported (quick)"
	stageOtherNm = "looking for copies under other names"
)

var bufPool = sync.Pool{New: func() any { b := make([]byte, bufSize); return &b }}

func getBuf() *[]byte  { return bufPool.Get().(*[]byte) }
func putBuf(b *[]byte) { bufPool.Put(b) }

// reserve picks a free destination path. If an identical file already exists
// and SkipDups is set it returns that path and how it was recognised.
//
// os.Stat runs without holding r.mu. That is safe because a claimed path
// stays in r.reserved: resvBusy until its transfer finishes, then resvWritten
// (never deleted), so a stat that raced with another worker's rename is
// detected and repeated.
func (r *runner) reserve(ctx context.Context, dir, name string, j job, expect [2]time.Time, sl *slot) (string, dupHow, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	size := j.info.Size()
	var (
		srcSum   []byte
		fullDone bool
	)
	for i := 0; i < 10000; i++ {
		cand := filepath.Join(dir, name)
		if i > 0 {
			cand = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
		}
		r.mu.Lock()
		before := r.reserved[cand]
		r.mu.Unlock()
		if before == resvBusy {
			continue
		}
		st, err := os.Stat(cand)
		if errors.Is(err, fs.ErrNotExist) {
			r.mu.Lock()
			now := r.reserved[cand]
			switch {
			case now == resvBusy:
				r.mu.Unlock()
				continue
			case now == resvWritten && before != resvWritten:
				// Written by another worker after our stat: look again.
				r.mu.Unlock()
				i--
				continue
			}
			r.reserved[cand] = resvBusy
			r.mu.Unlock()
			return cand, notDup, nil
		}
		if err != nil {
			return "", notDup, err
		}
		if !r.opt.SkipDups || st.Size() != size || !st.Mode().IsRegular() {
			continue
		}
		how, err := r.checkCandidate(ctx, j, cand, st, expect, sl, &srcSum, &fullDone)
		if err != nil {
			return "", notDup, err
		}
		if how != notDup {
			return cand, how, nil
		}
		sl.set("preparing", "")
	}
	return "", notDup, errors.New("too many name collisions")
}

// release ends a reservation. A path that was written stays known to the
// run so concurrent reservations re-check it (see reserve).
func (r *runner) release(p string, written bool) {
	r.mu.Lock()
	if written {
		r.reserved[p] = resvWritten
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

// sampledSame compares the first, middle and last sampleSize bytes of a and
// b (both size bytes long). It returns the number of bytes read.
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
	ba, bb := make([]byte, sampleSize), make([]byte, sampleSize)
	var read int64
	for _, off := range [...]int64{0, size/2 - sampleSize/2, size - sampleSize} {
		na, err := fa.ReadAt(ba, off)
		read += int64(na)
		if err != nil && !(errors.Is(err, io.EOF) && na == sampleSize) {
			return false, read, err
		}
		nb, err := fb.ReadAt(bb, off)
		read += int64(nb)
		if err != nil && !(errors.Is(err, io.EOF) && nb == sampleSize) {
			return false, read, err
		}
		if !bytes.Equal(ba, bb) {
			return false, read, nil
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
		r.bytes.Add(-sl.counted.Swap(0))
		sl.bytes.Store(0)
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
		s, err := hashFile(j.src, wait, func(n int64) { r.dupBytes.Add(n) })
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
	})
	dropCache(existing)
	same := err == nil && bytes.Equal(d, *srcSum)
	if !same {
		r.bytes.Add(-sl.counted.Swap(0))
		sl.bytes.Store(0)
	}
	return same, err
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

// hashFile returns the SHA-256 of p, calling wait before each chunk.
func hashFile(p string, wait func() error, onBytes func(int64)) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
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

func fileHash(p string) ([]byte, error) {
	return hashFile(p, func() error { return nil }, func(int64) {})
}

func dropCache(p string) {
	if f, err := os.Open(p); err == nil {
		_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
		f.Close()
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
	pb := getBuf()
	defer putBuf(pb)
	buf := *pb
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
