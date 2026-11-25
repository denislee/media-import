package importer

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIndexLoadDoesNotAliasLines checks that nothing an entry keeps points into
// the line it was parsed from. load parses lines in place in the scanner's
// buffer, which later lines overwrite (3000 lines are several buffers' worth);
// an entry that kept a substring of its line would come out corrupted here.
func TestIndexLoadDoesNotAliasLines(t *testing.T) {
	type want struct {
		key    idxKey
		rel    string
		dmtime int64
		sha    []byte
		sample []byte
	}

	dest := t.TempDir()
	p := indexPath(dest)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}

	var lines []string
	wants := make([]want, 0, 3000)
	for i := range 3000 {
		s := sha256.Sum256([]byte(fmt.Sprint("sha", i)))
		t2 := sha256.Sum256([]byte(fmt.Sprint("sample", i)))
		e := &idxEntry{
			key:    idxKey{name: fmt.Sprintf("IMG_%05d.JPG", i), size: int64(1000 + i), mtime: int64(i) * 1e9},
			rel:    fmt.Sprintf("2024/%02d/IMG_%05d.jpg", i%12+1, i),
			dmtime: int64(i)*1e9 + 7,
			sha:    append([]byte(nil), s[:]...),
			sample: append([]byte(nil), t2[:]...),
		}
		wants = append(wants, want{e.key, e.rel, e.dmtime, e.sha, e.sample})
		lines = append(lines, e.line())
		if i%10 == 0 {
			// A removal of a path that does not exist: it drops nothing.
			lines = append(lines, removalLine(e.key, "gone/"+e.rel))
		}
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix, err := openIndex(dest, true)
	if err != nil {
		t.Fatalf("openIndex: %v", err)
	}
	defer ix.close()

	for i, w := range wants {
		es := ix.lookup(w.key)
		if len(es) != 1 {
			t.Fatalf("entry %d (%q): lookup returned %d entries, want 1", i, w.key.name, len(es))
		}
		e := es[0]
		if e.key != w.key {
			t.Fatalf("entry %d: got key %+v, want %+v", i, e.key, w.key)
		}
		if e.rel != w.rel {
			t.Fatalf("entry %d: got rel %q, want %q", i, e.rel, w.rel)
		}
		if e.dmtime != w.dmtime {
			t.Fatalf("entry %d: got dmtime %d, want %d", i, e.dmtime, w.dmtime)
		}
		if !bytes.Equal(e.sha, w.sha) {
			t.Fatalf("entry %d: got sha %x, want %x", i, e.sha, w.sha)
		}
		if !bytes.Equal(e.sample, w.sample) {
			t.Fatalf("entry %d: got sample %x, want %x", i, e.sample, w.sample)
		}
	}
}
