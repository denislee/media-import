package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countLines reports the number of non-empty lines in p.
func countLines(t *testing.T, p string) int {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			n++
		}
	}
	return n
}

// writeStaleIndex writes an index holding 20 refresh records for one entry
// plus one record for another entry, and returns its path.
func writeStaleIndex(t *testing.T, dest string) string {
	t.Helper()
	lines := make([]string, 0, 21)
	for i := range 20 {
		e := idxEntry{key: idxKey{"A.JPG", 3, 1}, rel: "a/A.jpg", dmtime: int64(i)}
		lines = append(lines, e.line())
	}
	other := idxEntry{key: idxKey{"B.JPG", 3, 1}, rel: "b/B.jpg", dmtime: 1}
	lines = append(lines, other.line())
	p := indexPath(dest)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIndexCompactsRefreshes(t *testing.T) {
	dest := t.TempDir()
	p := writeStaleIndex(t, dest)
	ix, err := openIndex(dest, false)
	if err != nil {
		t.Fatal(err)
	}
	ix.close()
	if n := countLines(t, p); n != 2 {
		t.Fatalf("index has %d lines after compaction, want 2", n)
	}
	ro, err := openIndex(dest, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.close()
	got := ro.lookup(idxKey{"A.JPG", 3, 1})
	if len(got) != 1 || got[0].dmtime != 19 {
		t.Fatalf("lookup = %+v, want one entry with dmtime 19", got)
	}
}

func TestIndexReadOnlyDoesNotCompact(t *testing.T) {
	dest := t.TempDir()
	p := writeStaleIndex(t, dest)
	ix, err := openIndex(dest, true)
	if err != nil {
		t.Fatal(err)
	}
	ix.close()
	if n := countLines(t, p); n != 21 {
		t.Fatalf("read-only open left %d lines, want 21", n)
	}
}

func TestIndexForgetsDeletedDest(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	run(t, o)
	for _, rel := range []string{"2021/06/07/A.jpg", "2021/06/07/A_1.jpg"} {
		if err := os.Remove(filepath.Join(dst, rel)); err != nil {
			t.Fatal(err)
		}
	}
	run(t, o) // copies both again, since their destinations are gone

	data, err := os.ReadFile(indexPath(dst))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "X\t") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("index has no removal record")
	}

	ro, err := openIndex(dst, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := ro.liveCount(); n != 3 {
		t.Fatalf("live entries = %d, want 3", n)
	}
	ro.close()

	run(t, o)
	if n := countLines(t, indexPath(dst)); n > 6 {
		t.Fatalf("index has %d lines after compaction, want at most 6", n)
	}
}

// An index path is usable when it is relative and inside the destination:
// "..foo.jpg" is a legitimate name, "../x" escapes.
func TestValidRel(t *testing.T) {
	for _, c := range []struct {
		rel  string
		want bool
	}{
		{"..foo.jpg", true},
		{"a/..b", true},
		{"a/b.jpg", true},
		{"..", false},
		{"../x", false},
		{"/abs", false},
		{"", false},
	} {
		if got := validRel(c.rel); got != c.want {
			t.Errorf("validRel(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}
