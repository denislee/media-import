package importer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSampleHasherMatchesSampleDigest checks that hashing a stream as it is
// written gives the same digest as reading the sampled blocks from disk.
func TestSampleHasherMatchesSampleDigest(t *testing.T) {
	for _, size := range []int64{0, 1, 1000, 3 * sampleSize, 3*sampleSize + 1, 1<<20 + 17} {
		p := filepath.Join(t.TempDir(), "f")
		data := blob(int(size), 3)
		writeBytes(t, p, data, t1)

		h := newSampleHasher(size)
		for off := 0; off < len(data); off += 7777 {
			end := min(off+7777, len(data))
			if _, err := h.Write(data[off:end]); err != nil {
				t.Fatalf("size %d: write: %v", size, err)
			}
		}
		got := h.sum()

		want, read, err := sampleDigest(p, size)
		if err != nil {
			t.Fatalf("size %d: sampleDigest: %v", size, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("size %d: digest %x, want %x", size, got, want)
		}
		if wantRead := min(size, 3*sampleSize); read != wantRead {
			t.Fatalf("size %d: read %d bytes, want %d", size, read, wantRead)
		}
	}
}

// TestSampleHasherShortStream checks that a stream shorter than the declared
// size yields no digest.
func TestSampleHasherShortStream(t *testing.T) {
	h := newSampleHasher(100)
	if _, err := h.Write(make([]byte, 99)); err != nil {
		t.Fatal(err)
	}
	if got := h.sum(); got != nil {
		t.Fatalf("short stream returned %x", got)
	}
}

// TestIndexLineSampleField checks that the optional sample field round-trips
// and that malformed sample fields are rejected.
func TestIndexLineSampleField(t *testing.T) {
	e := &idxEntry{
		key:    idxKey{"A.JPG", 10, 5},
		rel:    "2021/A.jpg",
		dmtime: 7,
		sha:    bytes.Repeat([]byte{1}, 32),
	}
	l7 := e.line()
	if n := strings.Count(l7, "\t"); n != 6 {
		t.Fatalf("%d fields: %q", n+1, l7)
	}
	parsed, ok := parseIndexLine(l7)
	if !ok {
		t.Fatalf("rejected %q", l7)
	}
	if parsed.sample != nil {
		t.Fatalf("sample %x, want nil", parsed.sample)
	}

	e.sample = bytes.Repeat([]byte{2}, 32)
	l8 := e.line()
	if n := strings.Count(l8, "\t"); n != 7 {
		t.Fatalf("%d fields: %q", n+1, l8)
	}
	parsed, ok = parseIndexLine(l8)
	if !ok {
		t.Fatalf("rejected %q", l8)
	}
	if !bytes.Equal(parsed.sample, e.sample) {
		t.Fatalf("sample %x, want %x", parsed.sample, e.sample)
	}

	if _, ok := parseIndexLine(l7 + "\tzz"); ok {
		t.Fatal("accepted a non-hex sample")
	}
	if _, ok := parseIndexLine(l7 + "\t" + strings.Repeat("ab", 31)); ok {
		t.Fatal("accepted a short sample")
	}
}

// TestDupAnyNameSamplesFirst checks that a duplicate under another name is
// decided by the sampled blocks, not by hashing whole files.
func TestDupAnyNameSamplesFirst(t *testing.T) {
	root := t.TempDir()
	card1, card2, dst := filepath.Join(root, "card1"), filepath.Join(root, "card2"), filepath.Join(root, "out")
	const n = 50
	const size = 1 << 20
	for i := range n {
		writeBytes(t, filepath.Join(card1, fmt.Sprintf("IMG_%02d.jpg", i)), blob(size, byte(i)), t1)
		writeBytes(t, filepath.Join(card2, fmt.Sprintf("OTHER_%02d.jpg", i)), blob(size, byte(i+100)), t1)
	}

	o := baseOpts(card1, dst)
	o.Verify = false
	o.DupAnyName = true
	if s := run(t, o); s.Copied != n {
		t.Fatalf("%+v", s)
	}

	o2 := baseOpts(card2, dst)
	o2.Verify = false
	o2.DupAnyName = true
	s := run(t, o2)
	if s.Copied != n || s.DupOtherName != 0 {
		t.Fatalf("%+v", s)
	}
	if s.DupCheckBytes > n*3*sampleSize {
		t.Fatalf("dup check read %d bytes, want at most %d", s.DupCheckBytes, n*3*sampleSize)
	}
}

// TestDupAnyNameOldIndexHashesFully is the negative control for
// TestDupAnyNameSamplesFirst: an index without sample fields has to fall back
// to hashing the whole file.
func TestDupAnyNameOldIndexHashesFully(t *testing.T) {
	root := t.TempDir()
	card1, card2, dst := filepath.Join(root, "card1"), filepath.Join(root, "card2"), filepath.Join(root, "out")
	const n = 50
	const size = 1 << 20
	for i := range n {
		writeBytes(t, filepath.Join(card1, fmt.Sprintf("IMG_%02d.jpg", i)), blob(size, byte(i)), t1)
		writeBytes(t, filepath.Join(card2, fmt.Sprintf("OTHER_%02d.jpg", i)), blob(size, byte(i+100)), t1)
	}

	o := baseOpts(card1, dst)
	o.Verify = false
	o.DupAnyName = true
	if s := run(t, o); s.Copied != n {
		t.Fatalf("%+v", s)
	}

	// Strip the sample field, leaving records older versions would write.
	p := indexPath(dst)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 8 {
			f = f[:7]
		}
		b.WriteString(strings.Join(f, "\t"))
		b.WriteString("\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	o2 := baseOpts(card2, dst)
	o2.Verify = false
	o2.DupAnyName = true
	s := run(t, o2)
	if s.Copied != n {
		t.Fatalf("%+v", s)
	}
	if s.DupCheckBytes < int64(n)*size {
		t.Fatalf("dup check read %d bytes, want at least %d", s.DupCheckBytes, int64(n)*size)
	}
}

// TestDupAnyNameStillFindsCopies checks that hiding the index's source keys
// does not stop same-name duplicates from being skipped.
func TestDupAnyNameStillFindsCopies(t *testing.T) {
	root := t.TempDir()
	card1, dst := filepath.Join(root, "card1"), filepath.Join(root, "out")
	const n = 50
	const size = 1 << 20
	for i := range n {
		writeBytes(t, filepath.Join(card1, fmt.Sprintf("IMG_%02d.jpg", i)), blob(size, byte(i)), t1)
	}

	o := baseOpts(card1, dst)
	o.Verify = false
	o.DupAnyName = true
	if s := run(t, o); s.Copied != n {
		t.Fatalf("%+v", s)
	}

	// Hide the index's source keys so only the checksum lookup can match.
	p := indexPath(dst)
	idx, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(idx), "F\t\"", "F\t\"x")), 0o644); err != nil {
		t.Fatal(err)
	}

	o3 := baseOpts(card1, dst)
	o3.DirTemplate = "{YYYY}"
	o3.DupAnyName = true
	s := run(t, o3)
	if s.Skipped != n || s.DupOtherName != n {
		t.Fatalf("%+v", s)
	}
}
