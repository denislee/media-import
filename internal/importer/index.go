package importer

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The destination index remembers what was imported, so a repeat import can
// recognise old files without parsing their dates or reading their content.
// It lives in <dest>/.media-import/index and, like the journal, is
// append-only and tab-separated; records are fsynced in batches (it is only
// a hint, so losing the last ones in a crash costs a re-check, nothing more):
//
//	F <src name> <size> <src mtime ns> <dest rel path> <dest mtime ns> <sha256 hex or -> [<sample sha256 hex>]
//	X <src name> <size> <src mtime ns> <dest rel path>
//
// The optional last field of an F record is the SHA-256 of the blocks
// sampledSame reads (see sampleRanges); lines without it stay readable by
// older versions. An X record means the entry for this source key and
// destination path is gone (its destination was deleted).
//
// Names and paths are quoted with strconv.Quote. Later records for the same
// (source key, dest path) replace earlier ones. The index is only a hint:
// every hit is confirmed against a fresh stat of the destination.

const (
	IndexDir  = ".media-import"
	indexFile = "index"
)

type idxKey struct {
	name  string
	size  int64
	mtime int64
}

type idxEntry struct {
	key    idxKey
	rel    string
	dmtime int64
	sha    []byte
	sample []byte
}

type index struct {
	mu     sync.Mutex
	dest   string
	f      *groupFile // nil when read-only
	byKey  map[idxKey][]*idxEntry
	bySize map[int64][]*idxEntry
}

// IndexPath returns the location of the destination index for dest.
func IndexPath(dest string) string { return filepath.Join(dest, IndexDir, indexFile) }

// openIndex loads dest's index. With readOnly the file is never created or
// written (dry runs). A missing index is empty; an unreadable one is moved
// aside and rebuilt from the copies this and later runs make.
func openIndex(dest string, readOnly bool) (*index, error) {
	ix := &index{dest: dest, byKey: map[idxKey][]*idxEntry{}, bySize: map[int64][]*idxEntry{}}
	p := IndexPath(dest)
	lines, err := ix.load(p)
	if err != nil && !os.IsNotExist(err) {
		ix.byKey, ix.bySize = map[idxKey][]*idxEntry{}, map[int64][]*idxEntry{}
		if !readOnly {
			_ = os.Rename(p, fmt.Sprintf("%s.corrupt-%d", p, time.Now().Unix()))
		}
	}
	if readOnly {
		return ix, nil
	}
	if err == nil && lines > 2*ix.liveCount() {
		_ = ix.compact(p) // the old file stays valid if this fails
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return ix, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return ix, err
	}
	ix.f = newGroupFile(f)
	return ix, nil
}

func (ix *index) load(p string) (int, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	lines := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		lines++
		if strings.HasPrefix(line, "X\t") {
			if k, rel, ok := parseRemovalLine(line); ok {
				ix.drop(k, rel)
			}
			continue
		}
		if e, ok := parseIndexLine(line); ok {
			ix.insert(e)
		}
	}
	return lines, sc.Err()
}

// validRel reports whether rel is a usable index path: relative and inside
// the destination. "..foo.jpg" is a legitimate name, "../x" is not.
func validRel(rel string) bool {
	return rel != "" && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, "../")
}

// parseRemovalLine parses an X record; malformed lines are rejected.
func parseRemovalLine(line string) (idxKey, string, bool) {
	f := strings.Split(line, "\t")
	if len(f) != 5 || f[0] != "X" {
		return idxKey{}, "", false
	}
	name, err1 := strconv.Unquote(f[1])
	size, err2 := strconv.ParseInt(f[2], 10, 64)
	smt, err3 := strconv.ParseInt(f[3], 10, 64)
	rel, err4 := strconv.Unquote(f[4])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || !validRel(rel) {
		return idxKey{}, "", false
	}
	return idxKey{name, size, smt}, rel, true
}

// removalLine formats the X record that forgets k's entry at rel.
func removalLine(k idxKey, rel string) string {
	return strings.Join([]string{"X", strconv.Quote(k.name), strconv.FormatInt(k.size, 10),
		strconv.FormatInt(k.mtime, 10), strconv.Quote(rel)}, "\t")
}

// parseIndexLine parses one record; malformed or torn lines are rejected.
func parseIndexLine(line string) (*idxEntry, bool) {
	f := strings.Split(line, "\t")
	if (len(f) != 7 && len(f) != 8) || f[0] != "F" {
		return nil, false
	}
	name, err1 := strconv.Unquote(f[1])
	size, err2 := strconv.ParseInt(f[2], 10, 64)
	smt, err3 := strconv.ParseInt(f[3], 10, 64)
	rel, err4 := strconv.Unquote(f[4])
	dmt, err5 := strconv.ParseInt(f[5], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || !validRel(rel) {
		return nil, false
	}
	e := &idxEntry{key: idxKey{name, size, smt}, rel: rel, dmtime: dmt}
	if f[6] != "-" {
		sha, err := hex.DecodeString(f[6])
		if err != nil || len(sha) != 32 {
			return nil, false
		}
		e.sha = sha
	}
	if len(f) == 8 {
		sample, err := hex.DecodeString(f[7])
		if err != nil || len(sample) != 32 {
			return nil, false
		}
		e.sample = sample
	}
	return e, true
}

func (e *idxEntry) line() string {
	sha := "-"
	if e.sha != nil {
		sha = hex.EncodeToString(e.sha)
	}
	f := []string{"F", strconv.Quote(e.key.name), strconv.FormatInt(e.key.size, 10),
		strconv.FormatInt(e.key.mtime, 10), strconv.Quote(e.rel), strconv.FormatInt(e.dmtime, 10), sha}
	if e.sample != nil {
		f = append(f, hex.EncodeToString(e.sample))
	}
	return strings.Join(f, "\t")
}

// insert adds e, replacing an entry for the same key and destination. It
// reports whether anything changed. The caller holds ix.mu (or owns ix).
func (ix *index) insert(e *idxEntry) bool {
	for _, old := range ix.byKey[e.key] {
		if old.rel != e.rel {
			continue
		}
		if old.dmtime == e.dmtime && (e.sha == nil || string(e.sha) == string(old.sha)) &&
			(e.sample == nil || string(e.sample) == string(old.sample)) {
			return false
		}
		old.dmtime = e.dmtime
		if e.sha != nil {
			old.sha = e.sha
		}
		if e.sample != nil {
			old.sample = e.sample
		}
		return true
	}
	ix.byKey[e.key] = append(ix.byKey[e.key], e)
	ix.bySize[e.key.size] = append(ix.bySize[e.key.size], e)
	return true
}

// drop removes the entry for k at rel and reports whether one was removed.
// The caller holds ix.mu (or owns ix).
func (ix *index) drop(k idxKey, rel string) bool {
	var hit *idxEntry
	es := ix.byKey[k]
	for i, e := range es {
		if e.rel == rel {
			hit = e
			ix.byKey[k] = append(es[:i], es[i+1:]...)
			break
		}
	}
	if hit == nil {
		return false
	}
	if len(ix.byKey[k]) == 0 {
		delete(ix.byKey, k)
	}
	ss := ix.bySize[k.size]
	for i, e := range ss {
		if e == hit {
			ix.bySize[k.size] = append(ss[:i], ss[i+1:]...)
			break
		}
	}
	if len(ix.bySize[k.size]) == 0 {
		delete(ix.bySize, k.size)
	}
	return true
}

// add records that src (name, size, mtime) is stored at dst, whose current
// mtime is dmtime. Either digest may be nil when it is unknown.
func (ix *index) add(k idxKey, dst string, dmtime time.Time, d digests) error {
	if ix == nil {
		return nil
	}
	rel, err := filepath.Rel(ix.dest, dst)
	if err != nil || !validRel(rel) {
		return nil
	}
	e := &idxEntry{key: k, rel: rel, dmtime: dmtime.UnixNano(), sha: d.sha, sample: d.sample}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if !ix.insert(e) || ix.f == nil {
		return nil
	}
	ix.f.append(e.line())
	return nil
}

// remove forgets that k's entry at dst (a path relative to the destination)
// exists, appending an X record when it did.
func (ix *index) remove(k idxKey, rel string) {
	if ix == nil {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.drop(k, rel) && ix.f != nil {
		ix.f.append(removalLine(k, rel))
	}
}

// lookup returns the entries recorded for a source key (copies of them).
func (ix *index) lookup(k idxKey) []idxEntry {
	if ix == nil {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]idxEntry, 0, len(ix.byKey[k]))
	for _, e := range ix.byKey[k] {
		out = append(out, *e)
	}
	return out
}

// withSize returns the entries of the given size that carry a checksum.
func (ix *index) withSize(size int64) []idxEntry {
	if ix == nil {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var out []idxEntry
	for _, e := range ix.bySize[size] {
		if e.sha != nil {
			out = append(out, *e)
		}
	}
	return out
}

// liveCount returns the number of entries the index holds.
func (ix *index) liveCount() int {
	n := 0
	for _, es := range ix.byKey {
		n += len(es)
	}
	return n
}

// compact rewrites the index with one record per live entry. Refresh records
// (new dmtime/sha) and removal records accumulate; rewriting keeps the file
// proportional to what it describes.
func (ix *index) compact(p string) error {
	ents := make([]*idxEntry, 0, ix.liveCount())
	for _, es := range ix.byKey {
		ents = append(ents, es...)
	}
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].rel != ents[j].rel {
			return ents[i].rel < ents[j].rel
		}
		return ents[i].key.name < ents[j].key.name
	})
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range ents {
		if _, err := w.WriteString(e.line() + "\n"); err != nil {
			return fail(err)
		}
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := syncFile(f); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(p)); err == nil {
		_ = d.Sync() // best effort: the rename itself is already durable enough for a hint
		_ = d.Close()
	}
	return nil
}

func (ix *index) close() {
	if ix != nil && ix.f != nil {
		_ = ix.f.close()
	}
}
