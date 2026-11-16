package importer

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The destination index remembers what was imported, so a repeat import can
// recognise old files without parsing their dates or reading their content.
// It lives in <dest>/.media-import/index and, like the journal, is
// append-only, tab-separated and synced after every record:
//
//	F <src name> <size> <src mtime ns> <dest rel path> <dest mtime ns> <sha256 hex or ->
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
}

type index struct {
	mu     sync.Mutex
	dest   string
	f      *os.File // nil when read-only
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
	if err := ix.load(p); err != nil && !os.IsNotExist(err) {
		ix.byKey, ix.bySize = map[idxKey][]*idxEntry{}, map[int64][]*idxEntry{}
		if !readOnly {
			_ = os.Rename(p, fmt.Sprintf("%s.corrupt-%d", p, time.Now().Unix()))
		}
	}
	if readOnly {
		return ix, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return ix, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return ix, err
	}
	ix.f = f
	return ix, nil
}

func (ix *index) load(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if e, ok := parseIndexLine(sc.Text()); ok {
			ix.insert(e)
		}
	}
	return sc.Err()
}

// parseIndexLine parses one record; malformed or torn lines are rejected.
func parseIndexLine(line string) (*idxEntry, bool) {
	f := strings.Split(line, "\t")
	if len(f) != 7 || f[0] != "F" {
		return nil, false
	}
	name, err1 := strconv.Unquote(f[1])
	size, err2 := strconv.ParseInt(f[2], 10, 64)
	smt, err3 := strconv.ParseInt(f[3], 10, 64)
	rel, err4 := strconv.Unquote(f[4])
	dmt, err5 := strconv.ParseInt(f[5], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || rel == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
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
	return e, true
}

func (e *idxEntry) line() string {
	sha := "-"
	if e.sha != nil {
		sha = hex.EncodeToString(e.sha)
	}
	return strings.Join([]string{"F", strconv.Quote(e.key.name), strconv.FormatInt(e.key.size, 10),
		strconv.FormatInt(e.key.mtime, 10), strconv.Quote(e.rel), strconv.FormatInt(e.dmtime, 10), sha}, "\t")
}

// insert adds e, replacing an entry for the same key and destination. It
// reports whether anything changed. The caller holds ix.mu (or owns ix).
func (ix *index) insert(e *idxEntry) bool {
	for _, old := range ix.byKey[e.key] {
		if old.rel != e.rel {
			continue
		}
		if old.dmtime == e.dmtime && (e.sha == nil || string(e.sha) == string(old.sha)) {
			return false
		}
		old.dmtime = e.dmtime
		if e.sha != nil {
			old.sha = e.sha
		}
		return true
	}
	ix.byKey[e.key] = append(ix.byKey[e.key], e)
	ix.bySize[e.key.size] = append(ix.bySize[e.key.size], e)
	return true
}

// add records that src (name, size, mtime) is stored at dst, whose current
// mtime is dmtime. sha may be nil when the checksum is unknown.
func (ix *index) add(k idxKey, dst string, dmtime time.Time, sha []byte) error {
	if ix == nil {
		return nil
	}
	rel, err := filepath.Rel(ix.dest, dst)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	e := &idxEntry{key: k, rel: rel, dmtime: dmtime.UnixNano(), sha: sha}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if !ix.insert(e) || ix.f == nil {
		return nil
	}
	if _, err := fmt.Fprintln(ix.f, e.line()); err != nil {
		return err
	}
	return ix.f.Sync()
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

func (ix *index) close() {
	if ix != nil && ix.f != nil {
		ix.f.Close()
	}
}
